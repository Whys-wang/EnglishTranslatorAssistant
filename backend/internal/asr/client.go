package asr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"simul-interpreter/internal/config"
	"simul-interpreter/internal/translate"
)

// Utterance 是一句识别结果。
type Utterance struct {
	Text      string `json:"text"`
	StartTime int    `json:"start_time"`
	EndTime   int    `json:"end_time"`
	Definite  bool   `json:"definite"` // true = 定稿(final);false = 中间结果(partial)
}

// Result 是识别结果负载中的 result 字段。
type Result struct {
	Text       string      `json:"text"`
	Utterances []Utterance `json:"utterances"`
}

// serverPayload 是 full server response 的 JSON 负载。
type serverPayload struct {
	Result Result `json:"result"`
}

// Event 是 ASR 客户端向上层抛出的识别事件。
type Event struct {
	Text       string      // 全量文本
	Utterances []Utterance // 分句(含 definite 标志)
	IsLast     bool        // 是否最后一包
}

// Handlers 是上层注入的回调。
type Handlers struct {
	OnEvent func(Event)
	OnError func(error)
	OnLogid func(logid string) // websocket 握手成功后回调,记录 X-Tt-Logid
}

// Client 是单个会话对应的火山 ASR 上游连接。
type Client struct {
	sessionID string
	log       *slog.Logger
	h         Handlers

	mu          sync.Mutex      // 保护 conn 写入与重连
	conn        *websocket.Conn // 当前上游连接(可能在重连期间为 nil)
	srcLang     string          // 用户源语言(规范名,空=自动检测)
	language    string          // audio.language,空=默认中英文模型
	useNostream bool            // 是否走 bigmodel_nostream(小语种)
	connectGen  uint64          // 语言切换时递增,丢弃过期的连接建立

	closed bool
}

// NewClient 创建一个 ASR 客户端。
func NewClient(sessionID string, log *slog.Logger, h Handlers) *Client {
	return &Client{sessionID: sessionID, log: log, h: h}
}

// SetSourceLanguage 按源语言选择 ASR 端点与 audio.language。
// 英语/中文/自动检测走 bigmodel_async(英→中同款);其余显式源语言走 nostream + 语种代码。
func (c *Client) SetSourceLanguage(srcLang string) {
	src := translate.NormalizeLang(srcLang)
	useNostream := translate.ASRNeedsNostream(src)
	langCode := ""
	if useNostream {
		langCode = translate.ASRLanguageCode(src)
	}

	c.mu.Lock()
	if c.language == langCode && c.useNostream == useNostream {
		c.srcLang = src
		c.mu.Unlock()
		return
	}
	c.srcLang = src
	c.language = langCode
	c.useNostream = useNostream
	c.connectGen++
	conn := c.conn
	c.conn = nil
	gen := c.connectGen
	c.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	c.log.Info("asr language configured",
		slog.String("session", c.sessionID),
		slog.String("source", src),
		slog.String("language", langCode),
		slog.Bool("nostream", useNostream),
		slog.Uint64("connect_gen", gen),
	)
}

func (c *Client) endpointLocked() string {
	if c.useNostream {
		return config.ASRWebSocketURLNostream
	}
	if config.ASRWebSocketURLAsync != "" {
		return config.ASRWebSocketURLAsync
	}
	return config.ASRWebSocketURL
}

// buildConfigJSON 组装 full client request 的 JSON 配置。
func (c *Client) buildConfigJSON() ([]byte, error) {
	req := map[string]any{
		"model_name":      config.ASRModelName,
		"enable_itn":      config.ASRRequest.EnableITN,
		"enable_punc":     config.ASRRequest.EnablePunc,
		"enable_ddc":      config.ASRRequest.EnableDDC,
		"show_utterances": config.ASRRequest.ShowUtterances,
		"result_type":     config.ASRRequest.ResultType,
	}
	c.mu.Lock()
	useNostream := c.useNostream
	c.mu.Unlock()
	if useNostream {
		if sz := translate.ASREndWindowSize(c.srcLang); sz > 0 {
			req["end_window_size"] = sz
		}
	} else {
		if config.ASRRequest.EndWindowSize > 0 {
			req["end_window_size"] = config.ASRRequest.EndWindowSize
		}
		if config.ASRRequest.EnableNonstream {
			req["enable_nonstream"] = true
		}
	}
	audio := map[string]any{
		"format":  "pcm",
		"codec":   "raw",
		"rate":    config.AudioSampleRate,
		"bits":    config.AudioBitDepth,
		"channel": config.AudioChannels,
	}
	c.mu.Lock()
	lang := c.language
	c.mu.Unlock()
	if lang != "" {
		audio["language"] = lang
	}
	payload := map[string]any{
		"user":    map[string]any{"uid": c.sessionID},
		"audio":   audio,
		"request": req,
	}
	return json.Marshal(payload)
}

