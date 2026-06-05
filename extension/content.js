// content.js —— 在页面中注入双语字幕 overlay。
//
// 由 background.js 在「开始翻译」时通过 chrome.scripting 主动注入,
// 因此可能被重复注入;下面的守卫保证只初始化一次(幂等)。
//
// 里程碑 5:双语字幕 UI。
//   - partial(中间结果)灰显斜体,final(定稿)正常显示;
//   - 同一 segment_id 原地更新(partial→final、译文异步回填都不新增行);
//   - final 但译文未到时显示「翻译中…」占位,回填后替换;
//   - 字幕按 start_time 时间顺序排列,只保留最近若干行,旧行自动淘汰;
//   - 处理 translate_error:保留原文并提示翻译失败。

(() => {
  if (window.__simulInterpreterInjected) return;
  window.__simulInterpreterInjected = true;

  const OVERLAY_ID = "__simul_interpreter_overlay__";
  const MAX_LINES = 4; // 字幕条最多同时显示的行数(旧行淘汰)
  const CORRECTED_HIGHLIGHT_MS = 4000; // 纠错高亮持续时长

  function ensureOverlay() {
    let el = document.getElementById(OVERLAY_ID);
    if (el) return el;
    el = document.createElement("div");
    el.id = OVERLAY_ID;
    el.innerHTML = `<div class="si-status"></div><div class="si-subtitle-list"></div>`;
    document.documentElement.appendChild(el);
    return el;
  }

  // setStatus 在字幕条顶部显示一行状态(注入确认 / 等待语音),收到字幕后自动隐藏。
  function setStatus(text) {
    const overlay = ensureOverlay();
    const el = overlay.querySelector(".si-status");
    el.textContent = text || "";
    el.style.display = text ? "block" : "none";
  }

  // 注入成功后立即给出可见反馈,便于确认内容脚本已就位。
  setStatus("🟢 同声传译已就绪,等待语音…");

  // segments: segment_id -> { segment_id, source, target, status, start_time, end_time, error }
  const segments = new Map();

  // upsertSegment 合并增量:后端每条 subtitle 都带 source,
  // 但 ASR 定稿先到(target 为空)、译文随后异步回填。
  // 当新消息的 target 为空且原文未变时,保留已有译文,避免译文「闪掉」。
  function upsertSegment(msg) {
    const prev = segments.get(msg.segment_id);
    const next = {
      segment_id: msg.segment_id,
      source: msg.source || "",
      target: msg.target || "",
      status: msg.status || "partial",
      start_time: msg.start_time ?? prev?.start_time ?? 0,
      end_time: msg.end_time ?? prev?.end_time ?? 0,
      error: "",
      // 纠错高亮(ASR 修订重译 / 周期性复审)的时间戳,过期后自动淡出。
      correctedAt: prev?.correctedAt ?? 0,
    };
    if (!next.target && prev && prev.source === next.source && prev.target) {
      next.target = prev.target;
    }
    if (msg.corrected) {
      next.correctedAt = Date.now();
    }
    segments.set(next.segment_id, next);
  }

  function markError(segmentId, message) {
    const seg = segments.get(segmentId);
    if (!seg) return;
    seg.error = message || "翻译失败";
    segments.set(segmentId, seg);
  }

  // visibleSegments 按 start_time 升序排序,只取最近 MAX_LINES 行。
  function visibleSegments() {
    const all = [...segments.values()].sort(
      (a, b) => a.start_time - b.start_time || a.segment_id.localeCompare(b.segment_id)
    );
    return all.slice(-MAX_LINES);
  }

  function render() {
    const overlay = ensureOverlay();
    const list = overlay.querySelector(".si-subtitle-list");
    const visible = visibleSegments();
    const keep = new Set(visible.map((s) => s.segment_id));

    // 删除已滚出可视窗口的旧行。
    list.querySelectorAll(".si-seg").forEach((row) => {
      if (!keep.has(row.dataset.seg)) row.remove();
    });

    let prevRow = null;
    for (const seg of visible) {
      let row = list.querySelector(`[data-seg="${seg.segment_id}"]`);
      if (!row) {
        row = document.createElement("div");
        row.className = "si-seg";
        row.dataset.seg = seg.segment_id;
        row.innerHTML = `<div class="si-source"></div><div class="si-target"></div>`;
      }
      // 按时间顺序重新插入,保证排序正确(revision / 迟到分句也归位)。
      if (prevRow) {
        if (prevRow.nextSibling !== row) list.insertBefore(row, prevRow.nextSibling);
      } else if (list.firstChild !== row) {
        list.insertBefore(row, list.firstChild);
      }
      prevRow = row;

      const isPartial = seg.status === "partial";
      const translating = seg.status === "final" && !seg.target && !seg.error;
      const corrected = seg.correctedAt && Date.now() - seg.correctedAt < CORRECTED_HIGHLIGHT_MS;
      row.classList.toggle("si-partial", isPartial);
      row.classList.toggle("si-final", seg.status === "final");
      row.classList.toggle("si-translating", translating);
      row.classList.toggle("si-error", !!seg.error);
      row.classList.toggle("si-corrected", !!corrected);

      row.querySelector(".si-source").textContent = seg.source || "";

      const targetEl = row.querySelector(".si-target");
      if (seg.error) {
        targetEl.textContent = "⚠ 翻译失败,仅显示原文";
      } else if (seg.target) {
        targetEl.textContent = seg.target;
      } else if (translating) {
        targetEl.textContent = "翻译中…";
      } else {
        targetEl.textContent = ""; // partial 阶段暂不显示译文行
      }
    }
  }

  chrome.runtime.onMessage.addListener((msg) => {
    // channel 是路由键;msg.target 留给「译文」字段,二者不可混用。
    if (msg?.channel !== "page-subtitle") return;
    if (msg.type === "subtitle") {
      setStatus(""); // 收到第一条字幕后隐藏状态提示
      upsertSegment(msg);
      render();
      if (msg.corrected) {
        // 高亮到期后再渲染一次以淡出。
        setTimeout(render, CORRECTED_HIGHLIGHT_MS + 200);
      }
    } else if (msg.type === "translate_error") {
      markError(msg.segment_id, msg.message);
      render();
    } else if (msg.type === "clear") {
      // 停止翻译:移除整个 overlay(含状态条与字幕)。
      segments.clear();
      const el = document.getElementById(OVERLAY_ID);
      if (el) el.remove();
    }
  });
})();
