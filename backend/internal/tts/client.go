package tts

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
)

// Client 是「双向流式 V3」语音合成客户端。
//
// 延迟优化(里程碑 8):复用同一条 WebSocket 连接——只在首次合成时建连并完成
// 连接级 StartConnection 握手,之后每句仅做会话级 StartSession→…→SessionFinished,
// 省去逐句重新建连 + 握手的往返。连接因空闲被服务端关闭时,下一次合成会自动重连。
// 调用方负责在会话结束时调用 Close 释放连接。
type Client struct {
	log *slog.Logger

	mu   sync.Mutex      // 串行化合成 + 保护 conn
	conn *websocket.Conn // 复用的连接(nil 表示尚未建连或已失效)
}

// NewClient 创建一个 TTS 客户端。
func NewClient(log *slog.Logger) *Client {
	return &Client{log: log}
}

// Configured 报告 TTS 是否可用(总开关开启且音色已填真实值)。
func Configured() bool {
	if !config.TTS.Enable {
		return false
	}
	v := config.TTSVoiceType
	return v != "" && !strings.HasPrefix(v, "PLEASE_FILL")
}

// audioParams 是 req_params.audio_params。
type audioParams struct {
	Format     string `json:"format"`
	SampleRate int    `json:"sample_rate"`
}

// reqParams 是 StartSession / TaskRequest 的 req_params。
type reqParams struct {
	Speaker     string       `json:"speaker,omitempty"`
	AudioParams *audioParams `json:"audio_params,omitempty"`
	Text        string       `json:"text,omitempty"`
}

// sessionPayload 是会话事件的 JSON 负载。
type sessionPayload struct {
	Event     int32     `json:"event"`
	Namespace string    `json:"namespace"`
	ReqParams reqParams `json:"req_params"`
}

// Synthesize 把一段文本合成为音频字节(整段返回),format 见 config.TTS.Format。
func (c *Client) Synthesize(ctx context.Context, text string) ([]byte, string, error) {
	if !Configured() {
		return nil, "", fmt.Errorf("tts 未启用或音色未配置")
	}
	return c.synthesizeWith(ctx, text, config.TTSVoiceType)
}

// SynthesizeWith 用指定音色合成一段文本(供按「目标语言」选音色的场景使用)。
// voice 为空表示该目标语言没有配置音色,直接跳过(返回空音频、无错误)。
func (c *Client) SynthesizeWith(ctx context.Context, text, voice string) ([]byte, string, error) {
	if strings.TrimSpace(voice) == "" {
		return nil, "", nil
	}
	return c.synthesizeWith(ctx, text, voice)
}

// synthesizeWith 用指定音色完成一次合成(供 Synthesize 与联机测试复用,不做 Configured 校验)。
//
// 复用持久连接:首次或重连后做一次连接级 StartConnection;每句仅跑会话级流程。
// 若在「复用的旧连接」上失败(常见于服务端把空闲连接关掉),自动重连后重试一次。
func (c *Client) synthesizeWith(ctx context.Context, text, voice string) ([]byte, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", nil
	}

	ctx, cancel := context.WithTimeout(ctx, config.TTS.Timeout)
	defer cancel()

	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		fresh, err := c.ensureConn(ctx)
		if err != nil {
			c.dropConnLocked()
			lastErr = err
			continue // 重连后再试一次
		}
		audio, format, err := c.runSession(ctx, text, voice)
		if err == nil {
			return audio, format, nil
		}
		lastErr = err
		c.dropConnLocked()
		if fresh {
			break // 全新连接仍失败,属真实错误,不再空转
		}
		c.log.Debug("tts reused conn failed, will redial", slog.String("err", err.Error()))
	}
	return nil, "", lastErr
}

// ensureConn 确保持久连接已建立并完成连接级握手。
// 返回 fresh=true 表示本次新建了连接(用于区分「旧连接失效」与「真实错误」)。
func (c *Client) ensureConn(ctx context.Context) (fresh bool, err error) {
	if c.conn != nil {
		return false, nil
	}

	header := http.Header{}
	if config.UseNewConsoleAuth {
		header.Set("X-Api-Key", config.SpeechAPIKey)
	} else {
		header.Set("X-Api-App-Key", config.TTSAppKey)
		header.Set("X-Api-Access-Key", config.TTSAccessKey)
	}
	header.Set("X-Api-Resource-Id", config.TTSResourceID)
	header.Set("X-Api-Connect-Id", uuid.NewString())

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, config.TTSWebSocketURL, header)
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("dial tts (http %d, logid=%s): %w", resp.StatusCode, resp.Header.Get("X-Tt-Logid"), err)
		}
		return false, fmt.Errorf("dial tts: %w", err)
	}
	if logid := resp.Header.Get("X-Tt-Logid"); logid != "" {
		c.log.Debug("tts handshake", slog.String("x_tt_logid", logid))
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(dl)
		_ = conn.SetReadDeadline(dl)
	}

	// 连接级 StartConnection(整条连接只做一次)。
	if err := conn.WriteMessage(websocket.BinaryMessage, buildClientFrame(EventStartConnection, "", []byte("{}"))); err != nil {
		_ = conn.Close()
		return false, fmt.Errorf("send StartConnection: %w", err)
	}
	if _, err := c.waitEvent(conn, EventConnectionStarted, EventConnectionFailed); err != nil {
		_ = conn.Close()
		return false, fmt.Errorf("等待 ConnectionStarted: %w", err)
	}

	c.conn = conn
	return true, nil
}