// dialAndInit 建立上游连接并发送首包配置。
func (c *Client) dialAndInit(ctx context.Context) error {
	c.mu.Lock()
	gen := c.connectGen
	endpoint := c.endpointLocked()
	c.mu.Unlock()

	header := http.Header{}
	if config.UseNewConsoleAuth {
		header.Set("X-Api-Key", config.SpeechAPIKey)
	} else {
		header.Set("X-Api-App-Key", config.ASRAppKey)
		header.Set("X-Api-Access-Key", config.ASRAccessKey)
	}
	header.Set("X-Api-Resource-Id", config.ASRResourceID)
	header.Set("X-Api-Request-Id", uuid.NewString())
	header.Set("X-Api-Connect-Id", uuid.NewString())
	header.Set("X-Api-Sequence", "-1")

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial asr (http %d, logid=%s): %w", resp.StatusCode, resp.Header.Get("X-Tt-Logid"), err)
		}
		return fmt.Errorf("dial asr: %w", err)
	}

	c.mu.Lock()
	if c.connectGen != gen || c.closed {
		c.mu.Unlock()
		_ = conn.Close()
		return fmt.Errorf("asr connect stale (gen=%d)", gen)
	}
	c.mu.Unlock()

	if c.h.OnLogid != nil {
		c.h.OnLogid(resp.Header.Get("X-Tt-Logid"))
	}

	cfgJSON, err := c.buildConfigJSON()
	if err != nil {
		_ = conn.Close()
		return err
	}
	frame, err := buildFullClientRequest(cfgJSON)
	if err != nil {
		_ = conn.Close()
		return err
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		_ = conn.Close()
		return fmt.Errorf("send full client request: %w", err)
	}

	c.mu.Lock()
	if c.connectGen != gen || c.closed {
		c.mu.Unlock()
		_ = conn.Close()
		return fmt.Errorf("asr connect stale after init (gen=%d)", gen)
	}
	c.conn = conn
	c.mu.Unlock()
	return nil
}

// Run 连接上游并持续读取识别结果;断线按指数退避自动重连,直到 ctx 取消或 Close。
func (c *Client) Run(ctx context.Context) {
	backoff := config.Reconnect.InitialBackoff
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if c.isClosed() {
			return
		}

		if err := c.dialAndInit(ctx); err != nil {
			if strings.Contains(err.Error(), "stale") {
				c.log.Debug("asr connect superseded", slog.String("err", err.Error()))
				backoff = config.Reconnect.InitialBackoff
				attempt = 0
				continue
			}
			c.emitErr(fmt.Errorf("asr connect: %w", err))
		} else {
			c.mu.Lock()
			endpoint := c.endpointLocked()
			src := c.srcLang
			lang := c.language
			c.mu.Unlock()
			c.log.Info("asr upstream connected",
				slog.String("session", c.sessionID),
				slog.String("endpoint", endpoint),
				slog.String("source", src),
				slog.String("language", lang),
			)
			backoff = config.Reconnect.InitialBackoff
			attempt = 0
			c.readLoop(ctx)
			c.clearConn()
		}

		if c.isClosed() {
			return
		}
		attempt++
		if config.Reconnect.MaxRetries > 0 && attempt > config.Reconnect.MaxRetries {
			c.emitErr(fmt.Errorf("asr reconnect: 超过最大重试次数 %d", config.Reconnect.MaxRetries))
			return
		}
		c.log.Warn("asr upstream disconnected, will reconnect",
			slog.String("session", c.sessionID),
			slog.Duration("backoff", backoff),
		)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = time.Duration(float64(backoff) * config.Reconnect.Multiplier)
		if backoff > config.Reconnect.MaxBackoff {
			backoff = config.Reconnect.MaxBackoff
		}
	}
}

// readLoop 读取并解析服务端帧,直到出错。
func (c *Client) readLoop(ctx context.Context) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			if !c.isClosed() {
				c.log.Debug("asr read error", slog.String("err", err.Error()))
			}
			return
		}
		frame, err := parseServerFrame(data)
		if err != nil {
			c.emitErr(fmt.Errorf("parse asr frame: %w", err))
			continue
		}
		switch frame.MessageType {
		case msgErrorResp:
			c.emitErr(fmt.Errorf("asr server error code=%d msg=%s", frame.ErrorCode, string(frame.Payload)))
		case msgFullServerResp:
			var p serverPayload
			if len(frame.Payload) > 0 {
				if err := json.Unmarshal(frame.Payload, &p); err != nil {
					c.emitErr(fmt.Errorf("decode asr result: %w (raw=%s)", err, string(frame.Payload)))
					continue
				}
			}
			if c.h.OnEvent != nil {
				c.h.OnEvent(Event{
					Text:       p.Result.Text,
					Utterances: p.Result.Utterances,
					IsLast:     frame.IsLast,
				})
			}
		}
	}
}

// SendAudio 把一段 PCM 发往上游;last 标记最后一包。重连期间会返回错误(上层可丢弃)。
func (c *Client) SendAudio(pcm []byte, last bool) error {
	frame, err := buildAudioRequest(pcm, last)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("asr 上游未连接")
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, frame)
}

// Close 关闭客户端,停止重连。
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()
}

func (c *Client) clearConn() {
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Client) emitErr(err error) {
	if c.h.OnError != nil {
		c.h.OnError(err)
	}
}
