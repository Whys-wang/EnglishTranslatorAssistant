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

	tr           *translate.Client
	trQueue      chan string   // 待翻译的 segment id
	trEnabled    bool          // Ark 是否已配置(未配置则跳过翻译)
	reviewSignal chan struct{} // 句子边界触发复审的信号(缓冲 1,自动合并)

	tts        *tts.Client
	ttsQueue   chan ttsJob // 待合成的译文
	ttsEnabled bool        // TTS 是否启用且音色已配置

	ctx    context.Context
	cancel context.CancelFunc

	// 翻译方向:源语言提示(留空=自动识别)与目标语言。由前端 start 消息设置。
	langMu  sync.RWMutex
	srcLang string
	tgtLang string
	langGen uint64 // 每次变更语言 +1,用于丢弃切换前已在途的译文

	// ASR 分句 -> 稳定 segment_id(避免 start_time 重复时覆盖上一句字幕)。
	asrSegMu      sync.Mutex
	asrSegSeq     int
	asrSegByStart map[int]*asrSegBind

	// 记录每个 segment 上一次下发的原文,用于去重(避免无变化的重复推送)。
	segMu         sync.Mutex
	lastSent      map[string]string
	segments      map[string]*segState
	order         []string          // segment id 按首次出现顺序
	lastReviewKey string            // 上次周期性复审的窗口指纹,内容未变则跳过,省调用
	lastTTS       map[string]string // 每个 segment 上次已合成的译文,避免重复合成

	// 边说边译:对说话中(partial)的句子做节流翻译的状态。
	partMu    sync.Mutex
	partState map[string]*partialState

	// 字幕去重:短时间内相同译文不重复上屏(避免 partial/小句切分重叠)。
	subDedupMu    sync.Mutex
	lastSubTarget string
	lastSubAt     time.Time
}

// asrSegBind 把 ASR start_time 绑定到会话内单调递增的 segment id。
type asrSegBind struct {
	id       string
	definite bool
}

