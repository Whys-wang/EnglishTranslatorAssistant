// content.js —— 页面内的「桌宠控制台」+ 流式字幕 overlay。
//
// 通过 manifest 的 content_scripts 在每个页面自动注入:只要扩展开着,
// 打开任意网页桌宠就常驻在页面上。桌宠即控制台:
//   - 点击桌宠展开控制面板:选择源/目标语言、开始/停止翻译、查看状态;
//   - 「开始翻译」既可点桌宠面板里的按钮,也可按快捷键 Alt+Shift+S(默认);
//   - 拖动桌宠可移动(位置记忆);有译文时张嘴提示「在说话」。
//
// 字幕 overlay —— 流式字幕条(YouTube / 系统实时字幕风格):
//   - 屏幕底部一条固定的字幕条,内容像打字机一样持续追加,永不主动消失;
//   - 数据模型:finals(已定稿句子,按出现顺序拼接) + tentative(当前 partial 译文,
//     接在末尾);ASR 修订时 finals 同 id 覆盖,自然反映纠错;
//   - 显示方式:左对齐、自然换行、固定 2 行高度 + 底部锚定竖直滚动。文字只在末尾
//     追加(已显示的字不再左右横移),旧内容向上滚出视野,底部始终停在最新两行 ——
//     避免「居中 + 按字滑动」造成的整行横移、读到一半被挤走找不到的问题;
//   - 因为永远在流动,没有「出现 → 等死 → 消失」的硬节奏,消除「字幕停留太久」
//     和「音画不同步」的别扭感;
//   - 连续说话时字幕只向上滚、不整屏消失;下一句接在底部,上一句留在滚动区上方;
//   - 仅在「停止翻译」时清空字幕;翻译进行中不因静音自动清屏;
//   - 无声音自动停止(可选):桌宠面板可开关,并自定义静音多久后自动停翻;
//   - 字幕可选中复制;上方四向箭头把手专用于拖动改位置,与选字互不干扰;
//   - 纠错高亮:被自动纠正过的句子以绿色底色长亮标记,不再淡出消失。

