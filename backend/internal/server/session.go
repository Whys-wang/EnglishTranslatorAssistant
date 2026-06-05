package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/gorilla/websocket"

	"simul-interpreter/internal/asr"
	"simul-interpreter/internal/config"
	"simul-interpreter/internal/translate"
	"simul-interpreter/internal/tts"
)

// segState 保存一个 segment 的最新状态,用于翻译上下文与回填。
type segState struct {
	id         string
	source     string
	target     string
	targetLang string // 该 segment 翻译所用的目标语言(用于选 TTS 音色)
	startTime  int
	endTime    int
	seq        int  // 首次出现的序号,用于按时间顺序构造上下文
	translated bool // 是否已产生过 final 译文(用于判定 ASR 修订重译)
	revised    bool // 本次重译是否由 ASR 修订触发(下发时打 corrected 标记)
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

	tts        *tts.Client
	ttsQueue   chan ttsJob // 待合成的译文
	ttsEnabled bool        // TTS 是否启用且音色已配置

	ctx    context.Context
	cancel context.CancelFunc

	// 翻译方向:源语言提示(留空=自动识别)与目标语言。由前端 start 消息设置。
	langMu  sync.RWMutex
	srcLang string
	tgtLang string

	// 记录每个 segment 上一次下发的原文,用于去重(避免无变化的重复推送)。
	segMu         sync.Mutex
	lastSent      map[string]string
	segments      map[string]*segState
	order         []string          // segment id 按首次出现顺序
	lastReviewKey string            // 上次周期性复审的窗口指纹,内容未变则跳过,省调用
	lastTTS       map[string]string // 每个 segment 上次已合成的译文,避免重复合成
}

// ttsJob 是一条待合成的译文任务。
type ttsJob struct {
	segID string
	text  string
	lang  string // 目标语言,用于选音色
}

func newSession(parent context.Context, id string, log *slog.Logger, conn *websocket.Conn) *session {
	ctx, cancel := context.WithCancel(parent)
	s := &session{
		id:         id,
		log:        log,
		conn:       conn,
		ctx:        ctx,
		cancel:     cancel,
		tr:         translate.NewClient(log),
		trQueue:    make(chan string, 64),
		trEnabled:  translate.Configured(),
		tts:        tts.NewClient(log),
		ttsQueue:   make(chan ttsJob, 64),
		ttsEnabled: tts.Configured(),
		lastSent:   make(map[string]string),
		segments:   make(map[string]*segState),
		lastTTS:    make(map[string]string),
		tgtLang:    config.TargetLanguage, // 默认目标语言,前端可覆盖
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

// setLanguages 设置翻译方向(由前端 start 消息携带)。
// src 为源语言提示(空/"自动检测" 视为自动);tgt 为目标语言(空则保持默认)。
func (s *session) setLanguages(src, tgt string) {
	s.langMu.Lock()
	defer s.langMu.Unlock()
	src = strings.TrimSpace(src)
	if src == "自动检测" || src == "auto" {
		src = ""
	}
	s.srcLang = src
	if t := strings.TrimSpace(tgt); t != "" {
		s.tgtLang = t
	}
	s.log.Info("languages set", slog.String("source", s.srcLang), slog.String("target", s.tgtLang))
}

// languages 返回当前的源语言提示与目标语言。
func (s *session) languages() (src, tgt string) {
	s.langMu.RLock()
	defer s.langMu.RUnlock()
	return s.srcLang, s.tgtLang
}

// start 在后台拉起 ASR 上游连接(含自动重连),并按需启动翻译 / 复审 worker。
func (s *session) start() {
	go s.asr.Run(s.ctx)
	if s.trEnabled {
		go s.translateLoop()
		// 纠错第二层:周期性 LLM 复审(用后文校正前文)。
		if config.Correction.EnablePeriodicReview {
			go s.reviewLoop()
		}
		// 里程碑 7:译文转语音(可开关,默认关闭/需配置音色)。
		if s.ttsEnabled {
			go s.ttsLoop()
		} else if config.TTS.Enable {
			s.log.Warn("tts disabled: 已开启 TTS.Enable 但 TTSVoiceType 仍是占位符,跳过语音合成")
		}
	} else {
		s.log.Warn("translation disabled: Ark 未配置(ArkAPIKey/ArkModel 仍是占位符),仅输出原文字幕")
	}
}

// close 结束会话,停止 ASR 并释放复用的 TTS 连接。
func (s *session) close() {
	s.cancel()
	s.asr.Close()
	if s.tts != nil {
		s.tts.Close()
	}
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
//
// 纠错第一层(ASR 修订重译):若某 segment 已产生过译文,而 ASR 之后又对其
// 返回了不同的原文,则视为「修订」。开启 EnableASRRevision 时重新翻译并打
// corrected 标记;关闭时仅更新原文记录、不重译。
func (s *session) enqueueTranslate(segID, source string, start, end int) {
	s.segMu.Lock()
	st, ok := s.segments[segID]
	if !ok {
		st = &segState{id: segID, startTime: start, endTime: end, seq: len(s.order)}
		s.segments[segID] = st
		s.order = append(s.order, segID)
	}
	isRevision := ok && st.translated && st.source != source
	if isRevision && !config.Correction.EnableASRRevision {
		st.source = source
		st.endTime = end
		s.segMu.Unlock()
		return
	}
	st.source = source
	st.endTime = end
	if isRevision {
		st.revised = true
		s.log.Debug("asr revision -> retranslate", slog.String("seg", segID))
	}
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

	srcLang, tgtLang := s.languages()
	target, err := s.tr.Translate(s.ctx, source, s.buildContext(seq), tgtLang, srcLang)
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
	st.targetLang = tgtLang
	st.translated = true
	corrected := st.revised // 本次回填是否源于 ASR 修订
	st.revised = false
	s.segMu.Unlock()

	s.writeJSON(map[string]any{
		"type":       "subtitle",
		"segment_id": segID,
		"source":     source,
		"target":     target,
		"status":     "final",
		"start_time": start,
		"end_time":   end,
		"corrected":  corrected,
	})
	s.log.Debug("translation backfilled", slog.String("seg", segID), slog.Bool("corrected", corrected))

	s.enqueueTTS(segID, target, tgtLang)
}

// enqueueTTS 把一段译文加入语音合成队列(非阻塞,去重)。lang 为目标语言,用于选音色。
func (s *session) enqueueTTS(segID, text, lang string) {
	if !s.ttsEnabled || strings.TrimSpace(text) == "" {
		return
	}
	s.segMu.Lock()
	if s.lastTTS[segID] == text {
		s.segMu.Unlock()
		return // 同一 segment 的同一译文已合成过,跳过
	}
	s.lastTTS[segID] = text
	s.segMu.Unlock()

	select {
	case s.ttsQueue <- ttsJob{segID: segID, text: text, lang: lang}:
	default:
		s.log.Warn("tts queue full, drop", slog.String("seg", segID))
	}
}

// ttsLoop 串行消费合成队列(避免并发打爆接口,也让音频顺序与字幕一致)。
func (s *session) ttsLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.ttsQueue:
			s.synthesize(job)
		}
	}
}

