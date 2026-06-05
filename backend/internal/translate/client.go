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
// (留空表示自动识别,不附加)。可选附带上下文。
func buildSystemPrompt(history []Pair, target, source string) string {
	target = resolveTarget(target)
	var sb strings.Builder
	sb.WriteString("你是专业的实时同声传译引擎。请把【待翻译原文】翻译成")
	sb.WriteString(target)
	sb.WriteString("。要求:\n")
	sb.WriteString("1. 只输出译文本身,不要输出原文、引号、解释或任何多余内容;\n")
	sb.WriteString("2. 译文要自然流畅、符合")
	sb.WriteString(target)
	sb.WriteString("的口语表达习惯;\n")
	sb.WriteString("3. 即使原文是一句话的片段也请给出通顺的部分译文,不要补全臆测的内容。\n")
	if s := strings.TrimSpace(source); s != "" {
		sb.WriteString("4. 原文语言为")
		sb.WriteString(s)
		sb.WriteString(",请据此正确理解原文。\n")
	}
	writeDomainAndGlossary(&sb)
	if len(history) > 0 {
		sb.WriteString("\n以下是最近的上下文(原文 => 译文),仅供你保持术语、人名与语气一致,切勿翻译或复述它们:\n")
		for _, p := range history {
			sb.WriteString("- ")
			sb.WriteString(p.Source)
			sb.WriteString(" => ")
			sb.WriteString(p.Target)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// Translate 把 source 翻译成 target(为空回退默认);history 为可选上下文,
// sourceLang 为可选源语言提示(留空表示自动识别)。
func (c *Client) Translate(ctx context.Context, source string, history []Pair, target, sourceLang string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil
	}

	reqBody := chatRequest{
		Model: config.ArkModel,
		Messages: []chatMessage{
			{Role: "system", Content: buildSystemPrompt(history, target, sourceLang)},
			{Role: "user", Content: "【待翻译原文】\n" + source},
		},
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

// Review 对最近若干句做整体复审纠错:模型可借后文澄清前文的歧义/人名/术语,
// 返回与输入等长、按顺序排列的「修订后译文」切片(无需修改的句子原样返回)。
// target 为目标语言(为空回退默认)。
func (c *Client) Review(ctx context.Context, items []ReviewItem, target string) ([]string, error) {
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
	arr, err := parseStringArray(content)
	if err != nil {
		return nil, err
	}
	if len(arr) != len(items) {
		return nil, fmt.Errorf("review length mismatch: got %d want %d", len(arr), len(items))
	}
	return arr, nil
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

// parseStringArray 从模型输出里抽取一个 JSON 字符串数组(容忍 ```json 代码块包裹)。
func parseStringArray(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("review: 输出中未找到 JSON 数组 (raw=%s)", s)
	}
	var arr []string
	if err := json.Unmarshal([]byte(s[start:end+1]), &arr); err != nil {
		return nil, fmt.Errorf("review: 解析 JSON 数组失败: %w (raw=%s)", err, s)
	}
	return arr, nil
}

// buildReviewSystemPrompt 组装复审纠错的系统提示词,目标语言为 target(为空回退默认)。
func buildReviewSystemPrompt(n int, target string) string {
	target = resolveTarget(target)
	var sb strings.Builder
	sb.WriteString("你是实时同声传译的质检与纠错引擎。用户会给你一个 JSON 数组,按时间先后顺序排列最近几句的 {index, source(原文), target(当前")
	sb.WriteString(target)
	sb.WriteString("译文)}。\n")
	sb.WriteString("请结合【完整上下文】(尤其是后文往往能澄清前文的歧义、人名、术语)逐句校正译文,使整体更准确、连贯、术语一致。要求:\n")
	sb.WriteString("1. 仅在确有改进时才修改;没有问题的句子原样返回其译文;\n")
	sb.WriteString("2. 译文须自然流畅、符合")
	sb.WriteString(target)
	sb.WriteString("口语习惯,且只对应该句原文,不要合并、拆分或臆测补全;\n")
	sb.WriteString(fmt.Sprintf("3. 严格只输出一个 JSON 字符串数组,长度必须为 %d,按 index 顺序给出每句修订后的译文,不要输出任何额外文字、键名、序号或注释。", n))
	writeDomainAndGlossary(&sb)
	return sb.String()
}