(() => {
  // 版本号变化时允许重新注入(否则扩展热更新后页面仍跑旧逻辑)。
  const CONTENT_SCRIPT_VERSION = 11;
  if (window.__simulInterpreterVersion === CONTENT_SCRIPT_VERSION) return;
  window.__simulInterpreterVersion = CONTENT_SCRIPT_VERSION;
  document.getElementById("__simul_interpreter_pet__")?.remove();
  document.getElementById("__simul_interpreter_overlay__")?.remove();

  const OVERLAY_ID = "__simul_interpreter_overlay__";
  const PET_ID = "__simul_interpreter_pet__";
  // 内存里最多保留多少条已定稿的译文。屏幕(底部锚定)只显示最新两行,但内存里多留
  // 几句:一是便于 ASR / LLM 复审对已滚出视野的句子做修订时仍能正确更新,二是滚动
  // 历史更连贯。超过此数丢最早的(它们早就滚出视野)。
  const FINAL_BUFFER_SIZE = 50;
  // 翻译进行中不自动清屏(字幕只滚上去、不消失);以下常量保留供将来可选功能。
  const CLEAR_AFTER_SILENCE_MS = 3000;
  // 无声音自动停止:默认关闭;时长与开关由桌宠面板设置并持久化。
  const DEFAULT_AUTO_STOP_SILENCE_ENABLED = false;
  const DEFAULT_AUTO_STOP_SILENCE_SEC = 60;
  const MIN_AUTO_STOP_SILENCE_SEC = 10;
  const MAX_AUTO_STOP_SILENCE_SEC = 600;
  // 纠正之后额外保留这么久的「安静可读」时间,期间不触发静音清屏。
  const READ_HOLD_AFTER_CORRECTION_MS = 2200;

  // 字幕颜色预设(桌宠面板里以小圆点形式排开,点击即应用)。
  // 第一项 #ffffff 是默认值。用户也可以点 color picker 选任意颜色。
  const CAPTION_COLOR_PRESETS = [
    ["#ffffff", "白"],
    ["#ffeb3b", "黄"],
    ["#7cf06e", "绿"],
    ["#7cc6ff", "蓝"],
    ["#ffb1d6", "粉"],
  ];
  const DEFAULT_CAPTION_COLOR = "#ffffff";

  // 字幕右上角锁图标的两种形态:开锁(可拖动)/ 合锁(已锁定)。
  const LOCK_OPEN_SVG =
    '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="11" width="16" height="9" rx="2"/><path d="M8 11V7a4 4 0 0 1 7.5-1.8"/></svg>';
  const LOCK_CLOSED_SVG =
    '<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><rect x="4" y="11" width="16" height="9" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/></svg>';
  // 字幕上方拖动手柄:四向箭头(仅拖此图标可移动位置)。
  const MOVE_HANDLE_SVG =
    '<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 5V2M12 19v3M5 12H2M22 12h-3"/><path d="m12 5-3-3 3-3 3 3-3 3z"/><path d="m12 19-3 3 3 3 3-3-3-3z"/><path d="m5 12-3-3 3 3 3 3-3-3z"/><path d="m19 12 3-3-3 3-3 3 3-3z"/></svg>';

  // 当前「开始/停止」快捷键(由 background 查询 chrome.commands 回填;用户可改)。
  let currentShortcut = "Alt+Shift+S";
  function idleHint() {
    return currentShortcut
      ? `已停止 · 按 ${currentShortcut} 开始`
      : "已停止 · 未设置快捷键(点下方「更改快捷键」)";
  }

  // 把启动失败的原始错误(多为英文)翻译成友好的中文引导。
  // 最常见的是 Chrome 的安全限制:抓取标签页音频必须由「点扩展图标 / 按快捷键」
  // 这种对扩展本身的操作来触发,页面内按钮首次点击会报 activeTab 未授权。
  function friendlyStartError(raw) {
    const s = String(raw || "");
    if (/has not been invoked|activeTab|user gesture|invoked for the current/i.test(s)) {
      const key = currentShortcut ? `(或按 ${currentShortcut})` : "";
      return `首次在本页使用,请先点一下浏览器右上角的扩展图标${key}授权本页;点图标会直接开始翻译,之后这个按钮也能用了。`;
    }
    if (/cannot be captured|chrome:\/\/|chrome pages|extension gallery|chromewebstore/i.test(s)) {
      return "此页面无法捕获音频(Chrome 内部页 / 商店页 / 新标签页等)。请在普通网页(如视频、播客页)上使用。";
    }
    return "启动失败:" + s;
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

  // ── 流式字幕 overlay ────────────────────────────────────────────────
  // finals:按"首次出现"的顺序保存已定稿的句子。Map 自带插入顺序,既能拼接,
  //   又允许 ASR / LLM 复审用同 id 原地覆盖。超过 FINAL_BUFFER_SIZE 时丢弃最早的。
  // tentative:当前说话中的句子(partial)的最新译文。同 id 的 final 一到就清掉,
  //   避免重复拼接。
  const finals = new Map();
  const tentative = { segId: null, target: "" };

  // 静音清屏用:最近一次「识别到内容」的时间戳;以及被清屏的句子 id 集合
  // (清屏后这些句子的迟到更新/复审纠错不再把字幕条唤回,避免老句子突然闪现)。
  let lastActivityAt = 0;
  const clearedIds = new Set();

  // 纠错长亮:曾被自动纠正过的 segment id,绿色底色一直保留到清屏/停翻。
  // lastCorrectionShownAt:最近一次纠正时间,用于纠正后暂缓静音清屏。
  const correctedIds = new Set();
  let lastCorrectionShownAt = 0;

  // 是否「粘底」:true=自动停在最新两行(默认);用户向上滚动看历史时变 false,
  // 此时不再自动拽回底部、也暂停静音清屏,滚回底部后恢复。
  let stickToBottom = true;

  // 音频 VAD:进入静音时记下时间戳;恢复有声则清零。用于「无声音自动停翻」。
  let audioSilenceStartedAt = 0;
  let autoStopSilenceEnabled = DEFAULT_AUTO_STOP_SILENCE_ENABLED;
  let autoStopSilenceSec = DEFAULT_AUTO_STOP_SILENCE_SEC;

  // 用户正在拖选字幕时暂缓 render,避免 DOM 重建把选区冲掉。
  let selectionDeferredRender = false;

  // 字幕外观偏好:用户拖到哪 / 颜色选的什么,都会持久化到 chrome.storage.local。
  // captionPos 为 null 时表示用默认「底部居中」(由 CSS 控制)。
  let captionPos = null; // {left:number, top:number} 或 null
  let captionColor = DEFAULT_CAPTION_COLOR;
  let captionLocked = false; // 锁定后字幕不可拖动(右上角锁图标切换,持久化)

  function buildOverlayHTML() {
    return `<div class="si-caption-stack"><button class="si-caption-move" type="button" aria-label="拖动字幕" title="拖此处移动字幕">${MOVE_HANDLE_SVG}</button><div class="si-caption"><span class="si-committed"></span><span class="si-tentative"></span></div></div><button class="si-caption-lock" type="button"></button>`;
  }

  // 把旧版字幕 DOM 迁移为「上方箭头把手 + 字幕条」结构。
  function migrateOverlayDOM(el) {
    if (!el) return;
    if (!el.querySelector(".si-caption-stack") || el.querySelector(".si-caption-shell")) {
      el.innerHTML = buildOverlayHTML();
    } else if (!el.querySelector(".si-caption-move")) {
      const stack = el.querySelector(".si-caption-stack");
      const cap = el.querySelector(".si-caption");
      if (stack && cap) {
        const btn = document.createElement("button");
        btn.className = "si-caption-move";
        btn.type = "button";
        btn.title = "拖此处移动字幕";
        btn.setAttribute("aria-label", "拖动字幕");
        btn.innerHTML = MOVE_HANDLE_SVG;
        stack.insertBefore(btn, cap);
      }
    }
    delete el.dataset.siDragBound;
    delete el.dataset.siScrollBound;
    delete el.dataset.siLockBound;
  }

  function bindOverlayInteractions(el) {
    if (!el || el.dataset.siDragBound === "1") return;
    makeCaptionDraggable(el);
    el.dataset.siDragBound = "1";

    const captionEl = el.querySelector(".si-caption");
    if (captionEl && el.dataset.siScrollBound !== "1") {
      captionEl.addEventListener("scroll", () => {
        stickToBottom =
          captionEl.scrollHeight - captionEl.scrollTop - captionEl.clientHeight < 24;
      });
      el.dataset.siScrollBound = "1";
    }

    const lockBtn = el.querySelector(".si-caption-lock");
    if (lockBtn && el.dataset.siLockBound !== "1") {
      lockBtn.addEventListener("pointerdown", (e) => e.stopPropagation());
      lockBtn.addEventListener("click", (e) => {
        e.stopPropagation();
        captionLocked = !captionLocked;
        try { chrome.storage?.local?.set({ captionLocked }); } catch {}
        applyCaptionLock(el);
      });
      el.dataset.siLockBound = "1";
    }
  }

  function ensureOverlay() {
    let el = document.getElementById(OVERLAY_ID);
    if (el) {
      if (!el.querySelector(".si-caption-stack") || el.querySelector(".si-caption-shell")) {
        migrateOverlayDOM(el);
      }
      bindOverlayInteractions(el);
      return el;
    }
    el = document.createElement("div");
    el.id = OVERLAY_ID;
    el.innerHTML = buildOverlayHTML();
    document.documentElement.appendChild(el);
    applyCaptionPos(el);
    applyCaptionColor(el);
    applyCaptionLock(el);
    bindOverlayInteractions(el);
    return el;
  }

  function clearCaptionState() {
    finals.clear();
    tentative.segId = null;
    tentative.target = "";
    lastCommittedSnapshot = "";
    lastActivityAt = 0;
    clearedIds.clear();
    correctedIds.clear();
    lastCorrectionShownAt = 0;
    stickToBottom = true;
  }

  // 切换源/目标语言时清空当前字幕缓存,避免旧语言译文残留或闪一下别的语言。
  function resetCaptionsForLangChange() {
    clearCaptionState();
    render(true);
  }

  function removeOverlay() {
    const el = document.getElementById(OVERLAY_ID);
    if (el) el.remove();
    clearCaptionState();
  }

  // 应用「字幕条位置」到 overlay 元素:
  //   - 未设置(captionPos==null):用 CSS 默认的底部居中(撤掉 inline style);
  //   - 已设置:打 si-positioned 类、写 left/top inline style。
  function applyCaptionPos(overlay) {
    if (!overlay) return;
    if (captionPos && typeof captionPos.left === "number" && typeof captionPos.top === "number") {
      overlay.classList.add("si-positioned");
      overlay.style.left = captionPos.left + "px";
      overlay.style.top = captionPos.top + "px";
      overlay.style.right = "auto";
      overlay.style.bottom = "auto";
      overlay.style.transform = "none";
    } else {
      overlay.classList.remove("si-positioned");
      overlay.style.left = "";
      overlay.style.top = "";
      overlay.style.right = "";
      overlay.style.bottom = "";
      overlay.style.transform = "";
    }
  }

  // 应用「字幕颜色」到 overlay:写到 CSS 变量 --si-caption-color 上,
  // .si-committed / .si-tentative / 末尾光标都通过 currentColor 继承。
  function applyCaptionColor(overlay) {
    if (!overlay) return;
    overlay.style.setProperty("--si-caption-color", captionColor || DEFAULT_CAPTION_COLOR);
  }

  // 应用锁定状态:锁定时隐藏上方拖动手柄,仅保留文字选区。
  function applyCaptionLock(overlay) {
    if (!overlay) return;
    const btn = overlay.querySelector(".si-caption-lock");
    const stack = overlay.querySelector(".si-caption-stack");
    if (btn) {
      btn.innerHTML = captionLocked ? LOCK_CLOSED_SVG : LOCK_OPEN_SVG;
      btn.classList.toggle("si-locked-on", captionLocked);
      btn.title = captionLocked
        ? "字幕已锁定 · 文字可选中复制 · 点击解锁后可拖动位置"
        : "拖上方箭头移动 · 文字区可选中复制 · 点击锁定";
    }
    if (stack) stack.classList.toggle("si-locked", captionLocked);
  }

  // 重置字幕位置到默认(底部居中),并清除 storage 中保存的位置。
  function resetCaptionPos() {
    captionPos = null;
    try {
      chrome.storage?.local?.remove?.("captionPos");
    } catch {}
    const overlay = document.getElementById(OVERLAY_ID);
    if (overlay) applyCaptionPos(overlay);
  }

  // 从 chrome.storage 恢复字幕外观偏好(位置 + 颜色),并刷新 UI。
  function restoreCaptionPrefs() {
    try {
      chrome.storage?.local?.get(["captionPos", "captionColor", "captionLocked"], (r) => {
        if (
          r?.captionPos &&
          typeof r.captionPos.left === "number" &&
          typeof r.captionPos.top === "number"
        ) {
          captionPos = { left: r.captionPos.left, top: r.captionPos.top };
        }
        if (typeof r?.captionColor === "string" && /^#[0-9a-fA-F]{6}$/.test(r.captionColor)) {
          captionColor = r.captionColor;
        }
        if (typeof r?.captionLocked === "boolean") {
          captionLocked = r.captionLocked;
        }
        const overlay = document.getElementById(OVERLAY_ID);
        if (overlay) {
          applyCaptionPos(overlay);
          applyCaptionColor(overlay);
          applyCaptionLock(overlay);
        }
        syncPetColorUI();
      });
    } catch {}
  }

  // 把当前 captionColor 同步到桌宠面板里的颜色选择 UI(预设高亮 + picker 值)。
  function syncPetColorUI() {
    const pet = document.getElementById(PET_ID);
    if (!pet) return;
    const picker = pet.querySelector(".si-pet-color-picker");
    if (picker) picker.value = captionColor;
    pet.querySelectorAll(".si-pet-color-preset").forEach((btn) => {
      btn.classList.toggle(
        "si-active",
        (btn.dataset.color || "").toLowerCase() === captionColor.toLowerCase()
      );
    });
  }

  // 用户是否正在字幕区域内拖选文字(有非折叠选区)。
  function hasCaptionSelection() {
    const sel = window.getSelection();
    if (!sel || sel.rangeCount === 0 || sel.isCollapsed) return false;
    const caption = document.getElementById(OVERLAY_ID)?.querySelector(".si-caption");
    if (!caption) return false;
    const node = sel.anchorNode;
    return !!(node && caption.contains(node));
  }

  // 仅拖字幕上方的四向箭头把手可改位置;字幕文字区留给选中复制。
  function makeCaptionDraggable(overlay) {
    const handle = overlay.querySelector(".si-caption-move");
    const stack = overlay.querySelector(".si-caption-stack");
    if (!handle) return;
    let dragging = false;
    let startX = 0;
    let startY = 0;
    let originLeft = 0;
    let originTop = 0;

    handle.addEventListener("pointerdown", (e) => {
      if (captionLocked || e.button !== 0) return;
      dragging = true;
      startX = e.clientX;
      startY = e.clientY;
      const rect = overlay.getBoundingClientRect();
      originLeft = rect.left;
      originTop = rect.top;
      stack?.classList.add("si-dragging");
      try { handle.setPointerCapture(e.pointerId); } catch {}
      e.preventDefault();
      e.stopPropagation();
    });

    handle.addEventListener("pointermove", (e) => {
      if (!dragging) return;
      const dx = e.clientX - startX;
      const dy = e.clientY - startY;
      const maxL = Math.max(0, window.innerWidth - overlay.offsetWidth);
      const maxT = Math.max(0, window.innerHeight - overlay.offsetHeight);
      captionPos = {
        left: Math.max(0, Math.min(maxL, originLeft + dx)),
        top: Math.max(0, Math.min(maxT, originTop + dy)),
      };
      applyCaptionPos(overlay);
    });

    const end = (e) => {
      if (!dragging) return;
      dragging = false;
      stack?.classList.remove("si-dragging");
      try { handle.releasePointerCapture(e.pointerId); } catch {}
      if (!captionPos) return;
      try {
        chrome.storage?.local?.set({ captionPos });
      } catch {}
    };
    handle.addEventListener("pointerup", end);
    handle.addEventListener("pointercancel", end);
  }

  // 把后端推来的一条 subtitle 事件吸收到 finals / tentative 里。
  function ingestSubtitle(msg) {
    if (!msg.segment_id) return;
    // 这条句子已经因静音被清屏。普通的迟到 partial / final 一律忽略,
    // 否则刚清空的字幕条又会被老内容唤回。但「纠错」是个例外:被清掉的句子
    // 若收到自动纠正,要把它重新放回屏幕,让用户能看清改成了什么(纠错优先)。
    const isCorrection = msg.corrected === true && msg.status === "final";
    if (clearedIds.has(msg.segment_id)) {
      if (!isCorrection) return;
      clearedIds.delete(msg.segment_id);
    }
    lastActivityAt = Date.now();
    if (msg.status === "final") {
      // 定稿:有译文就写入 finals(同 id 自然覆盖,支持 ASR 修订 + LLM 复审纠错)。
      // 没有译文(翻译失败)时,如果 tentative 上有同 id 的 partial 预览,就把
      // 这次预览升级为 final,避免「partial 显示过 -> final 翻译失败 -> 译文丢失」。
      let target = msg.target || "";
      if (!target && tentative.segId === msg.segment_id && tentative.target) {
        target = tentative.target;
      }
      if (target) {
        finals.set(msg.segment_id, target);
      }
      // 同 id 的 partial 已被定稿吸收,清掉 tentative。
      if (tentative.segId === msg.segment_id) {
        tentative.segId = null;
        tentative.target = "";
      }
      // 内存上限:历史 final 太多就丢最早的(它们早就滚出屏幕,看不到)。
      while (finals.size > FINAL_BUFFER_SIZE) {
        const firstKey = finals.keys().next().value;
        finals.delete(firstKey);
      }
    } else if (msg.target) {
      // partial:只刷 tentative,不写 finals(避免把不稳定文本永久写入)。
      tentative.segId = msg.segment_id;
      tentative.target = msg.target;
    }
  }

  // 取当前字幕流的有序分段:committed(已定稿,按出现顺序)+ tentative(当前 partial)。
  // 每段保留 segment_id,这样才能把「刚被纠正的那一段」单独高亮。
  function buildPieces() {
    const pieces = [];
    for (const [id, txt] of finals) {
      if (txt) pieces.push({ id, text: txt, tentative: false });
    }
    if (tentative.target && (!tentative.segId || !finals.has(tentative.segId))) {
      pieces.push({ id: tentative.segId, text: tentative.target, tentative: true });
    }
    return pieces;
  }

  // 已定稿快照:partial 更新时只动末尾预览行,已定稿行(含纠错绿底)不重绘。
  let lastCommittedSnapshot = "";

  function committedSnapshot() {
    let k = "";
    for (const [id, txt] of finals) k += id + "\x1f" + txt + "\x1e";
    return k;
  }

  // 仅更新正在说的 preview 行;已定稿句保持不动(滚动 + 纠错常亮不丢)。
  function renderTentative() {
    if (hasCaptionSelection()) {
      selectionDeferredRender = true;
      return;
    }
    const snap = committedSnapshot();
    if (snap !== lastCommittedSnapshot) {
      render(true);
      return;
    }
    const overlay = ensureOverlay();
    const caption = overlay.querySelector(".si-caption");
    if (!caption) return;

    let wrap = caption.querySelector(".si-tentative-line");
    if (!tentative.target || (tentative.segId && finals.has(tentative.segId))) {
      wrap?.remove();
      overlay.classList.toggle("si-empty", finals.size === 0 && !running);
      if (stickToBottom) caption.scrollTop = caption.scrollHeight;
      return;
    }

    if (!wrap) {
      wrap = document.createElement("div");
      wrap.className = "si-tentative-line si-line";
      const span = document.createElement("span");
      span.className = "si-tentative";
      wrap.appendChild(span);
      caption.appendChild(wrap);
    }
    const span = wrap.querySelector(".si-tentative");
    if (span.textContent === tentative.target) {
      if (stickToBottom) caption.scrollTop = caption.scrollHeight;
      return;
    }
    span.textContent = tentative.target;
    overlay.classList.remove("si-empty");
    if (stickToBottom) caption.scrollTop = caption.scrollHeight;
  }

  // 渲染字幕:左对齐 + 底部锚定的竖直滚动(参考 YouTube / 系统实时字幕)。
  // 关键改动:不再「按末尾 N 字滑动 + 居中」——那会让整行每来一个字就重新居中、
  // 向左横移,导致正在读的内容一直在跑、读不完就被挤走。现在文字左对齐、自然换行、
  // 只在末尾追加(已显示的文字不再左右横移),旧内容向上滚出视野,
  // 底部始终停在最新两行,阅读位置稳定。
  function render(force) {
    if (!force && hasCaptionSelection()) {
      selectionDeferredRender = true;
      return;
    }
    selectionDeferredRender = false;
    const overlay = ensureOverlay();
    const caption = overlay.querySelector(".si-caption");
    if (!caption) return;
    // 记录重建前的滚动位置:用户在看历史(未粘底)时,重建后要还原,不能跳。
    const prevScrollTop = caption.scrollTop;

    const pieces = buildPieces();

    // 按句分行重建:每句定稿一行,向上滚;当前 partial 接在最后一行。
    caption.textContent = "";
    pieces.forEach((it) => {
      const span = document.createElement("span");
      span.className = it.tentative ? "si-tentative" : "si-committed";
      span.textContent = it.text;
      if (!it.tentative && correctedIds.has(it.id)) {
        span.classList.add("si-corrected");
      }
      if (it.tentative) {
        caption.appendChild(span);
      } else {
        const line = document.createElement("div");
        line.className = "si-line";
        line.appendChild(span);
        caption.appendChild(line);
      }
    });

    // 翻译进行中即使 momentarily 无新字也不隐藏字幕条(避免「闪没再出现」)。
    overlay.classList.toggle("si-empty", pieces.length === 0 && !running);
    // 粘底时停在最底显示最新两行;用户在看历史(未粘底)时,还原其滚动位置,
    // 不被新字幕拽回底部(内容是向末尾追加的,顶部稳定,还原位置即可保持视图)。
    if (stickToBottom) {
      caption.scrollTop = caption.scrollHeight;
    } else {
      caption.scrollTop = prevScrollTop;
    }
    lastCommittedSnapshot = committedSnapshot();
  }

  // 标记某段曾被自动纠正(ASR 修订 / Pro 精修 / LLM 复审),绿色长亮直到停翻。
  function markCorrected(segId) {
    if (!segId) return;
    correctedIds.add(segId);
    lastCorrectionShownAt = Date.now();
  }

  // 翻译进行中不清屏:字幕只向上滚,下一句接在底部。仅停止翻译时 removeOverlay 清空。
  function clearCaptionOnSilence() {
    if (running) return;
    if (finals.size === 0 && !tentative.target) return;
    // 用户正在向上滚动看历史译文:暂停清屏,免得正读着就被清空。
    if (!stickToBottom) return;
    const now = Date.now();
    if (!lastActivityAt || now - lastActivityAt < CLEAR_AFTER_SILENCE_MS) return;
    // 刚有过纠正:在一段时间内不清屏,避免纠正标记刚出现就被清掉。
    if (lastCorrectionShownAt && now - lastCorrectionShownAt < READ_HOLD_AFTER_CORRECTION_MS) {
      return;
    }
    clearedIds.clear();
    for (const id of finals.keys()) clearedIds.add(id);
    correctedIds.clear();
    if (tentative.segId) clearedIds.add(tentative.segId);
    finals.clear();
    tentative.segId = null;
    tentative.target = "";
    render();
  }
  setInterval(clearCaptionOnSilence, 500);

  // 仅当页面音频「进入静音」时才开始计时;一旦恢复有声立刻清零。
  // 计时起点绝不是「开始翻译」的时刻,而是「真的没声音了」之后。
  function onAudioVAD(event) {
    if (event === "silence") {
      audioSilenceStartedAt = Date.now();
    } else if (event === "speech") {
      audioSilenceStartedAt = 0;
    }
  }

  function clampAutoStopSec(sec) {
    const n = Math.round(Number(sec));
    if (!Number.isFinite(n)) return DEFAULT_AUTO_STOP_SILENCE_SEC;
    return Math.max(MIN_AUTO_STOP_SILENCE_SEC, Math.min(MAX_AUTO_STOP_SILENCE_SEC, n));
  }

  function syncPetSilenceStopUI() {
    const pet = document.getElementById(PET_ID);
    if (!pet) return;
    const chk = pet.querySelector(".si-pet-auto-stop-enable");
    const inp = pet.querySelector(".si-pet-silence-sec");
    const row = pet.querySelector(".si-pet-silence-row");
    if (chk) chk.checked = autoStopSilenceEnabled;
    if (inp) {
      inp.value = String(autoStopSilenceSec);
      inp.disabled = !autoStopSilenceEnabled;
    }
    if (row) row.classList.toggle("si-disabled", !autoStopSilenceEnabled);
  }

  function saveSilenceStopPrefs() {
    try {
      chrome.storage?.local?.set({ autoStopSilenceEnabled, autoStopSilenceSec });
    } catch {}
  }

  function restoreSilenceStopPrefs() {
    try {
      chrome.storage?.local?.get(["autoStopSilenceEnabled", "autoStopSilenceSec"], (r) => {
        if (typeof r?.autoStopSilenceEnabled === "boolean") {
          autoStopSilenceEnabled = r.autoStopSilenceEnabled;
        }
        if (typeof r?.autoStopSilenceSec === "number") {
          autoStopSilenceSec = clampAutoStopSec(r.autoStopSilenceSec);
        }
        syncPetSilenceStopUI();
      });
    } catch {}
  }

  // 静音已持续达到用户设定秒数 -> 自动停翻(需桌宠面板开启)。
  function autoStopOnAudioSilence() {
    if (!autoStopSilenceEnabled) return;
    if (!running || !audioSilenceStartedAt) return;
    const silentForMs = Date.now() - audioSilenceStartedAt;
    if (silentForMs < autoStopSilenceSec * 1000) return;
    audioSilenceStartedAt = 0;
    try {
      chrome.runtime.sendMessage({ target: "background", type: "stop" });
    } catch {}
    setRunning(false);
    const pet = document.getElementById(PET_ID);
    const statusEl = pet?.querySelector(".si-pet-status");
    if (statusEl) statusEl.textContent = `已自动停止(静音持续 ${autoStopSilenceSec} 秒)`;
  }
  setInterval(autoStopOnAudioSilence, 1000);

  // 选区消失后补一次被推迟的 render。
  document.addEventListener("selectionchange", () => {
    if (selectionDeferredRender && !hasCaptionSelection()) render();
  });

  // ── 桌宠控制台(可拖动) ─────────────────────────────────────────────
  let petTalkTimer = null;

  const PET_SVG = `
<svg viewBox="0 0 120 120" width="76" height="76" xmlns="http://www.w3.org/2000/svg">
  <defs>
    <linearGradient id="siBody" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#9bd9ff"/>
      <stop offset="0.55" stop-color="#5aa9f5"/>
      <stop offset="1" stop-color="#3f7fe0"/>
    </linearGradient>
    <radialGradient id="siFace" cx="50%" cy="40%" r="62%">
      <stop offset="0" stop-color="#f2faff"/>
      <stop offset="1" stop-color="#d2eaff"/>
    </radialGradient>
    <radialGradient id="siGlow" cx="50%" cy="50%" r="50%">
      <stop offset="0" stop-color="#c8f6ff"/>
      <stop offset="1" stop-color="#5fd6ef" stop-opacity="0"/>
    </radialGradient>
    <linearGradient id="siCup" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="#3a4a66"/>
      <stop offset="1" stop-color="#26334a"/>
    </linearGradient>
  </defs>

  <ellipse cx="60" cy="113" rx="30" ry="5.5" fill="rgba(0,0,0,0.18)"/>

  <!-- 头顶小天线(发亮的「信号」点,呼应实时传译) -->
  <line x1="60" y1="23" x2="60" y2="10" stroke="#3f7fe0" stroke-width="3" stroke-linecap="round"/>
  <circle cx="60" cy="8.5" r="6.5" fill="url(#siGlow)"/>
  <circle cx="60" cy="8.5" r="3.4" fill="#7cf0ff"/>

  <!-- 两只小手臂(藏在身体两侧探出来) -->
  <ellipse cx="17" cy="80" rx="8" ry="10.5" fill="#4a8fe7" transform="rotate(-18 17 80)"/>
  <ellipse cx="103" cy="80" rx="8" ry="10.5" fill="#4a8fe7" transform="rotate(18 103 80)"/>

  <!-- 身体 + 光泽高光 -->
  <ellipse cx="60" cy="65" rx="41" ry="42" fill="url(#siBody)"/>
  <ellipse cx="43" cy="44" rx="14" ry="9.5" fill="#ffffff" opacity="0.30"/>

  <!-- 脸盘 -->
  <ellipse cx="60" cy="63" rx="30" ry="28" fill="url(#siFace)"/>

  <!-- 耳机:头梁 + 两侧耳罩 -->
  <path d="M19 58 A41 41 0 0 1 101 58" fill="none" stroke="#2c3a55" stroke-width="7.5" stroke-linecap="round"/>
  <rect x="8" y="50" width="18" height="32" rx="9" fill="url(#siCup)"/>
  <rect x="94" y="50" width="18" height="32" rx="9" fill="url(#siCup)"/>
  <rect x="12.5" y="57" width="9" height="18" rx="4.5" fill="#5fd6ef" opacity="0.85"/>
  <rect x="98.5" y="57" width="9" height="18" rx="4.5" fill="#5fd6ef" opacity="0.85"/>

  <!-- 眉毛 -->
  <path d="M40 49 q7 -4.5 14 0" fill="none" stroke="#2c3a55" stroke-width="2.4" stroke-linecap="round"/>
  <path d="M66 49 q7 -4.5 14 0" fill="none" stroke="#2c3a55" stroke-width="2.4" stroke-linecap="round"/>

  <!-- 水汪汪大眼睛 + 高光 -->
  <ellipse cx="49" cy="62" rx="7.8" ry="9.8" fill="#fff"/>
  <ellipse cx="71" cy="62" rx="7.8" ry="9.8" fill="#fff"/>
  <circle cx="50" cy="64" r="4.4" fill="#22304a"/>
  <circle cx="72" cy="64" r="4.4" fill="#22304a"/>
  <circle cx="52" cy="62" r="1.6" fill="#fff"/>
  <circle cx="74" cy="62" r="1.6" fill="#fff"/>
  <circle cx="48.2" cy="66.4" r="0.95" fill="#fff" opacity="0.85"/>
  <circle cx="70.2" cy="66.4" r="0.95" fill="#fff" opacity="0.85"/>

  <!-- 腮红 -->
  <ellipse cx="39" cy="73" rx="5" ry="3" fill="#ff9bb3" opacity="0.78"/>
  <ellipse cx="81" cy="73" rx="5" ry="3" fill="#ff9bb3" opacity="0.78"/>

  <!-- 嘴巴(说话时由 CSS 控制一张一合) -->
  <ellipse class="si-pet-mouth" cx="60" cy="78" rx="5.5" ry="3.4" fill="#22304a"/>
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
    const colorPresetButtons = CAPTION_COLOR_PRESETS
      .map(
        ([c, label]) =>
          `<button class="si-pet-color-preset" type="button" data-color="${c}" style="background:${c}" title="${label}"></button>`
      )
      .join("");
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
        <button class="si-pet-start" type="button">开始翻译</button>
        <button class="si-pet-stop" type="button">停止翻译</button>
        <div class="si-pet-section">无声音自动停止</div>
        <label class="si-pet-check">
          <input type="checkbox" class="si-pet-auto-stop-enable" />
          <span>开启(仅统计页面无声音时长)</span>
        </label>
        <label class="si-pet-field si-pet-silence-row">
          <span>静音持续</span>
          <input type="number" class="si-pet-silence-sec" min="10" max="600" step="5" value="60" />
          <span>秒后停翻</span>
        </label>
        <div class="si-pet-section">字幕外观(拖上方箭头移动,文字可选中复制)</div>
        <div class="si-pet-color-row">
          ${colorPresetButtons}
          <input type="color" class="si-pet-color-picker" value="${captionColor}" title="自定义颜色">
        </div>
        <button class="si-pet-caption-reset" type="button">重置字幕位置</button>
        <div class="si-pet-shortcut">开始/停止快捷键:<b class="si-pet-key">${currentShortcut || "未设置"}</b></div>
        <button class="si-pet-shortcut-btn" type="button">更改快捷键</button>
      </div>
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
    // 任一下拉变更:存储 + 通知 background 热切换。这样翻译进行中改语言也能立刻生效,
    // 不用「先停再开」。后端 server.go 已支持 type:"config" 的语言热切换。
    function pushLangConfig() {
      try {
        chrome.runtime.sendMessage({
          target: "background",
          type: "config",
          sourceLang: srcSel.value,
          targetLang: tgtSel.value,
        });
      } catch {}
    }
    srcSel.addEventListener("change", () => {
      try { chrome.storage?.local?.set({ sourceLang: srcSel.value }); } catch {}
      if (running) resetCaptionsForLangChange();
      pushLangConfig();
    });
    tgtSel.addEventListener("change", () => {
      try { chrome.storage?.local?.set({ targetLang: tgtSel.value }); } catch {}
      if (running) resetCaptionsForLangChange();
      pushLangConfig();
    });
    // 防止下拉/按钮交互被拖动逻辑吞掉。
    pet.querySelector(".si-pet-panel").addEventListener("pointerdown", (e) => e.stopPropagation());
    pet.querySelector(".si-pet-start").addEventListener("click", () => {
      const statusEl = pet.querySelector(".si-pet-status");
      if (statusEl) statusEl.textContent = "正在连接…";
      try {
        chrome.runtime.sendMessage(
          {
            target: "background",
            type: "start",
            sourceLang: srcSel.value,
            targetLang: tgtSel.value,
          },
          (resp) => {
            if (chrome.runtime.lastError) {
              if (statusEl) statusEl.textContent = friendlyStartError(chrome.runtime.lastError.message);
              return;
            }
            if (resp?.ok) {
              setRunning(true);
            } else {
              if (statusEl) statusEl.textContent = friendlyStartError(resp?.error || "未知错误");
            }
          }
        );
      } catch (e) {
        if (statusEl) statusEl.textContent = friendlyStartError(String(e));
      }
    });
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

    const autoStopChk = pet.querySelector(".si-pet-auto-stop-enable");
    const silenceSecInp = pet.querySelector(".si-pet-silence-sec");
    autoStopChk.addEventListener("change", () => {
      autoStopSilenceEnabled = !!autoStopChk.checked;
      syncPetSilenceStopUI();
      saveSilenceStopPrefs();
    });
    function applySilenceSecFromInput() {
      autoStopSilenceSec = clampAutoStopSec(silenceSecInp.value);
      silenceSecInp.value = String(autoStopSilenceSec);
      saveSilenceStopPrefs();
    }
    silenceSecInp.addEventListener("change", applySilenceSecFromInput);
    silenceSecInp.addEventListener("blur", applySilenceSecFromInput);

    // 字幕颜色:预设按钮 + 自定义 color picker,改完即刻应用并持久化。
    function applyAndSaveColor(c) {
      if (!/^#[0-9a-fA-F]{6}$/.test(c)) return;
      captionColor = c;
      const overlay = document.getElementById(OVERLAY_ID);
      if (overlay) applyCaptionColor(overlay);
      try { chrome.storage?.local?.set({ captionColor }); } catch {}
      syncPetColorUI();
    }
    pet.querySelectorAll(".si-pet-color-preset").forEach((btn) => {
      btn.addEventListener("click", () => applyAndSaveColor(btn.dataset.color || DEFAULT_CAPTION_COLOR));
    });
    const colorPicker = pet.querySelector(".si-pet-color-picker");
    // input 事件:拖颜色环时实时预览;change 兜底。
    colorPicker.addEventListener("input", (e) => applyAndSaveColor(e.target.value));
    colorPicker.addEventListener("change", (e) => applyAndSaveColor(e.target.value));

    // 「重置字幕位置」按钮:把字幕条放回默认底部居中。
    pet.querySelector(".si-pet-caption-reset").addEventListener("click", () => {
      resetCaptionPos();
    });

    // 面板刚拼好,把当前 captionColor / 无声音停翻设置同步到 UI。
    syncPetColorUI();
    syncPetSilenceStopUI();
    restoreSilenceStopPrefs();

    restorePetPos(pet);
    makePetDraggable(pet);
    setRunning(running);
    // 同步先按默认 DOM 位置算一个 quadrant(storage 异步回调到之前也有可用的类)。
    updateQuadrant(pet);
    // 窗口尺寸变化时,桌宠相对屏幕的象限可能变,重算一次让面板展开方向跟上。
    window.addEventListener("resize", () => updateQuadrant(pet));
    return pet;
  }

  function setRunning(on) {
    running = !!on;
    if (!running) audioSilenceStartedAt = 0;
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

  // 控制面板尺寸的保守估计(实际约 212x247,稍微宽一点更稳)。
  // 用来决定面板朝桌宠的哪个方向展开,确保不溢出屏幕。
  const PANEL_W = 220;
  const PANEL_H = 320;

  // updateQuadrant 给桌宠打 si-pet-q-tl / tr / bl / br 类,
  // CSS 据此决定控制面板向远离屏幕边缘的方向展开。
  // 判定标准:**比较桌宠四周可用空间和面板尺寸**,
  // 把面板放到空间够的那一边;如果两边都不够,选空间更大的一边。
  function updateQuadrant(pet) {
    const handle = pet.querySelector(".si-pet-body");
    if (!handle) return;
    const r = handle.getBoundingClientRect();
    const spaceAbove = r.top;
    const spaceBelow = window.innerHeight - r.bottom;
    const spaceLeft = r.right;             // 面板若左对齐桌宠则向右扩,看右边可用空间
    const spaceRight = window.innerWidth - r.left; // 面板若右对齐桌宠则向左扩,看左边可用空间
    // 优先选「能完全放下」的一边;两边都放不下就选剩余空间更大的一边。
    const goUp = spaceAbove >= PANEL_H
      ? true
      : spaceBelow >= PANEL_H
        ? false
        : spaceAbove >= spaceBelow;
    const alignLeft = spaceRight >= PANEL_W
      ? true
      : spaceLeft >= PANEL_W
        ? false
        : spaceRight >= spaceLeft;
    // 类名按「桌宠所处象限」命名(t=面板向下,b=面板向上;l=桌宠靠左/面板向右扩)
    const cls =
      !goUp && alignLeft ? "si-pet-q-tl" :
      !goUp && !alignLeft ? "si-pet-q-tr" :
      goUp && alignLeft ? "si-pet-q-bl" : "si-pet-q-br";
    for (const c of ["si-pet-q-tl", "si-pet-q-tr", "si-pet-q-bl", "si-pet-q-br"]) {
      if (c !== cls) pet.classList.remove(c);
    }
    pet.classList.add(cls);
  }

  function makePetDraggable(pet) {
    const handle = pet.querySelector(".si-pet-body");
    let dragging = false;
    let moved = false;
    let startX = 0;
    let startY = 0;
    // 锚定方式始终是「右下角」(right/bottom)。配合容器只占 76x76 的 CSS,
    // 锚定点不会因为面板展开/收起而漂移,拖动定位稳定。
    let originRight = 0;
    let originBottom = 0;

    handle.addEventListener("pointerdown", (e) => {
      dragging = true;
      moved = false;
      startX = e.clientX;
      startY = e.clientY;
      const rect = pet.getBoundingClientRect();
      originRight = window.innerWidth - rect.right;
      originBottom = window.innerHeight - rect.bottom;
      pet.classList.add("si-pet-dragging");
      try { handle.setPointerCapture(e.pointerId); } catch {}
      e.preventDefault();
    });

    handle.addEventListener("pointermove", (e) => {
      if (!dragging) return;
      const dx = e.clientX - startX;
      const dy = e.clientY - startY;
      if (Math.abs(dx) + Math.abs(dy) > 3) moved = true;
      // 没越过拖动阈值则什么都不动,避免把「点击」误当成「微拖动」改写 style。
      if (!moved) return;
      const maxR = window.innerWidth - pet.offsetWidth;
      const maxB = window.innerHeight - pet.offsetHeight;
      const right = Math.max(0, Math.min(originRight - dx, maxR));
      const bottom = Math.max(0, Math.min(originBottom - dy, maxB));
      pet.style.right = right + "px";
      pet.style.bottom = bottom + "px";
      pet.style.left = "auto";
      pet.style.top = "auto";
      updateQuadrant(pet);
    });

    const end = (e) => {
      if (!dragging) return;
      dragging = false;
      pet.classList.remove("si-pet-dragging");
      try { handle.releasePointerCapture(e.pointerId); } catch {}
      if (moved) {
        savePetPos(pet);
      } else {
        // 点击展开/收起面板。展开前再重算一次 quadrant,基于当下桌宠位置
        // 和窗口尺寸选最不溢出屏幕的展开方向。
        if (!pet.classList.contains("si-pet-open")) updateQuadrant(pet);
        pet.classList.toggle("si-pet-open");
        if (pet.classList.contains("si-pet-open")) refreshState();
      }
    };
    handle.addEventListener("pointerup", end);
    handle.addEventListener("pointercancel", end);
  }

  function savePetPos(pet) {
    try {
      chrome.storage?.local?.set({ petPosRB: { right: pet.style.right, bottom: pet.style.bottom } });
    } catch {}
  }

  function restorePetPos(pet) {
    try {
      // 顺手清掉旧版按 left/top 存的位置数据。旧逻辑在「点击」时也会误存,
      // 留着会导致每次注入又把桌宠放到那个坏位置上。
      chrome.storage?.local?.remove?.("petPos");
      chrome.storage?.local?.get("petPosRB", (r) => {
        if (r && r.petPosRB && r.petPosRB.right) {
          pet.style.right = r.petPosRB.right;
          pet.style.bottom = r.petPosRB.bottom;
          pet.style.left = "auto";
          pet.style.top = "auto";
        }
        updateQuadrant(pet); // 不论恢复成功与否,都根据当前位置选好象限
      });
    } catch {
      updateQuadrant(pet);
    }
  }

  function petTalk() {
    const pet = ensurePet();
    pet.classList.add("si-pet-talking");
    clearTimeout(petTalkTimer);
    petTalkTimer = setTimeout(() => pet.classList.remove("si-pet-talking"), 1600);
  }

  // ── 消息处理 ────────────────────────────────────────────────────────
  chrome.runtime.onMessage.addListener((msg) => {
    if (msg?.channel !== "page-subtitle") return;
    if (msg.type === "subtitle") {
      setRunning(true);
      ingestSubtitle(msg);
      if (msg.corrected) markCorrected(msg.segment_id);
      if (msg.status === "final" || msg.corrected) {
        render();
      } else if (msg.target) {
        renderTentative();
      }
      // 有新译文就让桌宠张嘴做一下「正在说话」动画(不再弹气泡,避免遮挡页面)。
      if (msg.target) petTalk();
    } else if (msg.type === "translate_error") {
      // 翻译失败:不打断字幕流,只在 finals 里给该句留下一个轻提示占位。
      // 句子文本仍是空(已有原文则保留),不影响后续译文/纠错回填覆盖。
      // 这里不放醒目报错,避免在流式字幕中插入一个无法滚掉的"⚠"字样。
    } else if (msg.type === "vad") {
      // VAD:检测页面何时进入/离开静音,驱动「静音持续 N 秒后停翻」。
      onAudioVAD(msg.event);
    } else if (msg.type === "state") {
      setRunning(!!msg.running);
    } else if (msg.type === "clear") {
      setRunning(false);
    } else if (msg.type === "lang_change") {
      resetCaptionsForLangChange();
    }
  });

  ensurePet();
  // 注入后查询运行状态与当前快捷键(刷新页面或补注入时恢复显示)。
  refreshState();
  // 恢复字幕外观偏好(用户上次拖到的位置 + 选的颜色),
  // 之后 ensureOverlay 创建字幕条时也会 applyCaptionPos/Color 一次,无缝衔接。
  restoreCaptionPrefs();
})();