// partialState 记录某句「说话中」的实时翻译节流状态。
type partialState struct {
	lastSource       string
	lastTarget       string
	lastAt           time.Time
	inflight         bool
	pendingSource    string
	lastPushedTarget string
	cancel           context.CancelFunc
	lockedSource     string // 已切分上屏的小句原文前缀
	clauseSeq        int
	lastClauseSource string // 上一段已上屏小句原文(给下一段当上下文)
	lastClauseTarget string // 上一段已上屏小句译文
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
		id:           id,
		log:          log,
		conn:         conn,
		ctx:          ctx,
		cancel:       cancel,
		tr:           translate.NewClient(log),
		trQueue:      make(chan string, 64),
		trEnabled:    translate.Configured(),
		reviewSignal: make(chan struct{}, 1),
		tts:          tts.NewClient(log),
		ttsQueue:     make(chan ttsJob, 64),
		ttsEnabled:   tts.Configured(),
		lastSent:     make(map[string]string),
		segments:     make(map[string]*segState),
		lastTTS:      make(map[string]string),
		partState:    make(map[string]*partialState),
		asrSegByStart: make(map[int]*asrSegBind),
		tgtLang:      config.TargetLanguage, // 默认目标语言,前端可覆盖
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

// setLanguages 设置翻译方向(由前端 start/config 消息携带)。
// src 为源语言提示(空/"自动检测" 视为自动);tgt 为目标语言(空则保持默认)。
// 方向变化时会清空已有译文缓存,避免旧语言的上下文污染新字幕。
func (s *session) setLanguages(src, tgt string) {
	newSrc := translate.NormalizeLang(src)
	newTgt := s.tgtLang
	if t := translate.NormalizeLang(tgt); t != "" {
		newTgt = t
	}

	s.langMu.Lock()
	oldSrc := s.srcLang
	changed := newSrc != s.srcLang || newTgt != s.tgtLang
	s.srcLang = newSrc
	s.tgtLang = newTgt
	s.langMu.Unlock()

	// 源语言变化时同步 ASR 路由:英/中/自动→async;其他语种→nostream+language。
	if newSrc != oldSrc {
		s.asr.SetSourceLanguage(newSrc)
	}

	if changed {
		s.bumpLangGen()
		s.resetTranslationState()
		s.requeueAllSegments()
		s.notifyLanguageChange()
	}
	s.log.Info("languages set", slog.String("source", s.srcLang), slog.String("target", s.tgtLang))
}

func (s *session) bumpLangGen() {
	s.langMu.Lock()
	s.langGen++
	s.langMu.Unlock()
}

func (s *session) currentLangGen() uint64 {
	s.langMu.RLock()
	defer s.langMu.RUnlock()
	return s.langGen
}

// notifyLanguageChange 通知前端立刻清空当前字幕,等待新语言译文回填。
func (s *session) notifyLanguageChange() {
	src, tgt := s.languages()
	s.writeJSON(map[string]any{
		"type":       "lang_change",
		"sourceLang": src,
		"targetLang": tgt,
	})
}

// requeueAllSegments 语言切换后,把已有原文的句子全部重新送入翻译队列。
func (s *session) requeueAllSegments() {
	if !s.trEnabled {
		return
	}
	s.segMu.Lock()
	ids := append([]string(nil), s.order...)
	s.segMu.Unlock()
	for _, id := range ids {
		s.segMu.Lock()
		st := s.segments[id]
		ok := st != nil && strings.TrimSpace(st.source) != ""
		s.segMu.Unlock()
		if !ok {
			continue
		}
		select {
		case s.trQueue <- id:
		default:
			s.log.Warn("translate queue full on lang change", slog.String("seg", id))
		}
	}
}

// resetTranslationState 在翻译方向变更后丢弃旧译文/上下文,防止中英混杂。
func (s *session) resetTranslationState() {
	s.segMu.Lock()
	for _, st := range s.segments {
		st.target = ""
		st.targetLang = ""
		st.translated = false
		st.revised = false
	}
	s.lastReviewKey = ""
	s.lastTTS = make(map[string]string)
	s.segMu.Unlock()

	s.partMu.Lock()
	s.partState = make(map[string]*partialState)
	s.partMu.Unlock()

	s.asrSegMu.Lock()
	s.asrSegByStart = make(map[int]*asrSegBind)
	s.asrSegMu.Unlock()
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
		go func() { s.tr.Prewarm(s.ctx) }()
		workers := config.Translate.FinalWorkers
		if workers < 1 {
			workers = 1
		}
		for i := 0; i < workers; i++ {
			go s.translateLoop()
		}
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

// writeSubtitle 下发字幕;过滤空译文,并抑制 3 秒内完全相同的重复行。
func (s *session) writeSubtitle(v map[string]any) {
	target, _ := v["target"].(string)
	target = strings.TrimSpace(target)
	if target == "" {
		return
	}
	v["target"] = target
	status, _ := v["status"].(string)
	// 只抑制 partial 之间的重复刷屏;定稿 final 必须下发(哪怕与预览相同)。
	if status == "partial" {
		s.subDedupMu.Lock()
		if target == s.lastSubTarget && time.Since(s.lastSubAt) < 3*time.Second {
			s.subDedupMu.Unlock()
			return
		}
		s.lastSubTarget = target
		s.lastSubAt = time.Now()
		s.subDedupMu.Unlock()
	}
	s.writeJSON(v)
}

// asrSegmentID 为 ASR 分句分配稳定的 segment_id。
// 同一句 partial/final 共用 id;上一句定稿后即使 start_time 重复也分配新 id,避免覆盖滚动历史。
func (s *session) asrSegmentID(u asr.Utterance) string {
	s.asrSegMu.Lock()
	defer s.asrSegMu.Unlock()

	bind, ok := s.asrSegByStart[u.StartTime]
	if !ok || (bind.definite && !u.Definite) {
		s.asrSegSeq++
		id := fmt.Sprintf("seg-%d", s.asrSegSeq)
		s.asrSegByStart[u.StartTime] = &asrSegBind{id: id, definite: u.Definite}
		return id
	}
	if u.Definite {
		bind.definite = true
	}
	return bind.id
}

// onASREvent 把 ASR 分句结果映射为字幕事件回发前端,并对定稿分句触发翻译。
func (s *session) onASREvent(ev asr.Event) {
	for _, u := range ev.Utterances {
		if u.Text == "" {
			continue
		}
		segID := s.asrSegmentID(u)
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

		// 翻译开启时只推送带译文的字幕(由 partial/final 翻译路径回填),
		// 避免 ASR 空 target 消息触发前端整屏重绘、拖慢同步感。
		if !s.trEnabled {
			s.writeJSON(map[string]any{
				"type":       "subtitle",
				"segment_id": segID,
				"source":     u.Text,
				"target":     u.Text,
				"status":     status,
				"start_time": u.StartTime,
				"end_time":   u.EndTime,
			})
		}

		if s.trEnabled {
			if u.Definite {
				// 定稿句子:走正式翻译(带上下文 + 纠错 + TTS)。
				s.enqueueTranslate(segID, u.Text, u.StartTime, u.EndTime)
			} else if config.Translate.PartialPreview {
				srcLang, _ := s.languages()
				policy := translate.SegmentPolicyFor(u.Text, srcLang)
				if !policy.SkipPartialPreview {
					// 边说边译:对说话中的句子做节流翻译,实时顶出预览译文,
					// 句子定稿后再被正式译文原地覆盖。
					s.maybeTranslatePartial(segID, u.Text, u.StartTime, u.EndTime)
				}
			}
		}
	}
}

// tryMergeUtterance 把 ASR 连续短定稿并入上一条字幕,减少俄语等「两三词一行」。
func (s *session) tryMergeUtterance(segID, source string, start, end int) (string, string, int, int, bool) {
	srcLang, _ := s.languages()
	policy := translate.SegmentPolicyFor(source, srcLang)
	if !policy.MergeShortUtterances {
		return segID, source, start, end, false
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return segID, source, start, end, false
	}

	s.segMu.Lock()
	defer s.segMu.Unlock()

	if len(s.order) == 0 {
		return segID, source, start, end, false
	}
	prevID := s.order[len(s.order)-1]
	if prevID == segID {
		return segID, source, start, end, false
	}
	prev := s.segments[prevID]
	if prev == nil {
		return segID, source, start, end, false
	}

	prevSrc := strings.TrimSpace(prev.source)
	newRunes := len([]rune(source))
	prevRunes := len([]rune(prevSrc))
	gap := start - prev.endTime
	if gap < 0 {
		gap = 0
	}

	const maxGapMS = 1500
	const shortRunes = 32
	shouldMerge := gap <= maxGapMS &&
		(newRunes <= shortRunes || prevRunes <= shortRunes || prevRunes+newRunes <= 64)
	if !shouldMerge {
		return segID, source, start, end, false
	}
	if prevSrc == source || strings.HasSuffix(prevSrc, source) {
		prev.endTime = end
		return prevID, prevSrc, prev.startTime, end, true
	}

	merged := prevSrc
	if merged != "" {
		merged += " "
	}
	merged += source
	prev.source = merged
	prev.endTime = end
	if prev.translated {
		prev.translated = false
		prev.revised = true
	}

	if st, ok := s.segments[segID]; ok && st != nil && len(s.order) > 0 && s.order[len(s.order)-1] == segID {
		s.order = s.order[:len(s.order)-1]
		delete(s.segments, segID)
		_ = st
	}
	delete(s.lastSent, segID)

	if s.log != nil {
		s.log.Debug("utterance merged",
			slog.String("into", prevID),
			slog.String("dropped", segID),
			slog.Int("gap_ms", gap),
			slog.Int("merged_runes", len([]rune(merged))),
		)
	}
	return prevID, merged, prev.startTime, end, true
}

// enqueueTranslate 记录/更新 segment 状态并把它加入翻译队列(非阻塞)。
//
// 纠错第一层(ASR 修订重译):若某 segment 已产生过译文,而 ASR 之后又对其
// 返回了不同的原文,则视为「修订」。开启 EnableASRRevision 时重新翻译并打
// corrected 标记;关闭时仅更新原文记录、不重译。
func (s *session) enqueueTranslate(segID, source string, start, end int) {
	if mergedID, mergedSrc, mStart, mEnd, ok := s.tryMergeUtterance(segID, source, start, end); ok {
		dropped := segID
		segID, source, start, end = mergedID, mergedSrc, mStart, mEnd
		s.partMu.Lock()
		delete(s.partState, dropped)
		delete(s.partState, mergedID)
		s.partMu.Unlock()
	}

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

	srcLang, _ := s.languages()
	policy := translate.SegmentPolicyFor(source, srcLang)

	if !policy.DisableClauseFlush {
		s.flushRemainingTail(segID, source, start, end)
	}

	partialTarget := s.snapshotPartialTarget(segID)
	promoteWait := config.Translate.PartialPromoteWait
	if policy.PromotePartialOnFinal && policy.PartialPromoteWait > 0 {
		promoteWait = policy.PartialPromoteWait
	}
	if partialTarget == "" && promoteWait > 0 {
		partialTarget = s.waitPartialTarget(segID, promoteWait)
	}
	s.partMu.Lock()
	ps := s.partState[segID]
	fullyChunked := ps != nil && ps.lockedSource == strings.TrimSpace(source)
	s.partMu.Unlock()

	s.partMu.Lock()
	delete(s.partState, segID)
	s.partMu.Unlock()

	if fullyChunked {
		s.segMu.Lock()
		if st, ok := s.segments[segID]; ok {
			st.translated = true
			st.revised = false
			if partialTarget != "" {
				st.target = partialTarget
			}
		}
		s.segMu.Unlock()
		return
	}

	if config.Translate.RefinePromoted && partialTarget != "" && !policy.DisableClauseFlush {
		s.applyFinalTranslation(segID, source, partialTarget, start, end, isRevision)
		go s.refineSegmentAsync(segID)
		return
	}

	// 仅 CJK 等开启秒升;英语走完整定稿翻译(用户验证过的行为)。
	if policy.PromotePartialOnFinal && partialTarget != "" {
		s.applyFinalTranslation(segID, source, partialTarget, start, end, isRevision)
		if config.Translate.RefinePromoted && !policy.SkipBackgroundRefine {
			go s.refineSegmentAsync(segID)
		}
		return
	}

	select {
	case s.trQueue <- segID:
	default:
		s.log.Warn("translate queue full, drop", slog.String("seg", segID))
	}
}

// maybeTranslatePartial 边说边译:长句按小句切分上屏;尾部短片段流式翻译,避免长时间空白。
func (s *session) maybeTranslatePartial(segID, source string, start, end int) {
	source = strings.TrimSpace(source)
	if source == "" {
		return
	}
	srcLang, _ := s.languages()
	policy := translate.SegmentPolicyFor(source, srcLang)

	if !policy.DisableClauseFlush {
		s.flushCompletedClauses(segID, source, start, end)
	}

	tail := source
	if !policy.DisableClauseFlush {
		s.partMu.Lock()
		ps := s.partState[segID]
		locked := ""
		if ps != nil {
			locked = ps.lockedSource
		}
		s.partMu.Unlock()
		tail = translate.RemainingClauseTail(source, locked)
	}
	if tail == "" {
		return
	}
	s.maybeTranslatePartialTail(segID, tail, source, start, end)
}

func (s *session) flushCompletedClauses(segID, source string, start, end int) {
	s.partMu.Lock()
	ps := s.partState[segID]
	if ps == nil {
		ps = &partialState{}
		s.partState[segID] = ps
	}
	locked := ps.lockedSource
	s.partMu.Unlock()

	srcLang, tgtLang := s.languages()
	policy := translate.SegmentPolicyFor(source, srcLang)
	clauses, newLocked := translate.SplitCompletedClausesWithPolicy(source, locked, policy)
	if len(clauses) == 0 {
		return
	}
	_ = newLocked

	gen := s.currentLangGen()
	for _, clause := range clauses {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		s.partMu.Lock()
		ps = s.partState[segID]
		if ps == nil {
			ps = &partialState{}
			s.partState[segID] = ps
		}
		idx := ps.clauseSeq
		ps.clauseSeq++
		var history []translate.Pair
		if prevSrc := strings.TrimSpace(ps.lastClauseSource); prevSrc != "" {
			if prevTgt := strings.TrimSpace(ps.lastClauseTarget); prevTgt != "" {
				history = []translate.Pair{{Source: prevSrc, Target: prevTgt}}
			}
		}
		s.partMu.Unlock()

		target := clause
		if !translate.ShouldPassthrough(clause, srcLang, tgtLang) {
			t, err := s.tr.TranslateCompact(s.ctx, clause, history, tgtLang, srcLang)
			if err != nil || strings.TrimSpace(t) == "" {
				continue
			}
			target = strings.TrimSpace(t)
		}
		if gen != s.currentLangGen() {
			return
		}
		chunkID := fmt.Sprintf("%s-c%d", segID, idx)
		s.writeSubtitle(map[string]any{
			"type":       "subtitle",
			"segment_id": chunkID,
			"target":     target,
			"status":     "final",
			"start_time": start,
			"end_time":   end,
		})
		s.partMu.Lock()
		if ps := s.partState[segID]; ps != nil {
			ps.lastClauseSource = clause
			ps.lastClauseTarget = target
			rest := source
			if ps.lockedSource != "" && strings.HasPrefix(source, ps.lockedSource) {
				rest = source[len(ps.lockedSource):]
			}
			if i := strings.Index(rest, clause); i >= 0 {
				endPos := len(source) - len(rest) + i + len(clause)
				if endPos <= len(source) {
					ps.lockedSource = strings.TrimSpace(source[:endPos])
				}
			}
		}
		s.partMu.Unlock()
	}
}

func (s *session) partialDisplayID(segID, source string) string {
	srcLang, _ := s.languages()
	if translate.SegmentPolicyFor(source, srcLang).DisableClauseFlush {
		return segID
	}
	s.partMu.Lock()
	defer s.partMu.Unlock()
	ps := s.partState[segID]
	if ps == nil {
		return segID
	}
	return fmt.Sprintf("%s-c%d", segID, ps.clauseSeq)
}

func (s *session) flushRemainingTail(segID, source string, start, end int) {
	srcLang, _ := s.languages()
	if translate.SegmentPolicyFor(source, srcLang).DisableClauseFlush {
		return
	}
	s.flushCompletedClauses(segID, source, start, end)

	s.partMu.Lock()
	ps := s.partState[segID]
	locked := ""
	if ps != nil {
		locked = ps.lockedSource
	}
	s.partMu.Unlock()

	tail := translate.RemainingClauseTail(source, locked)
	if tail == "" {
		return
	}

	srcLang, tgtLang := s.languages()
	gen := s.currentLangGen()
	target := tail
	if !translate.ShouldPassthrough(tail, srcLang, tgtLang) {
		if t := s.snapshotPartialTarget(segID); t != "" {
			target = t
		} else {
			t, err := s.tr.TranslateCompact(s.ctx, tail, nil, tgtLang, srcLang)
			if err != nil || strings.TrimSpace(t) == "" {
				return
			}
			target = strings.TrimSpace(t)
		}
	}
	if gen != s.currentLangGen() {
		return
	}

	s.partMu.Lock()
	ps = s.partState[segID]
	if ps == nil {
		ps = &partialState{}
		s.partState[segID] = ps
	}
	idx := ps.clauseSeq
	ps.lockedSource = source
	ps.clauseSeq++
	s.partMu.Unlock()

	chunkID := fmt.Sprintf("%s-c%d", segID, idx)
	s.writeSubtitle(map[string]any{
		"type":       "subtitle",
		"segment_id": chunkID,
		"target":     target,
		"status":     "final",
		"start_time": start,
		"end_time":   end,
	})
}

// maybeTranslatePartialTail 对仍在说的小句尾部做流式 partial 翻译(输入短,首 token 更快)。
func (s *session) maybeTranslatePartialTail(segID, tail, fullSource string, start, end int) {
	cfg := config.Translate

	s.partMu.Lock()
	ps := s.partState[segID]
	if ps == nil {
		ps = &partialState{}
		s.partState[segID] = ps
	}
	now := time.Now()
	grown := len([]rune(tail)) - len([]rune(ps.lastSource))
	first := ps.lastSource == ""

	if ps.inflight {
		if tail != ps.lastSource {
			ps.pendingSource = fullSource
			srcLang, _ := s.languages()
			policy := translate.SegmentPolicyFor(fullSource, srcLang)
			minChars := cfg.PartialMinChars
			if policy.PartialMinChars > 0 {
				minChars = policy.PartialMinChars
			}
			cancelThreshold := minChars + 2
			if policy.PartialCancelGrow > 0 {
				cancelThreshold = policy.PartialCancelGrow
			}
			if cancelThreshold < 3 {
				cancelThreshold = 3
			}
			if grown >= cancelThreshold && ps.cancel != nil {
				ps.cancel()
			}
		}
		s.partMu.Unlock()
		return
	}
	if !first {
		srcLang, _ := s.languages()
		policy := translate.SegmentPolicyFor(fullSource, srcLang)
		minInterval := cfg.PartialMinInterval
		if policy.PartialMinInterval > 0 {
			minInterval = policy.PartialMinInterval
		}
		if now.Sub(ps.lastAt) < minInterval {
			s.partMu.Unlock()
			return
		}
		minChars := cfg.PartialMinChars
		if policy.PartialMinChars > 0 {
			minChars = policy.PartialMinChars
		}
		if minChars > 0 && grown < minChars {
			s.partMu.Unlock()
			return
		}
	}
	srcLang, _ := s.languages()
	policy := translate.SegmentPolicyFor(fullSource, srcLang)
	minTail := policy.PartialMinTail
	if minTail <= 0 {
		minTail = 15
	}
	if len([]rune(strings.TrimSpace(tail))) < minTail {
		s.partMu.Unlock()
		return
	}

	partialCtx, cancel := context.WithCancel(s.ctx)
	ps.inflight = true
	ps.lastSource = tail
	ps.lastAt = now
	ps.pendingSource = ""
	ps.cancel = cancel
	s.partMu.Unlock()

	go s.translatePartial(segID, tail, start, end, partialCtx)
}

func (s *session) translatePartial(segID, source string, start, end int, partialCtx context.Context) {
	gen := s.currentLangGen()
	defer func() {
		s.partMu.Lock()
		ps := s.partState[segID]
		var pending string
		if ps != nil {
			ps.inflight = false
			ps.cancel = nil
			pending = ps.pendingSource
			ps.pendingSource = ""
		}
		s.partMu.Unlock()
		if pending != "" {
			s.maybeTranslatePartial(segID, pending, start, end)
		}
	}()

	srcLang, tgtLang := s.languages()
	if translate.ShouldPassthrough(source, srcLang, tgtLang) {
		s.emitPartialTranslation(s.partialDisplayID(segID, source), source, source, start, end, gen)
		return
	}

	s.partMu.Lock()
	ps := s.partState[segID]
	var history []translate.Pair
	if ps != nil {
		if prevSrc := strings.TrimSpace(ps.lastClauseSource); prevSrc != "" {
			if prevTgt := strings.TrimSpace(ps.lastClauseTarget); prevTgt != "" {
				history = []translate.Pair{{Source: prevSrc, Target: prevTgt}}
			}
		}
	}
	s.partMu.Unlock()

	onChunk := func(acc string) error {
		if gen != s.currentLangGen() {
			return translate.ErrStreamAbort
		}
		s.segMu.Lock()
		st := s.segments[segID]
		finalized := st != nil && st.translated
		s.segMu.Unlock()
		if finalized {
			return translate.ErrStreamAbort
		}
		s.emitPartialTranslation(s.partialDisplayID(segID, source), source, acc, start, end, gen)
		return nil
	}

	target, err := s.tr.TranslatePartial(partialCtx, source, history, tgtLang, srcLang, onChunk)
	if err != nil {
		if err != translate.ErrStreamAbort && partialCtx.Err() == nil {
			s.log.Debug("partial translate failed", slog.String("seg", segID), slog.String("err", err.Error()))
		}
		return
	}
	if strings.TrimSpace(target) != "" {
		s.emitPartialTranslation(s.partialDisplayID(segID, source), source, target, start, end, gen)
	}
}

func (s *session) emitPartialTranslation(segID, source, target string, start, end int, gen uint64) {
	if gen != s.currentLangGen() {
		return
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return
	}
	s.partMu.Lock()
	ps := s.partState[segID]
	if ps != nil {
		ps.lastTarget = target
		if ps.lastPushedTarget == target {
			s.partMu.Unlock()
			return
		}
		ps.lastPushedTarget = target
	}
	s.partMu.Unlock()

	s.segMu.Lock()
	st := s.segments[segID]
	finalized := st != nil && st.translated
	s.segMu.Unlock()
	if finalized {
		return
	}

	s.writeSubtitle(map[string]any{
		"type":       "subtitle",
		"segment_id": segID,
		"target":     target,
		"status":     "partial",
	})
}

func (s *session) applyFinalTranslation(segID, source, target string, start, end int, corrected bool) {
	_, tgtLang := s.languages()
	s.segMu.Lock()
	st := s.segments[segID]
	if st == nil || st.source != source {
		s.segMu.Unlock()
		return
	}
	st.target = target
	st.targetLang = tgtLang
	st.translated = true
	st.revised = false
	s.segMu.Unlock()

	s.writeSubtitle(map[string]any{
		"type":       "subtitle",
		"segment_id": segID,
		"source":     source,
		"target":     target,
		"status":     "final",
		"start_time": start,
		"end_time":   end,
		"corrected":  corrected,
	})
	s.log.Debug("translation backfilled", slog.String("seg", segID), slog.Bool("promoted", true))
	s.enqueueTTS(segID, target, tgtLang)
	s.triggerReview()
}

func (s *session) refineSegmentAsync(segID string) {
	if !config.Translate.RefinePromoted {
		return
	}
	if d := config.Translate.RefineDelay; d > 0 {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(d):
		}
	}
	s.segMu.Lock()
	st := s.segments[segID]
	if st == nil || !st.translated {
		s.segMu.Unlock()
		return
	}
	source, seq, start, end, oldTarget := st.source, st.seq, st.startTime, st.endTime, st.target
	s.segMu.Unlock()

	srcLang, tgtLang := s.languages()
	target := oldTarget
	if !translate.ShouldPassthrough(source, srcLang, tgtLang) {
		var err error
		target, err = s.tr.TranslateCompact(s.ctx, source, s.buildContext(seq), tgtLang, srcLang)
		if err != nil || strings.TrimSpace(target) == "" || target == oldTarget {
			return
		}
	} else if target == oldTarget {
		return
	}

	s.segMu.Lock()
	st = s.segments[segID]
	if st == nil || st.source != source || st.target != oldTarget {
		s.segMu.Unlock()
		return
	}
	st.target = target
	st.targetLang = tgtLang
	s.segMu.Unlock()

	s.writeSubtitle(map[string]any{
		"type":       "subtitle",
		"segment_id": segID,
		"source":     source,
		"target":     target,
		"status":     "final",
		"start_time": start,
		"end_time":   end,
		"corrected":  true,
	})
	s.enqueueTTS(segID, target, tgtLang)
}

func (s *session) snapshotPartialTarget(segID string) string {
	s.partMu.Lock()
	defer s.partMu.Unlock()
	ps := s.partState[segID]
	if ps == nil {
		return ""
	}
	if t := strings.TrimSpace(ps.lastTarget); t != "" {
		return t
	}
	return strings.TrimSpace(ps.lastPushedTarget)
}

func (s *session) waitPartialTarget(segID string, maxWait time.Duration) string {
	deadline := time.Now().Add(maxWait)
	for {
		if t := s.snapshotPartialTarget(segID); t != "" {
			return t
		}
		s.partMu.Lock()
		inflight := s.partState[segID] != nil && s.partState[segID].inflight
		s.partMu.Unlock()
		if !inflight || time.Now().After(deadline) {
			return s.snapshotPartialTarget(segID)
		}
		time.Sleep(5 * time.Millisecond)
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

// resolveTargetText 生成字幕译文:目标语言与原文一致时直通 ASR 文本,
// 否则调用 LLM 翻译,确保「目标语言是什么,字幕就是什么」。
func (s *session) resolveTargetText(source string, history []translate.Pair) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil
	}
	srcLang, tgtLang := s.languages()
	if translate.ShouldPassthrough(source, srcLang, tgtLang) {
		return source, nil
	}
	return s.tr.Translate(s.ctx, source, history, tgtLang, srcLang)
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
	if st.translated && !st.revised {
		s.segMu.Unlock()
		return
	}
	source, seq, start, end := st.source, st.seq, st.startTime, st.endTime
	s.segMu.Unlock()

	if strings.TrimSpace(source) == "" {
		return
	}

	gen := s.currentLangGen()
	_, tgtLang := s.languages()
	target, err := s.resolveTargetText(source, s.buildContext(seq))
	if err != nil {
		s.log.Warn("translate failed", slog.String("seg", segID), slog.String("err", err.Error()))
		s.writeJSON(map[string]any{"type": "translate_error", "segment_id": segID, "message": err.Error()})
		return
	}
	if target == "" {
		return
	}
	if gen != s.currentLangGen() {
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

	s.writeSubtitle(map[string]any{
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

	// 句子边界:一句刚定稿,立刻触发一次复审,让"用后文纠正前文"的修正尽早闪现,
	// 而不是干等下一个定时器整点。
	s.triggerReview()
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

// reviewLoop 触发 LLM 复审纠错,直到会话结束。两种触发源:
//   - 句子边界:每当一句定稿翻译完成就立刻复审一次(reviewSignal),
//     让"用后文纠正前文"的修正尽快出现,不再干等定时器;
//   - 定时兜底:每隔 ReviewInterval 再扫一次,覆盖久无新句但仍可优化的情况。
//
// reviewRecent 内部有窗口指纹去重(lastReviewKey),内容没变不会真正调用 LLM,
// 而本 loop 是单 goroutine 串行执行,LLM 在途时新信号在缓冲通道里自动合并,
// 不会刷爆接口。
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
		case <-s.reviewSignal:
			s.reviewRecent()
		}
	}
}

// triggerReview 在句子边界请求一次尽快的复审(非阻塞;已有待处理信号则合并)。
func (s *session) triggerReview() {
	if !config.Correction.EnablePeriodicReview {
		return
	}
	select {
	case s.reviewSignal <- struct{}{}:
	default: // 已有待处理信号,合并即可
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
	revised, err := s.tr.ReviewDetailed(s.ctx, items, tgtLang)
	if err != nil {
		s.log.Debug("review failed", slog.String("err", err.Error()))
		return
	}
	if len(revised) != len(ids) {
		return
	}

	// 3) 仅覆盖「模型确实改了、置信度达标、且自快照以来未被更新过」的 segment。
	//    置信度门槛杜绝过度纠错:把本来正确的句子改成另一种同义说法只会让字幕闪烁。
	minConf := config.Correction.ReviewMinConfidence
	gateOnConfidence := config.Correction.OverwriteOnlyIfMoreConfident
	for i, id := range ids {
		rev := revised[i]
		newTarget := strings.TrimSpace(rev.Target)
		// 模型没标记改动、译文为空、或与旧译文一致 -> 跳过(不触发前端纠错高亮)。
		if !rev.Changed || newTarget == "" || newTarget == tgtSnap[i] {
			continue
		}
		// 置信度门槛:开启后,低于阈值的修订一律不采纳(宁可不改也不乱改)。
		if gateOnConfidence && rev.Confidence < minConf {
			s.log.Debug("review skipped: low confidence",
				slog.String("seg", id), slog.Float64("confidence", rev.Confidence))
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

		s.writeSubtitle(map[string]any{
			"type":       "subtitle",
			"segment_id": id,
			"source":     source,
			"target":     newTarget,
			"status":     "final",
			"start_time": start,
			"end_time":   end,
			"corrected":  true,
		})
		s.log.Debug("review corrected",
			slog.String("seg", id),
			slog.Float64("confidence", rev.Confidence),
			slog.String("reason", rev.Reason))
		s.enqueueTTS(id, newTarget, tgtLang)
	}
}

func (s *session) onASRError(err error) {
	s.log.Warn("asr error", slog.String("err", err.Error()))
	s.writeJSON(map[string]any{"type": "asr_error", "message": err.Error()})
}
