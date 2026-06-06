// Package translate 通过火山方舟(Ark, OpenAI 兼容的 Chat Completions 接口)
// 把 ASR 识别出的外语原文翻译成目标语言(本项目固定为中文)。
//
// 设计要点:
//   - 仅依赖 config 中写死的 ArkAPIKey / ArkModel / ArkEndpoint;
//   - 支持携带最近 N 段「原文 => 译文」作为上下文,保持术语/语气一致;
//   - 提示词约束模型只输出译文本身,便于直接回填 segment。
package translate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"log/slog"

	"simul-interpreter/internal/config"
)

// Pair 是一段「原文 => 译文」上下文。
type Pair struct {
	Source string
	Target string
}

// Client 是方舟翻译客户端(可被同一会话复用)。
type Client struct {
	log  *slog.Logger
	http *http.Client
}

// NewClient 创建一个翻译客户端。
func NewClient(log *slog.Logger) *Client {
	return &Client{
		log:  log,
		http: &http.Client{Timeout: config.Translate.Timeout},
	}
}

// Configured 报告 Ark 是否已配置真实密钥与模型(占位符视为未配置)。
func Configured() bool {
	notPlaceholder := func(s string) bool {
		return s != "" && !strings.HasPrefix(s, "PLEASE_FILL")
	}
	return notPlaceholder(config.ArkAPIKey) && notPlaceholder(config.ArkModel)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type thinkingOption struct {
	Type string `json:"type"` // "disabled" / "enabled" / "auto"
}

type chatRequest struct {
	Model       string          `json:"model"`
	Messages    []chatMessage   `json:"messages"`
	Temperature float64         `json:"temperature"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stream      bool            `json:"stream"`
	Thinking    *thinkingOption `json:"thinking,omitempty"` // 关闭深度思考,显著降低延迟
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// writeDomainAndGlossary 把领域背景与术语表(若已配置)写入系统提示词。
// 术语条目按原文词条字典序输出,保证提示词稳定、便于测试。
func writeDomainAndGlossary(sb *strings.Builder) {
	if d := strings.TrimSpace(config.Prompt.Domain); d != "" {
		sb.WriteString("\n【领域背景】")
		sb.WriteString(d)
		sb.WriteString("\n请据此理解专有名词与语境。\n")
	}
	if len(config.Prompt.Glossary) > 0 {
		keys := make([]string, 0, len(config.Prompt.Glossary))
		for k := range config.Prompt.Glossary {
			if strings.TrimSpace(k) != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			sb.WriteString("\n【术语对照表】以下词条出现时必须采用指定译法(大小写/复数等变体同样适用):\n")
			for _, k := range keys {
				sb.WriteString("- ")
				sb.WriteString(k)
				sb.WriteString(" => ")
				sb.WriteString(config.Prompt.Glossary[k])
				sb.WriteString("\n")
			}
		}
	}
}

// resolveTarget 返回有效目标语言:为空时回退到默认(config.TargetLanguage)。
func resolveTarget(target string) string {
	if t := strings.TrimSpace(target); t != "" {
		return t
	}
	return config.TargetLanguage
}

// buildSystemPrompt 组装系统提示词,翻译目标语言为 target;source 为源语言提示
// (留空表示自动识别,不附加)。上下文以多轮对话(user/assistant)的形式单独喂给模型,
// 不再混入 system prompt——这样模型几乎不会把上一轮译文复述/拼接到当前译文里。
func buildSystemPrompt(target, source string) string {
	target = resolveTarget(target)
	var sb strings.Builder
	sb.WriteString("你是专业的实时同声传译引擎。请把【待翻译原文】翻译成")
	sb.WriteString(target)
	sb.WriteString("。严格要求:\n")
	sb.WriteString("1. 每轮只输出「最后一条 user 消息」中【待翻译原文】对应的译文本身,不要输出原文、引号、解释、序号或任何多余内容;\n")
	sb.WriteString("2. 严禁把前几轮(历史对话)中的原文或译文复述、拼接到当前译文里;前几轮仅用于让你保持术语、人名与语气一致;\n")
	sb.WriteString("3. 译文要自然流畅、符合")
	sb.WriteString(target)
	sb.WriteString("的口语表达习惯;\n")
	sb.WriteString("4. 即使原文是一句话的片段也请给出通顺的部分译文,不要补全臆测的内容。\n")
	if s := strings.TrimSpace(source); s != "" {
		sb.WriteString("5. 原文语言为")
		sb.WriteString(s)
		sb.WriteString(",请据此正确理解原文。\n")
	}
	writeDomainAndGlossary(&sb)
	return sb.String()
}

// Translate 把 source 翻译成 target(为空回退默认);history 为可选上下文,
// sourceLang 为可选源语言提示(留空表示自动识别)。
//
// 上下文不是塞进 system prompt 拼成「原文 => 译文」表,而是作为标准多轮对话发送:
// 每段历史 = 一条 user(原文) + 一条 assistant(译文),当前句作为最后一条 user。
// LLM 训练时见得最多的就是这种格式,生成边界很清晰,显著降低「上一句译文渗漏到本句」。
func (c *Client) Translate(ctx context.Context, source string, history []Pair, target, sourceLang string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil
	}

	messages := make([]chatMessage, 0, 2+2*len(history))
	messages = append(messages, chatMessage{Role: "system", Content: buildSystemPrompt(target, sourceLang)})
	for _, p := range history {
		if strings.TrimSpace(p.Source) == "" || strings.TrimSpace(p.Target) == "" {
			continue
		}
		messages = append(messages,
			chatMessage{Role: "user", Content: "【待翻译原文】\n" + p.Source},
			chatMessage{Role: "assistant", Content: p.Target},
		)
	}
	messages = append(messages, chatMessage{Role: "user", Content: "【待翻译原文】\n" + source})

	reqBody := chatRequest{
		Model:       config.ArkModel,
		Messages:    messages,
		Temperature: config.Translate.Temperature,
		MaxTokens:   config.Translate.MaxTokens,
		Stream:      false,
		// 翻译是确定性任务,关闭推理模型的深度思考以降低延迟(seed 系列支持)。
		Thinking: &thinkingOption{Type: "disabled"},
	}
	return c.doChat(ctx, reqBody)
}

// ReviewItem 是一条待复审的字幕(原文 + 当前译文)。
type ReviewItem struct {
	Source string
	Target string
}

// Revision 是复审对一句字幕给出的结构化结果。
//
// 相比旧版只返回「修订后译文字符串」,这里额外带上:
//   - Changed:模型是否真的改了这句(没改就别触发前端「纠错高亮」,避免无谓闪烁);
//   - Confidence:模型对「这次修改确实更准确」的把握(0~1),
//     供上层按阈值过滤,杜绝把本来正确的句子越改越糟的「过度纠错」;
//   - Reason:简短改动理由(便于日志排查,可为空)。
type Revision struct {
	Index      int     `json:"index"`
	Target     string  `json:"target"`
	Changed    bool    `json:"changed"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// ReviewDetailed 对最近若干句做整体复审纠错:模型借「完整上下文(尤其后文)」
// 校正前文的同音误识别、专有名词、代词指代、术语一致性、数字单位、过早翻译造成的
// 语序错误、一词多义等问题。返回与输入等长、按 index 排序的结构化结果。
func (c *Client) ReviewDetailed(ctx context.Context, items []ReviewItem, target string) ([]Revision, error) {
	if len(items) == 0 {
		return nil, nil
	}

	type inItem struct {
		Index  int    `json:"index"`
		Source string `json:"source"`
		Target string `json:"target"`
	}
	in := make([]inItem, len(items))
	for i, it := range items {
		in[i] = inItem{Index: i, Source: it.Source, Target: it.Target}
	}
	inJSON, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal review input: %w", err)
	}

	reqBody := chatRequest{
		Model: config.ArkModel,
		Messages: []chatMessage{
			{Role: "system", Content: buildReviewSystemPrompt(len(items), target)},
			{Role: "user", Content: string(inJSON)},
		},
		Temperature: config.Translate.Temperature,
		Stream:      false,
		Thinking:    &thinkingOption{Type: "disabled"},
	}
	content, err := c.doChat(ctx, reqBody)
	if err != nil {
		return nil, err
	}
	revs, err := parseRevisions(content)
	if err != nil {
		return nil, err
	}
	if len(revs) != len(items) {
		return nil, fmt.Errorf("review length mismatch: got %d want %d", len(revs), len(items))
	}
	return revs, nil
}

// Review 是 ReviewDetailed 的兼容封装:只返回「修订后译文」切片
// (无需修改的句子原样返回),保持旧调用方/测试可用。
func (c *Client) Review(ctx context.Context, items []ReviewItem, target string) ([]string, error) {
	revs, err := c.ReviewDetailed(ctx, items, target)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(revs))
	for i, r := range revs {
		out[i] = r.Target
	}
	return out, nil
}

