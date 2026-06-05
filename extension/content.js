// content.js —— 页面内的「桌宠控制台」+ 双语字幕 overlay。
//
// 通过 manifest 的 content_scripts 在每个页面自动注入:只要扩展开着,
// 打开任意网页桌宠就常驻在页面上。桌宠即控制台:
//   - 点击桌宠展开控制面板:选择源/目标语言、查看状态、停止翻译;
//   - 「开始翻译」受浏览器限制必须由扩展调用,故绑定快捷键 Alt+Shift+S(见 background);
//   - 拖动桌宠可移动(位置记忆);有译文时张嘴并冒气泡。
//
// 字幕 overlay(翻译进行中才出现):
//   - partial 灰显斜体,final 定稿;同一 segment_id 原地更新;译文异步回填;
//   - 纠错高亮;按时间排序,仅保留最近若干行。

(() => {
  if (window.__simulInterpreterInjected) return;
  window.__simulInterpreterInjected = true;

  const OVERLAY_ID = "__simul_interpreter_overlay__";
  const PET_ID = "__simul_interpreter_pet__";
  const MAX_LINES = 1; // 只显示最新一句译文,说完一句下一句顶上,不堆积占空间
  const AUTOHIDE_MS = 4000; // 译文出现后若 4s 内没有新句,自动消失(不再占屏)
  const CORRECTED_HIGHLIGHT_MS = 4000;

  // 当前「开始/停止」快捷键(由 background 查询 chrome.commands 回填;用户可改)。
  let currentShortcut = "Alt+Shift+S";
  function idleHint() {
    return currentShortcut
      ? `已停止 · 按 ${currentShortcut} 开始`
      : "已停止 · 未设置快捷键(点下方「更改快捷键」)";
  }

  // 语言选项(value 必须与后端识别的语言名一致;源语言空串=自动检测)。
  const SOURCE_LANGS = [
    ["", "自动检测"],
    ["英语", "英语"],
    ["中文", "中文"],
    ["日语", "日语"],
    ["韩语", "韩语"],
    ["法语", "法语"],
    ["德语", "德语"],
    ["西班牙语", "西班牙语"],
    ["俄语", "俄语"],
    ["粤语", "粤语"],
  ];
  const TARGET_LANGS = SOURCE_LANGS.filter(([v]) => v !== "");

  let running = false;

  // ── 字幕 overlay ────────────────────────────────────────────────────
  const segments = new Map();
  const dismissed = new Set(); // 已自动消失的 segment id,避免被复审更新重新唤出

  function ensureOverlay() {
    let el = document.getElementById(OVERLAY_ID);
    if (el) return el;
    el = document.createElement("div");
    el.id = OVERLAY_ID;
    el.innerHTML = `<div class="si-subtitle-list"></div>`;
    document.documentElement.appendChild(el);
    return el;
  }

  function removeOverlay() {
    const el = document.getElementById(OVERLAY_ID);
    if (el) el.remove();
    segments.clear();
    dismissed.clear();
  }

  function upsertSegment(msg) {
    if (dismissed.has(msg.segment_id)) return; // 已自动消失的句子不再唤回
    const prev = segments.get(msg.segment_id);
    const next = {
      segment_id: msg.segment_id,
      source: msg.source || "",
      target: msg.target || "",
      status: msg.status || "partial",
      start_time: msg.start_time ?? prev?.start_time ?? 0,
      end_time: msg.end_time ?? prev?.end_time ?? 0,
      error: "",
      correctedAt: prev?.correctedAt ?? 0,
      translatedAt: prev?.translatedAt ?? 0,
    };
    if (!next.target && prev && prev.source === next.source && prev.target) {
      next.target = prev.target;
    }
    // 记录译文出现/变化的时间,作为「自动消失」计时起点。
    if (next.target && next.target !== prev?.target) {
      next.translatedAt = Date.now();
    }
    if (msg.corrected) next.correctedAt = Date.now();
    segments.set(next.segment_id, next);
  }

  // 周期性清除「已出现 AUTOHIDE_MS 仍无新句顶替」的旧译文,让屏幕保持清爽。
  function sweepExpired() {
    const now = Date.now();
    let changed = false;
    for (const [id, s] of segments) {
      if (s.status === "final" && s.target && s.translatedAt && now - s.translatedAt > AUTOHIDE_MS) {
        segments.delete(id);
        dismissed.add(id);
        changed = true;
      }
    }
    if (changed) render();
  }
  setInterval(sweepExpired, 1000);

  function markError(segmentId, message) {
    const seg = segments.get(segmentId);
    if (!seg) return;
    seg.error = message || "翻译失败";
    segments.set(segmentId, seg);
  }

  function visibleSegments() {
    // 只显示目标语言字幕:仅保留已定稿(会触发翻译)的分句,
    // 过滤掉还在说话中的 partial(此时没有译文,显示出来只会是空框)。
    const all = [...segments.values()]
      .filter((s) => s.status === "final")
      .sort((a, b) => a.start_time - b.start_time || a.segment_id.localeCompare(b.segment_id));
    return all.slice(-MAX_LINES);
  }

  function render() {
    const overlay = ensureOverlay();
    const list = overlay.querySelector(".si-subtitle-list");
    const visible = visibleSegments();
    const keep = new Set(visible.map((s) => s.segment_id));

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
        targetEl.textContent = "";
      }
    }
  }

  // ── 桌宠控制台(可拖动) ─────────────────────────────────────────────
  let petTalkTimer = null;
  let petBubbleTimer = null;

  const PET_SVG = `
<svg viewBox="0 0 120 120" width="76" height="76" xmlns="http://www.w3.org/2000/svg">
  <ellipse cx="60" cy="111" rx="29" ry="6" fill="rgba(0,0,0,0.18)"/>
  <defs>
    <linearGradient id="siPetG" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#7cc6ff"/>
      <stop offset="1" stop-color="#4a8fe7"/>
    </linearGradient>
  </defs>
  <path d="M22 62 A38 38 0 0 1 98 62" fill="none" stroke="#2f3b52" stroke-width="7" stroke-linecap="round"/>
  <rect x="14" y="55" width="16" height="27" rx="7" fill="#2f3b52"/>
  <rect x="90" y="55" width="16" height="27" rx="7" fill="#2f3b52"/>
  <circle cx="60" cy="67" r="36" fill="url(#siPetG)"/>
  <ellipse cx="48" cy="63" rx="7" ry="8.5" fill="#fff"/>
  <ellipse cx="72" cy="63" rx="7" ry="8.5" fill="#fff"/>
  <circle cx="49" cy="65" r="3.6" fill="#22304a"/>
  <circle cx="73" cy="65" r="3.6" fill="#22304a"/>
  <circle cx="50.6" cy="63.4" r="1.2" fill="#fff"/>
  <circle cx="74.6" cy="63.4" r="1.2" fill="#fff"/>
  <ellipse cx="40" cy="75" rx="5" ry="3" fill="#ff9bb3" opacity="0.75"/>
  <ellipse cx="80" cy="75" rx="5" ry="3" fill="#ff9bb3" opacity="0.75"/>
  <ellipse class="si-pet-mouth" cx="60" cy="79" rx="6" ry="3.2" fill="#22304a"/>
</svg>`;

  function buildOptions(list, selected) {
    return list
      .map(([v, label]) => `<option value="${v}"${v === selected ? " selected" : ""}>${label}</option>`)
      .join("");
  }

  function ensurePet() {
    let pet = document.getElementById(PET_ID);
    if (pet) return pet;
    pet = document.createElement("div");
    pet.id = PET_ID;
    pet.innerHTML = `
      <div class="si-pet-panel">
        <div class="si-pet-title">同声传译</div>
        <div class="si-pet-status">${idleHint()}</div>
        <label class="si-pet-field">源语言
          <select class="si-pet-src">${buildOptions(SOURCE_LANGS, "")}</select>
        </label>
        <label class="si-pet-field">译为
          <select class="si-pet-tgt">${buildOptions(TARGET_LANGS, "中文")}</select>
        </label>
        <button class="si-pet-stop" type="button">停止翻译</button>
        <div class="si-pet-shortcut">开始/停止快捷键:<b class="si-pet-key">${currentShortcut || "未设置"}</b></div>
        <button class="si-pet-shortcut-btn" type="button">更改快捷键</button>
      </div>
      <div class="si-pet-bubble"></div>
      <div class="si-pet-body" title="点我展开 · 拖我移动">${PET_SVG}</div>`;
    document.documentElement.appendChild(pet);

    // 恢复语言选择。
    const srcSel = pet.querySelector(".si-pet-src");
    const tgtSel = pet.querySelector(".si-pet-tgt");
    try {
      chrome.storage?.local?.get(["sourceLang", "targetLang"], (r) => {
        if (typeof r?.sourceLang === "string") srcSel.value = r.sourceLang;
        if (r?.targetLang) tgtSel.value = r.targetLang;
      });
    } catch {}
    srcSel.addEventListener("change", () => {
      try { chrome.storage?.local?.set({ sourceLang: srcSel.value }); } catch {}
    });
    tgtSel.addEventListener("change", () => {
      try { chrome.storage?.local?.set({ targetLang: tgtSel.value }); } catch {}
    });
    // 防止下拉/按钮交互被拖动逻辑吞掉。
    pet.querySelector(".si-pet-panel").addEventListener("pointerdown", (e) => e.stopPropagation());
    pet.querySelector(".si-pet-stop").addEventListener("click", () => {
      try {
        chrome.runtime.sendMessage({ target: "background", type: "stop" });
      } catch {}
      setRunning(false);
    });
    pet.querySelector(".si-pet-shortcut-btn").addEventListener("click", () => {
      try {
        chrome.runtime.sendMessage({ target: "background", type: "openShortcuts" });
      } catch {}
    });

    restorePetPos(pet);
    makePetDraggable(pet);
    setRunning(running);
    return pet;
  }

  function setRunning(on) {
    running = !!on;
    const pet = document.getElementById(PET_ID);
    if (!pet) return;
    pet.classList.toggle("si-pet-running", running);
    const statusEl = pet.querySelector(".si-pet-status");
    if (statusEl) statusEl.textContent = running ? "翻译中…" : idleHint();
    if (!running) removeOverlay();
  }

  // 向后端查询运行状态与当前快捷键,并刷新桌宠显示。
  function refreshState() {
    try {
      chrome.runtime.sendMessage({ target: "background", type: "getState" }, (resp) => {
        if (chrome.runtime.lastError || !resp) return;
        if (typeof resp.shortcut === "string") {
          currentShortcut = resp.shortcut;
          const pet = document.getElementById(PET_ID);
          const keyEl = pet?.querySelector(".si-pet-key");
          if (keyEl) keyEl.textContent = currentShortcut || "未设置";
        }
        if (typeof resp.running === "boolean") setRunning(resp.running);
      });
    } catch {}
  }

  function makePetDraggable(pet) {
    const handle = pet.querySelector(".si-pet-body");
    let dragging = false;
    let moved = false;
    let startX = 0;
    let startY = 0;
    let originLeft = 0;
    let originTop = 0;

    handle.addEventListener("pointerdown", (e) => {
      dragging = true;
      moved = false;
      startX = e.clientX;
      startY = e.clientY;
      const rect = pet.getBoundingClientRect();
      originLeft = rect.left;
      originTop = rect.top;
      pet.classList.add("si-pet-dragging");
      try { handle.setPointerCapture(e.pointerId); } catch {}
      e.preventDefault();
    });

    handle.addEventListener("pointermove", (e) => {
      if (!dragging) return;
      const dx = e.clientX - startX;
      const dy = e.clientY - startY;
      if (Math.abs(dx) + Math.abs(dy) > 3) moved = true;
      const maxL = window.innerWidth - pet.offsetWidth;
      const maxT = window.innerHeight - pet.offsetHeight;
      const left = Math.max(0, Math.min(originLeft + dx, maxL));
      const top = Math.max(0, Math.min(originTop + dy, maxT));
      pet.style.left = left + "px";
      pet.style.top = top + "px";
      pet.style.right = "auto";
      pet.style.bottom = "auto";
    });

    const end = (e) => {
      if (!dragging) return;
      dragging = false;
      pet.classList.remove("si-pet-dragging");
      try { handle.releasePointerCapture(e.pointerId); } catch {}
      savePetPos(pet);
      if (!moved) {
        pet.classList.toggle("si-pet-open"); // 没拖动 = 点击,展开/收起面板
        if (pet.classList.contains("si-pet-open")) refreshState(); // 展开时刷新状态与快捷键
      }
    };
    handle.addEventListener("pointerup", end);
    handle.addEventListener("pointercancel", end);
  }

  function savePetPos(pet) {
    try {
      chrome.storage?.local?.set({ petPos: { left: pet.style.left, top: pet.style.top } });
    } catch {}
  }

  function restorePetPos(pet) {
    try {
      chrome.storage?.local?.get("petPos", (r) => {
        if (r && r.petPos && r.petPos.left) {
          pet.style.left = r.petPos.left;
          pet.style.top = r.petPos.top;
          pet.style.right = "auto";
          pet.style.bottom = "auto";
        }
      });
    } catch {}
  }

  function petTalk() {
    const pet = ensurePet();
    pet.classList.add("si-pet-talking");
    clearTimeout(petTalkTimer);
    petTalkTimer = setTimeout(() => pet.classList.remove("si-pet-talking"), 1600);
  }

  function petSay(text) {
    if (!text) return;
    const pet = ensurePet();
    const bubble = pet.querySelector(".si-pet-bubble");
    bubble.textContent = text;
    bubble.classList.add("si-show");
    clearTimeout(petBubbleTimer);
    petBubbleTimer = setTimeout(() => bubble.classList.remove("si-show"), 4000);
  }

  // ── 消息处理 ────────────────────────────────────────────────────────
  chrome.runtime.onMessage.addListener((msg) => {
    if (msg?.channel !== "page-subtitle") return;
    if (msg.type === "subtitle") {
      setRunning(true);
      upsertSegment(msg);
      render();
      if (msg.target) {
        petSay(msg.target);
        petTalk();
      }
      if (msg.corrected) setTimeout(render, CORRECTED_HIGHLIGHT_MS + 200);
    } else if (msg.type === "translate_error") {
      markError(msg.segment_id, msg.message);
      render();
    } else if (msg.type === "state") {
      setRunning(!!msg.running);
    } else if (msg.type === "clear") {
      setRunning(false);
    }
  });

  ensurePet();
  // 注入后查询运行状态与当前快捷键(刷新页面或补注入时恢复显示)。
  refreshState();
})();
