// Package server 实现前端扩展 <-> 后端的 WebSocket 中继。
//
//   - 启动 HTTP 服务,提供 /healthz 健康检查;
//   - 提供 /ws,接受前端连接,接收音频二进制分片与文本控制消息;
//   - 为每个会话建立上游火山 ASR 连接,转发 PCM,并把识别结果(partial/final)
//     作为字幕事件回发前端。翻译 -> TTS 在后续里程碑填充。
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"log/slog"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"simul-interpreter/internal/config"
)

// Server 持有运行期依赖。
type Server struct {
	log      *slog.Logger
	upgrader websocket.Upgrader
	httpSrv  *http.Server
}

// New 创建一个 Server。
func New(log *slog.Logger) *Server {
	s := &Server{
		log: log,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// 扩展运行在 chrome-extension:// 源下,放行跨源握手。
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc(config.HealthPath, s.handleHealth)
	mux.HandleFunc(config.ClientWSPath, s.handleWS)

	s.httpSrv = &http.Server{
		Addr:    config.ListenAddr,
		Handler: mux,
	}
	return s
}

// Run 启动服务并阻塞,直到 ctx 取消后优雅关闭。
func (s *Server) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(shutdownCtx)
	}()

	s.log.Info("websocket relay listening",
		slog.String("addr", config.ListenAddr),
		slog.String("ws_path", config.ClientWSPath),
	)

	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

// clientMessage 是前端通过文本帧发送的控制消息结构。
type clientMessage struct {
	Type       string `json:"type"`       // 例如 "start" / "stop" / "config"
	SourceLang string `json:"sourceLang"` // 源语言提示(空/"自动检测" = 自动)
	TargetLang string `json:"targetLang"` // 目标语言(翻译成什么语言)
}

// handleWS 处理一条前端连接的完整生命周期。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Error("ws upgrade failed", slog.String("err", err.Error()))
		return
	}
	sessionID := uuid.NewString()
	log := s.log.With(slog.String("session", sessionID))
	log.Info("client connected", slog.String("remote", r.RemoteAddr))

	sess := newSession(r.Context(), sessionID, log, conn)
	sess.start() // 后台建立上游 ASR 连接(含自动重连)
	defer func() {
		sess.close()
		_ = conn.Close()
		log.Info("client disconnected")
	}()

	var (
		audioFrames int
		audioBytes  int
	)
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Warn("ws read error", slog.String("err", err.Error()))
			}
			break
		}

		switch msgType {
		case websocket.BinaryMessage:
			// 二进制 = 16kHz/16bit/单声道 PCM 音频分片,转发给上游 ASR。
			audioFrames++
			audioBytes += len(data)
			if audioFrames%50 == 0 { // 节流日志,约每 5 秒打印一次
				// 按 16kHz/16bit/单声道估算已接收音频时长,便于核对采样率是否正确。
				seconds := float64(audioBytes) / float64(config.AudioSampleRate*(config.AudioBitDepth/8)*config.AudioChannels)
				log.Debug("audio received",
					slog.Int("frames", audioFrames),
					slog.Int("bytes", audioBytes),
					slog.Float64("approx_seconds", seconds),
				)
			}
			sess.sendAudio(data)

		case websocket.TextMessage:
			var m clientMessage
			if err := json.Unmarshal(data, &m); err != nil {
				log.Warn("bad control message", slog.String("raw", string(data)))
				continue
			}
			log.Info("control message",
				slog.String("type", m.Type),
				slog.String("source_lang", m.SourceLang),
				slog.String("target_lang", m.TargetLang),
			)
			if m.Type == "start" || m.Type == "config" {
				sess.setLanguages(m.SourceLang, m.TargetLang)
			}
			sess.writeJSON(map[string]any{"type": "ack", "of": m.Type})
		}
	}
}