// doChat 执行一次非流式 Chat Completions 调用,返回首个 choice 的文本内容。
func (c *Client) doChat(ctx context.Context, reqBody chatRequest) (string, error) {
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal ark request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, config.ArkEndpoint, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("build ark request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+config.ArkAPIKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("ark request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ark http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("decode ark response: %w (raw=%s)", err, string(body))
	}
	if cr.Error != nil {
		return "", fmt.Errorf("ark error code=%v: %s", cr.Error.Code, cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("ark empty choices (raw=%s)", string(body))
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), nil
}

// parseRevisions 从模型输出里抽取一个 JSON 对象数组(容忍 ```json 代码块包裹)。
// 每个对象形如 {index, target, changed, confidence, reason}。
func parseRevisions(s string) ([]Revision, error) {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("review: 输出中未找到 JSON 数组 (raw=%s)", s)
	}
	var revs []Revision
	if err := json.Unmarshal([]byte(s[start:end+1]), &revs); err != nil {
		return nil, fmt.Errorf("review: 解析 JSON 数组失败: %w (raw=%s)", err, s)
	}
	return revs, nil
}

// buildReviewSystemPrompt 组装复审纠错的系统提示词,目标语言为 target(为空回退默认)。
//
// 提示词显式列举「实时同传里最常见、且能借后文修正」的错误类型,让模型有的放矢;
// 并强约束「保守」原则(没问题不要改)+ 结构化输出(带 changed / confidence),
// 供上层按置信度阈值过滤,避免过度纠错反复改写已经正确的句子。
func buildReviewSystemPrompt(n int, target string) string {
	target = resolveTarget(target)
	var sb strings.Builder
	sb.WriteString("你是实时同声传译的质检与纠错引擎。用户会给你一个 JSON 数组,按时间先后顺序排列最近几句的 {index, source(原文), target(当前")
	sb.WriteString(target)
	sb.WriteString("译文)}。\n")
	sb.WriteString("请结合【完整上下文】(尤其后文常能澄清前文)逐句判断译文是否有误并校正。重点排查以下错误类型:\n")
	sb.WriteString("① 同音/谐音误识别:ASR 可能把原文听错(如英文 their/there、中文 期时/其实),若结合上下文明显是另一个词,据此修正译文;\n")
	sb.WriteString("② 专有名词/人名/地名/产品名:后文出现全称或更清晰写法时,回填修正前文里的音译或错译;\n")
	sb.WriteString("③ 代词指代:it/they/这/那/他 等在后文明确所指后,修正为更准确的表达;\n")
	sb.WriteString("④ 术语一致性:同一概念全程使用统一译法(若有术语表必须遵循);\n")
	sb.WriteString("⑤ 数字/单位/时间/金额/日期:核对是否与原文一致;\n")
	sb.WriteString("⑥ 过早翻译导致的语序/结构错误:边说边译时前半句可能被误解,后文补全后修正为通顺表达;\n")
	sb.WriteString("⑦ 一词多义/歧义:后文确定词义后修正(如 Apple 公司 vs 苹果)。\n")
	sb.WriteString("【保守原则】没有问题的句子一律保持原译文不变、changed 置为 false;只在确有改进时才修改。宁可不改,也不要把已经正确、通顺的句子改成同义的另一种说法(那只会让字幕无谓闪烁)。\n")
	sb.WriteString("译文须自然流畅、符合")
	sb.WriteString(target)
	sb.WriteString("口语习惯,且只对应该句原文,不要合并、拆分或臆测补全。\n")
	sb.WriteString(fmt.Sprintf("【输出格式】严格只输出一个 JSON 数组,长度必须为 %d,按 index 升序排列,每个元素是对象:\n", n))
	sb.WriteString("{\"index\": 该句序号, \"target\": \"修订后译文(没改就原样返回原译文)\", \"changed\": true/false(是否做了实质修改), \"confidence\": 0~1 的数字(你对『本次修改确实更准确』的把握;changed=false 时填 1)}\n")
	sb.WriteString("不要输出任何额外文字、Markdown 代码块、解释或注释。")
	writeDomainAndGlossary(&sb)
	return sb.String()
}
