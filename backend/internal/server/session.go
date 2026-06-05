package server

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"log/slog"

	"github.com/gorilla/websocket"

	"simul-interpreter/internal/asr"
	"simul-interpreter/internal/config"
	"simul-interpreter/internal/translate"
)

// segState 保存一个 segment 的最新状态,用于翻译上下文与回填。
type segState struct {
	id        string
	source    string
	target    string
	startTime int
	endTime   int
	seq       int // 首次出现的序号,用于按时间顺序构造上下文
}

// session 表示一条前端连接的会话,桥接前端 WebSocket 与上游 ASR / 翻译。
type session struct {
	id   string
	log  *slog.Logger
	conn *websocket.Conn

	writeMu sync.Mutex // gorilla 要求同一连接的写操作串行化
	asr     *asr.Client

	tr        *translate.Client
	trQueue   chan string // 待翻译的 segment id
	trEnabled bool        // Ark 是否已配置(未配置则跳过翻译)

	ctx    context.Context
	cancel context.CancelFunc

	// 记录每个 segment 上一次下发的原文,用于去重(避免无变化的重复推送)。
	segMu    sync.Mutex
	lastSent map[string]string
	segments map[string]*segState
	order    []string // segment id 按首次出现顺序
}

func newSession(parent context.Context, id string, log *slog.Logger, conn *websocket.Conn) *session {
	ctx, cancel := context.WithCancel(parent)
	s := &session{
		id:        id,
		log:       log,
		conn:      conn,
		ctx:       ctx,
		cancel:    cancel,
		tr:        translate.NewClient(log),
		trQueue:   make(chan string, 64),
		trEnabled: translate.Configured(),
		lastSent:  make(map[string]string),
		segments:  make(map[string]*segState),
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

// start 在后台拉起 ASR 上游连接(含自动重连),并按需启动翻译 worker。
func (s *session) start() {
	go s.asr.Run(s.ctx)
	if s.trEnabled {
		go s.translateLoop()
	} else {
		s.log.Warn("translation disabled: Ark 未配置(ArkAPIKey/ArkModel 仍是占位符),仅输出原文字幕")
	}
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

// onASREvent 把 ASR 分句结果映射为字幕事件回发前端,并对定稿分句触发翻译。
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
			"target":     "", // 译文由翻译 worker 异步回填
			"status":     status,
			"start_time": u.StartTime,
			"end_time":   u.EndTime,
		})

		// 仅对定稿(final)分句触发翻译,降低调用量与抖动。
		if u.Definite && s.trEnabled {
			s.enqueueTranslate(segID, u.Text, u.StartTime, u.EndTime)
		}
	}
}

// enqueueTranslate 记录/更新 segment 状态并把它加入翻译队列(非阻塞)。
func (s *session) enqueueTranslate(segID, source string, start, end int) {
	s.segMu.Lock()
	st, ok := s.segments[segID]
	if !ok {
		st = &segState{id: segID, startTime: start, endTime: end, seq: len(s.order)}
		s.segments[segID] = st
		s.order = append(s.order, segID)
	}
	st.source = source
	st.endTime = end
	s.segMu.Unlock()

	select {
	case s.trQueue <- segID:
	default:
		s.log.Warn("translate queue full, drop", slog.String("seg", segID))
	}
}

// translateLoop 串行消费翻译队列,保证上下文顺序一致、避免并发打爆接口。
func (s *session) translateLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case segID := <-s.trQueue:
			s.translateSegment(segID)
		}
	}
}

// buildContext 取出 seq 之前、已有译文的最近 N 段作为上下文。
func (s *session) buildContext(seq int) []translate.Pair {
	s.segMu.Lock()
	var pairs []translate.Pair
	for _, id := range s.order {
		st := s.segments[id]
		if st == nil || st.seq >= seq || st.target == "" {
			continue
		}
		pairs = append(pairs, translate.Pair{Source: st.source, Target: st.target})
	}
	s.segMu.Unlock()

	if win := config.Translate.ContextWindow; win > 0 && len(pairs) > win {
		pairs = pairs[len(pairs)-win:]
	}
	return pairs
}

// translateSegment 翻译单个 segment 并把译文回填到前端(同 segment_id 原地更新)。
func (s *session) translateSegment(segID string) {
	s.segMu.Lock()
	st := s.segments[segID]
	if st == nil {
		s.segMu.Unlock()
		return
	}
	source, seq, start, end := st.source, st.seq, st.startTime, st.endTime
	s.segMu.Unlock()

	if strings.TrimSpace(source) == "" {
		return
	}

	target, err := s.tr.Translate(s.ctx, source, s.buildContext(seq))
	if err != nil {
		s.log.Warn("translate failed", slog.String("seg", segID), slog.String("err", err.Error()))
		s.writeJSON(map[string]any{"type": "translate_error", "segment_id": segID, "message": err.Error()})
		return
	}
	if target == "" {
		return
	}

	// 若期间该 segment 的原文已被修订,则丢弃这次(过期)译文。
	s.segMu.Lock()
	st = s.segments[segID]
	if st == nil || st.source != source {
		s.segMu.Unlock()
		return
	}
	st.target = target
	s.segMu.Unlock()

	s.writeJSON(map[string]any{
		"type":       "subtitle",
		"segment_id": segID,
		"source":     source,
		"target":     target,
		"status":     "final",
		"start_time": start,
		"end_time":   end,
	})
	s.log.Debug("translation backfilled", slog.String("seg", segID))
}

func (s *session) onASRError(err error) {
	s.log.Warn("asr error", slog.String("err", err.Error()))
	s.writeJSON(map[string]any{"type": "asr_error", "message": err.Error()})
}
