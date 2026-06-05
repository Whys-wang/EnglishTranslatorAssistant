package tts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"log/slog"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"simul-interpreter/internal/config"
)

// Client 是「双向流式 V3」语音合成客户端;每次 Synthesize 用一条独立连接完成一句合成。
type Client struct {
	log *slog.Logger
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
// 流程:StartConnection -> StartSession -> TaskRequest(text) -> FinishSession -> 收音频 -> SessionFinished。
func (c *Client) Synthesize(ctx context.Context, text string) ([]byte, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", nil
	}
	if !Configured() {
		return nil, "", fmt.Errorf("tts 未启用或音色未配置")
	}

	ctx, cancel := context.WithTimeout(ctx, config.TTS.Timeout)
	defer cancel()

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
			return nil, "", fmt.Errorf("dial tts (http %d, logid=%s): %w", resp.StatusCode, resp.Header.Get("X-Tt-Logid"), err)
		}
		return nil, "", fmt.Errorf("dial tts: %w", err)
	}
	defer conn.Close()
	if logid := resp.Header.Get("X-Tt-Logid"); logid != "" {
		c.log.Debug("tts handshake", slog.String("x_tt_logid", logid))
	}

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(dl)
		_ = conn.SetReadDeadline(dl)
	}

	sessionID := uuid.NewString()

	// 1) StartConnection
	if err := conn.WriteMessage(websocket.BinaryMessage, buildClientFrame(EventStartConnection, "", []byte("{}"))); err != nil {
		return nil, "", fmt.Errorf("send StartConnection: %w", err)
	}
	if _, err := c.waitEvent(conn, EventConnectionStarted, EventConnectionFailed); err != nil {
		return nil, "", fmt.Errorf("等待 ConnectionStarted: %w", err)
	}

	// 2) StartSession
	startPayload, _ := json.Marshal(sessionPayload{
		Event:     EventStartSession,
		Namespace: config.TTSNamespace,
		ReqParams: reqParams{
			Speaker:     config.TTSVoiceType,
			AudioParams: &audioParams{Format: config.TTS.Format, SampleRate: config.TTS.SampleRate},
		},
	})
	if err := conn.WriteMessage(websocket.BinaryMessage, buildClientFrame(EventStartSession, sessionID, startPayload)); err != nil {
		return nil, "", fmt.Errorf("send StartSession: %w", err)
	}
	if _, err := c.waitEvent(conn, EventSessionStarted, EventSessionFailed); err != nil {
		return nil, "", fmt.Errorf("等待 SessionStarted: %w", err)
	}

	// 3) TaskRequest(文本) + FinishSession
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

	// 4) 收音频直到 SessionFinished。
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
			// 尽力发 FinishConnection,失败无所谓。
			_ = conn.WriteMessage(websocket.BinaryMessage, buildClientFrame(EventFinishConnection, "", []byte("{}")))
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