// synthesize 合成一句译文并把音频(base64)下发前端播放。
// 按目标语言选音色;该语言未配置音色时跳过(仅出字幕)。
func (s *session) synthesize(job ttsJob) {
	voice := config.TTSVoiceFor(job.lang)
	if voice == "" {
		s.log.Debug("tts skipped: 目标语言未配置音色", slog.String("lang", job.lang), slog.String("seg", job.segID))
		return
	}
	audio, format, err := s.tts.SynthesizeWith(s.ctx, job.text, voice)
	if err != nil {
		s.log.Warn("tts failed", slog.String("seg", job.segID), slog.String("err", err.Error()))
		return
	}
	if len(audio) == 0 {
		return
	}
	s.writeJSON(map[string]any{
		"type":        "tts_audio",
		"segment_id":  job.segID,
		"format":      format,
		"sample_rate": config.TTS.SampleRate,
		"audio":       base64.StdEncoding.EncodeToString(audio),
	})
	s.log.Debug("tts audio sent", slog.String("seg", job.segID), slog.Int("bytes", len(audio)))
}

// reviewLoop 周期性触发 LLM 复审纠错,直到会话结束。
func (s *session) reviewLoop() {
	interval := config.Correction.ReviewInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.reviewRecent()
		}
	}
}

// reviewRecent 取最近 N 段「原文+译文」整体送 LLM 复审,把被改进的译文原地更新。
func (s *session) reviewRecent() {
	win := config.Correction.ReviewContextWindow
	if win <= 0 {
		win = 5
	}

	// 1) 快照最近 win 段已翻译的 segment(在锁内只做拷贝,不发网络请求)。
	s.segMu.Lock()
	var ids []string
	for _, id := range s.order {
		st := s.segments[id]
		if st != nil && st.translated && strings.TrimSpace(st.source) != "" && strings.TrimSpace(st.target) != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) > win {
		ids = ids[len(ids)-win:]
	}
	items := make([]translate.ReviewItem, 0, len(ids))
	srcSnap := make([]string, 0, len(ids))
	tgtSnap := make([]string, 0, len(ids))
	for _, id := range ids {
		st := s.segments[id]
		items = append(items, translate.ReviewItem{Source: st.source, Target: st.target})
		srcSnap = append(srcSnap, st.source)
		tgtSnap = append(tgtSnap, st.target)
	}
	s.segMu.Unlock()

	if len(items) < 2 {
		return // 不足两句,没有「用后文校正前文」的意义
	}

	// 2) 窗口内容与上次复审完全一致则跳过,避免无谓的接口调用。
	key := strings.Join(srcSnap, "\x1f") + "\x1e" + strings.Join(tgtSnap, "\x1f")
	s.segMu.Lock()
	if key == s.lastReviewKey {
		s.segMu.Unlock()
		return
	}
	s.lastReviewKey = key
	s.segMu.Unlock()

	_, tgtLang := s.languages()
	revised, err := s.tr.Review(s.ctx, items, tgtLang)
	if err != nil {
		s.log.Debug("review failed", slog.String("err", err.Error()))
		return
	}
	if len(revised) != len(ids) {
		return
	}

	// 3) 仅覆盖确有改动、且自快照以来未被更新过的 segment。
	for i, id := range ids {
		newTarget := strings.TrimSpace(revised[i])
		if newTarget == "" || newTarget == tgtSnap[i] {
			continue
		}
		s.segMu.Lock()
		st := s.segments[id]
		if st == nil || st.source != srcSnap[i] || st.target != tgtSnap[i] {
			s.segMu.Unlock() // 期间已有更新的重译,放弃这次复审结果
			continue
		}
		st.target = newTarget
		source, start, end := st.source, st.startTime, st.endTime
		s.segMu.Unlock()

		s.writeJSON(map[string]any{
			"type":       "subtitle",
			"segment_id": id,
			"source":     source,
			"target":     newTarget,
			"status":     "final",
			"start_time": start,
			"end_time":   end,
			"corrected":  true,
		})
		s.log.Debug("review corrected", slog.String("seg", id))
		s.enqueueTTS(id, newTarget, tgtLang)
	}
}

func (s *session) onASRError(err error) {
	s.log.Warn("asr error", slog.String("err", err.Error()))
	s.writeJSON(map[string]any{"type": "asr_error", "message": err.Error()})
}
