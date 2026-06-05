package asr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"log/slog"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"simul-interpreter/internal/config"
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

	mu   sync.Mutex      // 保护 conn 写入与重连
	conn *websocket.Conn // 当前上游连接(可能在重连期间为 nil)

	closed bool
}

// NewClient 创建一个 ASR 客户端。
func NewClient(sessionID string, log *slog.Logger, h Handlers) *Client {
	return &Client{sessionID: sessionID, log: log, h: h}
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
	if config.ASRRequest.EndWindowSize > 0 {
		req["end_window_size"] = config.ASRRequest.EndWindowSize
	}
	payload := map[string]any{
		"user": map[string]any{"uid": c.sessionID},
		"audio": map[string]any{
			"format":  "pcm",
			"codec":   "raw",
			"rate":    config.AudioSampleRate,
			"bits":    config.AudioBitDepth,
			"channel": config.AudioChannels,
		},
		"request": req,
	}
	return json.Marshal(payload)
}

// dialAndInit 建立上游连接并发送首包配置。
func (c *Client) dialAndInit(ctx context.Context) error {
	header := http.Header{}
	if config.UseNewConsoleAuth {
		// 新版控制台:仅需 X-Api-Key。
		header.Set("X-Api-Key", config.SpeechAPIKey)
	} else {
		// 旧版控制台:App Key + Access Key。
		header.Set("X-Api-App-Key", config.ASRAppKey)
		header.Set("X-Api-Access-Key", config.ASRAccessKey)
	}
	header.Set("X-Api-Resource-Id", config.ASRResourceID)
	header.Set("X-Api-Request-Id", uuid.NewString())
	header.Set("X-Api-Connect-Id", uuid.NewString())
	header.Set("X-Api-Sequence", "-1")

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, config.ASRWebSocketURL, header)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial asr (http %d, logid=%s): %w", resp.StatusCode, resp.Header.Get("X-Tt-Logid"), err)
		}
		return fmt.Errorf("dial asr: %w", err)
	}
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
			c.emitErr(fmt.Errorf("asr connect: %w", err))
		} else {
			c.log.Info("asr upstream connected", slog.String("session", c.sessionID))
			backoff = config.Reconnect.InitialBackoff
			attempt = 0
			c.readLoop(ctx) // 阻塞直到读出错或连接关闭
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
