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

// buildSystemPrompt 组装系统提示词,可选附带上下文。
func buildSystemPrompt(history []Pair) string {
	var sb strings.Builder
	sb.WriteString("你是专业的实时同声传译引擎。请把【待翻译原文】翻译成")
	sb.WriteString(config.TargetLanguage)
	sb.WriteString("。要求:\n")
	sb.WriteString("1. 只输出译文本身,不要输出原文、引号、解释或任何多余内容;\n")
	sb.WriteString("2. 译文要自然流畅、符合")
	sb.WriteString(config.TargetLanguage)
	sb.WriteString("的口语表达习惯;\n")
	sb.WriteString("3. 即使原文是一句话的片段也请给出通顺的部分译文,不要补全臆测的内容。\n")
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

// Translate 把 source 翻译成目标语言;history 为可选上下文。
func (c *Client) Translate(ctx context.Context, source string, history []Pair) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil
	}

	reqBody := chatRequest{
		Model: config.ArkModel,
		Messages: []chatMessage{
			{Role: "system", Content: buildSystemPrompt(history)},
			{Role: "user", Content: "【待翻译原文】\n" + source},
		},
		Temperature: config.Translate.Temperature,
		MaxTokens:   config.Translate.MaxTokens,
		Stream:      false,
		// 翻译是确定性任务,关闭推理模型的深度思考以降低延迟(seed 系列支持)。
		Thinking: &thinkingOption{Type: "disabled"},
	}
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
