package server

import (
	"context"
	"fmt"
	"sync"

	"log/slog"

	"github.com/gorilla/websocket"

	"simul-interpreter/internal/asr"
)

// session 表示一条前端连接的会话,桥接前端 WebSocket 与上游 ASR。
type session struct {
	id   string
	log  *slog.Logger
	conn *websocket.Conn

	writeMu sync.Mutex // gorilla 要求同一连接的写操作串行化
	asr     *asr.Client

	ctx    context.Context
	cancel context.CancelFunc

	// 记录每个 segment 上一次下发的原文,用于去重(避免无变化的重复推送)。
	segMu    sync.Mutex
	lastSent map[string]string
}

func newSession(parent context.Context, id string, log *slog.Logger, conn *websocket.Conn) *session {
	ctx, cancel := context.WithCancel(parent)
	s := &session{
		id:       id,
		log:      log,
		conn:     conn,
		ctx:      ctx,
		cancel:   cancel,
		lastSent: make(map[string]string),
	}
	s.asr = asr.NewClient(id, log, asr.Handlers{
		OnEvent: s.onASREvent,
		OnError: s.onASRError,
		OnLogid: func(logid string) {
			log.Info("asr handshake", slog.String("x_tt_logid", logid))
		},
	})
	return s
}

// start 在后台拉起 ASR 上游连接(含自动重连)。
func (s *session) start() {
	go s.asr.Run(s.ctx)
}

// close 结束会话,停止 ASR。
func (s *session) close() {
	s.cancel()
	s.asr.Close()
}

// sendAudio 转发一段前端上来的 PCM 到 ASR。
func (s *session) sendAudio(pcm []byte) {
	if err := s.asr.SendAudio(pcm, false); err != nil {
		// 重连期间会失败,直接丢弃实时帧,避免积压。
		s.log.Debug("drop audio frame", slog.String("reason", err.Error()))
	}
}

// writeJSON 串行化地向前端连接写一条 JSON。
func (s *session) writeJSON(v any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.WriteJSON(v); err != nil {
		s.log.Debug("write to client failed", slog.String("err", err.Error()))
	}
}

// onASREvent 把 ASR 分句结果映射为字幕事件回发前端。
func (s *session) onASREvent(ev asr.Event) {
	for _, u := range ev.Utterances {
		if u.Text == "" {
			continue
		}
		// 用 start_time 作为稳定的 segment_id:同一句在多次返回间保持一致,
		// 文本变化即原地更新(体现 partial->final 与服务端修订)。
		segID := fmt.Sprintf("seg-%d", u.StartTime)
		status := "partial"
		if u.Definite {
			status = "final"
		}

		fingerprint := u.Text + "|" + status
		s.segMu.Lock()
		if prev, ok := s.lastSent[segID]; ok && prev == fingerprint {
			s.segMu.Unlock()
			continue // 与上次完全一致,跳过。
		}
		s.lastSent[segID] = fingerprint
		s.segMu.Unlock()

		s.writeJSON(map[string]any{
			"type":       "subtitle",
			"segment_id": segID,
			"source":     u.Text,
			"target":     "", // 译文在里程碑 4 回填
			"status":     status,
			"start_time": u.StartTime,
			"end_time":   u.EndTime,
		})
	}
}

func (s *session) onASRError(err error) {
	s.log.Warn("asr error", slog.String("err", err.Error()))
	s.writeJSON(map[string]any{"type": "asr_error", "message": err.Error()})
}