// runSession 在已建立的持久连接上完成一句合成(不关闭连接,供后续复用)。
// 流程:StartSession -> TaskRequest(text) -> FinishSession -> 收音频 -> SessionFinished。
func (c *Client) runSession(ctx context.Context, text, voice string) ([]byte, string, error) {
	conn := c.conn
	if conn == nil {
		return nil, "", fmt.Errorf("tts 连接不存在")
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(dl)
		_ = conn.SetReadDeadline(dl)
	}

	sessionID := uuid.NewString()

	startPayload, _ := json.Marshal(sessionPayload{
		Event:     EventStartSession,
		Namespace: config.TTSNamespace,
		ReqParams: reqParams{
			Speaker:     voice,
			AudioParams: &audioParams{Format: config.TTS.Format, SampleRate: config.TTS.SampleRate},
		},
	})
	if err := conn.WriteMessage(websocket.BinaryMessage, buildClientFrame(EventStartSession, sessionID, startPayload)); err != nil {
		return nil, "", fmt.Errorf("send StartSession: %w", err)
	}
	if _, err := c.waitEvent(conn, EventSessionStarted, EventSessionFailed); err != nil {
		return nil, "", fmt.Errorf("等待 SessionStarted: %w", err)
	}

	taskPayload, _ := json.Marshal(sessionPayload{
		Event:     EventTaskRequest,
		Namespace: config.TTSNamespace,
		ReqParams: reqParams{Text: text},
	})
	if err := conn.WriteMessage(websocket.BinaryMessage, buildClientFrame(EventTaskRequest, sessionID, taskPayload)); err != nil {
		return nil, "", fmt.Errorf("send TaskRequest: %w", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, buildClientFrame(EventFinishSession, sessionID, []byte("{}"))); err != nil {
		return nil, "", fmt.Errorf("send FinishSession: %w", err)
	}

	var audio []byte
	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		default:
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return nil, "", fmt.Errorf("读取 tts 帧: %w", err)
		}
		frame, err := parseServerFrame(data)
		if err != nil {
			return nil, "", fmt.Errorf("解析 tts 帧: %w", err)
		}
		switch {
		case frame.IsError:
			return nil, "", fmt.Errorf("tts 服务端错误 code=%d msg=%s", frame.ErrorCode, string(frame.Payload))
		case frame.MessageType == msgAudioOnlyResp || frame.Event == EventTTSResponse:
			audio = append(audio, frame.Payload...)
		case frame.Event == EventSessionFailed:
			return nil, "", fmt.Errorf("tts 会话失败: %s", string(frame.Payload))
		case frame.Event == EventSessionFinished:
			// 会话结束但保持连接,供下一句复用(连接级 FinishConnection 留到 Close)。
			if len(audio) == 0 {
				return nil, "", fmt.Errorf("tts 完成但未返回音频")
			}
			return audio, config.TTS.Format, nil
		default:
			// TTSSentenceStart/End 等,忽略。
			c.log.Debug("tts event ignored", slog.String("event", describeEvent(frame.Event)))
		}
	}
}

// dropConnLocked 关闭并清空当前连接(调用方须持有 c.mu)。
func (c *Client) dropConnLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// Close 关闭复用的持久连接(尽力发送连接级 FinishConnection)。会话结束时调用。
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.WriteMessage(websocket.BinaryMessage, buildClientFrame(EventFinishConnection, "", []byte("{}")))
		_ = c.conn.Close()
		c.conn = nil
	}
}

// waitEvent 读取若干帧直到出现期望事件;命中 failEvent 或错误帧则返回错误。
func (c *Client) waitEvent(conn *websocket.Conn, want, fail int32) (*ServerFrame, error) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		frame, err := parseServerFrame(data)
		if err != nil {
			return nil, err
		}
		if frame.IsError {
			return nil, fmt.Errorf("服务端错误 code=%d msg=%s", frame.ErrorCode, string(frame.Payload))
		}
		if frame.Event == fail {
			return nil, fmt.Errorf("%s: %s", describeEvent(frame.Event), string(frame.Payload))
		}
		if frame.Event == want {
			return frame, nil
		}
		// 其它事件(音频/句子标记)在握手阶段一般不会出现,记录后继续。
		c.log.Debug("tts waiting event", slog.String("got", describeEvent(frame.Event)), slog.String("want", describeEvent(want)))
	}
}
