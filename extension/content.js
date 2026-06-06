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
//   - 静音清屏:连续说话时只滚动不消失,但一旦超过 ~3 秒没识别到新内容,字幕条
//     自动清空留白(滚动效果不变),再次说话时新句子从空白处重新开始滚动。

(() => {
  if (window.__simulInterpreterInjected) return;
  window.__simulInterpreterInjected = true;

  const OVERLAY_ID = "__simul_interpreter_overlay__";
  const PET_ID = "__simul_interpreter_pet__";
  // 内存里最多保留多少条已定稿的译文。屏幕(底部锚定)只显示最新两行,但内存里多留
  // 几句:一是便于 ASR / LLM 复审对已滚出视野的句子做修订时仍能正确更新,二是滚动
  // 历史更连贯。超过此数丢最早的(它们早就滚出视野)。
  const FINAL_BUFFER_SIZE = 50;
  // 静音自动清屏:超过这么久没有识别到新内容,就把当前字幕条清空(留白)。
  // 之后一旦再识别到声音,新句子从空白处重新开始滚动,不影响滚动效果本身。
  const CLEAR_AFTER_SILENCE_MS = 3000;
  // 纠错高亮:某段被「ASR 修订重译」或「LLM 复审」改过后,在字幕上给它一个
  // 淡入即淡出的底色高亮,持续这么久后自动消失(让你肉眼看到"这句刚被自动纠正")。
  const CORRECTED_FLASH_MS = 1200;
  // 纠正之后,在高亮淡出结束后再额外保留这么久的「安静可读」时间,期间不触发
  // 静音清屏,确保闪光不会和字幕消失撞在一起、让你来得及看清修正后的句子。
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

  // 纠错高亮用:segId -> 高亮起始时间戳;flashTimer 驱动淡出期间的平滑重绘。
  // lastCorrectionShownAt:最近一次「确实显示出来的纠正」时间,用于在纠正后
  // 暂缓静音清屏,给用户留出阅读时间。
  const correctedAt = new Map();
  let flashTimer = null;
  let lastCorrectionShownAt = 0;

  // 是否「粘底」:true=自动停在最新两行(默认);用户向上滚动看历史时变 false,
  // 此时不再自动拽回底部、也暂停静音清屏,滚回底部后恢复。
  let stickToBottom = true;

  // 字幕外观偏好:用户拖到哪 / 颜色选的什么,都会持久化到 chrome.storage.local。
  // captionPos 为 null 时表示用默认「底部居中」(由 CSS 控制)。
  let captionPos = null; // {left:number, top:number} 或 null
  let captionColor = DEFAULT_CAPTION_COLOR;
  let captionLocked = false; // 锁定后字幕不可拖动(右上角锁图标切换,持久化)

  function ensureOverlay() {
    let el = document.getElementById(OVERLAY_ID);
    if (el) return el;
    el = document.createElement("div");
    el.id = OVERLAY_ID;
    // 字幕条 + 右上角锁图标。初始 span 占位,render 会按分段重建内容。
    el.innerHTML = `<div class="si-caption"><span class="si-committed"></span><span class="si-tentative"></span></div><button class="si-caption-lock" type="button"></button>`;
    document.documentElement.appendChild(el);
    applyCaptionPos(el);
    applyCaptionColor(el);
    applyCaptionLock(el);
    makeCaptionDraggable(el);

    // 滚动监听:判断用户是否在底部附近。滚到底=粘底(自动跟最新);
    // 向上滚=查看历史,暂停自动跟随与静音清屏。
    const captionEl = el.querySelector(".si-caption");
    if (captionEl) {
      captionEl.addEventListener("scroll", () => {
        stickToBottom =
          captionEl.scrollHeight - captionEl.scrollTop - captionEl.clientHeight < 24;
      });
    }

    // 锁图标:点击切换锁定/解锁并持久化;阻止冒泡,避免被拖动逻辑吞掉。
    const lockBtn = el.querySelector(".si-caption-lock");
    if (lockBtn) {
      lockBtn.addEventListener("pointerdown", (e) => e.stopPropagation());
      lockBtn.addEventListener("click", (e) => {
        e.stopPropagation();
        captionLocked = !captionLocked;
        try { chrome.storage?.local?.set({ captionLocked }); } catch {}
        applyCaptionLock(el);
      });
    }
    return el;
  }

  function removeOverlay() {
    const el = document.getElementById(OVERLAY_ID);
    if (el) el.remove();
    finals.clear();
    tentative.segId = null;
    tentative.target = "";
    lastActivityAt = 0;
    clearedIds.clear();
    correctedAt.clear();
    lastCorrectionShownAt = 0;
    stickToBottom = true;
    if (flashTimer) {
      clearTimeout(flashTimer);
      flashTimer = null;
    }
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

  // 应用锁定状态:切换锁图标(开/合)、字幕光标,并据此启用/禁用拖动。
  function applyCaptionLock(overlay) {
    if (!overlay) return;
    const btn = overlay.querySelector(".si-caption-lock");
    const caption = overlay.querySelector(".si-caption");
    if (btn) {
      btn.innerHTML = captionLocked ? LOCK_CLOSED_SVG : LOCK_OPEN_SVG;
      btn.classList.toggle("si-locked-on", captionLocked);
      btn.title = captionLocked ? "字幕已锁定 · 点击解锁后可拖动" : "字幕可拖动 · 点击锁定位置";
    }
    if (caption) caption.classList.toggle("si-locked", captionLocked);
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

  // 把 overlay 做成「按住 + 拖动」就能改位置,松手即保存到 storage。
  // 边界做了夹取,不会被拖到屏幕外面去找不回来;实在拖丢可点桌宠面板的「重置位置」。
  function makeCaptionDraggable(overlay) {
    const caption = overlay.querySelector(".si-caption");
    if (!caption) return;
    let dragging = false;
    let startX = 0;
    let startY = 0;
    let originLeft = 0;
    let originTop = 0;
    let moved = false;

    caption.addEventListener("pointerdown", (e) => {
      if (captionLocked) return; // 已锁定:禁止拖动
      // 点在右侧滚动条上时不拖动,交给原生滚动(clientWidth 不含滚动条宽度)。
      if (e.offsetX > caption.clientWidth) return;
      dragging = true;
      moved = false;
      startX = e.clientX;
      startY = e.clientY;
      const rect = overlay.getBoundingClientRect();
      originLeft = rect.left;
      originTop = rect.top;
      caption.classList.add("si-dragging");
      try { caption.setPointerCapture(e.pointerId); } catch {}
      e.preventDefault();
      e.stopPropagation();
    });

    caption.addEventListener("pointermove", (e) => {
      if (!dragging) return;
      const dx = e.clientX - startX;
      const dy = e.clientY - startY;
      if (Math.abs(dx) + Math.abs(dy) > 2) moved = true;
      if (!moved) return;
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
      caption.classList.remove("si-dragging");
      try { caption.releasePointerCapture(e.pointerId); } catch {}
      // 没真正拖动(只是点了一下)就什么都不保存。
      if (!moved || !captionPos) return;
      try {
        chrome.storage?.local?.set({ captionPos });
      } catch {}
    };
    caption.addEventListener("pointerup", end);
    caption.addEventListener("pointercancel", end);
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

  // 某段是否处于「刚被纠正」的高亮窗口内,返回 1->0 的剩余强度(线性淡出)。
  function correctionStrength(id, now) {
    if (id == null) return 0;
    const ts = correctedAt.get(id);
    if (ts == null) return 0;
    const elapsed = now - ts;
    if (elapsed >= CORRECTED_FLASH_MS) return 0;
    return 1 - elapsed / CORRECTED_FLASH_MS;
  }

  // 渲染字幕:左对齐 + 底部锚定的竖直滚动(参考 YouTube / 系统实时字幕)。
  // 关键改动:不再「按末尾 N 字滑动 + 居中」——那会让整行每来一个字就重新居中、
  // 向左横移,导致正在读的内容一直在跑、读不完就被挤走。现在文字左对齐、自然换行、
  // 只在末尾追加(已显示的文字不再左右横移),旧内容向上滚出视野,
  // 底部始终停在最新两行,阅读位置稳定。
  function render() {
    const overlay = ensureOverlay();
    const caption = overlay.querySelector(".si-caption");
    if (!caption) return;
    const now = Date.now();
    // 记录重建前的滚动位置:用户在看历史(未粘底)时,重建后要还原,不能跳。
    const prevScrollTop = caption.scrollTop;

    const pieces = buildPieces();

    // 按分段重建 span。.si-caption 本身不重建,挂在它上面的拖动监听不受影响。
    caption.textContent = "";
    let anyFlash = false;
    pieces.forEach((it, idx) => {
      if (idx > 0) caption.appendChild(document.createTextNode(" "));
      const span = document.createElement("span");
      span.className = it.tentative ? "si-tentative" : "si-committed";
      span.textContent = it.text;
      if (!it.tentative) {
        const k = correctionStrength(it.id, now);
        if (k > 0) {
          span.classList.add("si-corrected");
          // 高亮底色随剩余强度淡出;直接写内联色,不靠 CSS 动画
          // (span 每次 render 都会重建,CSS 动画会被反复打断,无法形成平滑淡出)。
          span.style.backgroundColor = "rgba(86, 214, 132, " + (0.55 * k).toFixed(3) + ")";
          anyFlash = true;
        }
      }
      caption.appendChild(span);
    });

    // 字幕条整体可见性:有任何内容才显示,完全没有就隐藏(避免空黑条)。
    overlay.classList.toggle("si-empty", pieces.length === 0);
    // 粘底时停在最底显示最新两行;用户在看历史(未粘底)时,还原其滚动位置,
    // 不被新字幕拽回底部(内容是向末尾追加的,顶部稳定,还原位置即可保持视图)。
    if (stickToBottom) {
      caption.scrollTop = caption.scrollHeight;
    } else {
      caption.scrollTop = prevScrollTop;
    }

    // 清理过期高亮记录,避免 Map 无限增长。
    if (correctedAt.size) {
      for (const [id, ts] of correctedAt) {
        if (now - ts >= CORRECTED_FLASH_MS) correctedAt.delete(id);
      }
    }
    // 仍有高亮在淡出 -> 安排下一帧重绘,做出平滑淡出(即使此刻没有新字幕进来)。
    if (anyFlash) scheduleFlash();
  }

  // 高亮淡出的「补帧」循环:在淡出窗口内以 ~12fps 反复重绘,直到所有高亮消失。
  // 单一 timer 防止并发循环;render 在仍有高亮时会自动续上。
  function scheduleFlash() {
    if (flashTimer) return;
    flashTimer = setTimeout(() => {
      flashTimer = null;
      render();
    }, 80);
  }

  // 标记某段「刚被自动纠正」,触发淡出高亮。只对当前确实在显示的句子打标,
  // 已被静音清屏的句子不再高亮(它根本不在屏上)。
  function markCorrected(segId) {
    if (!segId || !finals.has(segId)) return;
    const now = Date.now();
    correctedAt.set(segId, now);
    lastCorrectionShownAt = now;
  }

  // 静音清屏:翻译进行中,若超过 CLEAR_AFTER_SILENCE_MS 没有新内容进来,
  // 就把当前字幕条清空(隐藏)。被清掉的句子 id 记下来,避免它们的迟到更新
  // 把字幕条又唤回;clearedIds 每次只保留「刚清掉的这一屏」,内存不会无限增长。
  // 之后一旦再识别到声音,新句子(新 id)正常进入,字幕从空白处重新开始滚动。
  function clearCaptionOnSilence() {
    if (!running) return;
    if (finals.size === 0 && !tentative.target) return;
    // 用户正在向上滚动看历史译文:暂停清屏,免得正读着就被清空。
    if (!stickToBottom) return;
    const now = Date.now();
    if (!lastActivityAt || now - lastActivityAt < CLEAR_AFTER_SILENCE_MS) return;
    // 刚有过纠正:在高亮淡出 + 一段可读时间内不清屏,避免「闪一下就消失」。
    if (
      lastCorrectionShownAt &&
      now - lastCorrectionShownAt < CORRECTED_FLASH_MS + READ_HOLD_AFTER_CORRECTION_MS
    ) {
      return;
    }
    clearedIds.clear();
    for (const id of finals.keys()) clearedIds.add(id);
    if (tentative.segId) clearedIds.add(tentative.segId);
    finals.clear();
    tentative.segId = null;
    tentative.target = "";
    render();
  }
  setInterval(clearCaptionOnSilence, 500);

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
        <div class="si-pet-section">字幕外观(可直接拖动字幕条改位置)</div>
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
      pushLangConfig();
    });
    tgtSel.addEventListener("change", () => {
      try { chrome.storage?.local?.set({ targetLang: tgtSel.value }); } catch {}
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

    // 面板刚拼好,把当前 captionColor 同步到 UI(高亮命中预设 / 写入 picker 值)。
    syncPetColorUI();

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
  const PANEL_H = 260;

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
      // 后端标记 corrected=true:本句由「ASR 修订重译」或「LLM 复审」纠正过,
      // 给它一个淡出高亮,让用户看到自动纠错确实发生了。
      if (msg.corrected) markCorrected(msg.segment_id);
      render();
      // 有新译文就让桌宠张嘴做一下「正在说话」动画(不再弹气泡,避免遮挡页面)。
      if (msg.target) petTalk();
    } else if (msg.type === "translate_error") {
      // 翻译失败:不打断字幕流,只在 finals 里给该句留下一个轻提示占位。
      // 句子文本仍是空(已有原文则保留),不影响后续译文/纠错回填覆盖。
      // 这里不放醒目报错,避免在流式字幕中插入一个无法滚掉的"⚠"字样。
    } else if (msg.type === "vad") {
      // VAD 仅做 worklet 端的静音检测信号,这里不直接用它清屏。
      // 「静音超过 3 秒清屏」由 clearCaptionOnSilence 定时器按「最近识别到内容的时间」
      // 判定(见 CLEAR_AFTER_SILENCE_MS),比逐帧 VAD 更稳,也不会被环境噪声误触发。
    } else if (msg.type === "state") {
      setRunning(!!msg.running);
    } else if (msg.type === "clear") {
      setRunning(false);
    }
  });

  ensurePet();
  // 注入后查询运行状态与当前快捷键(刷新页面或补注入时恢复显示)。
  refreshState();
  // 恢复字幕外观偏好(用户上次拖到的位置 + 选的颜色),
  // 之后 ensureOverlay 创建字幕条时也会 applyCaptionPos/Color 一次,无缝衔接。
  restoreCaptionPrefs();
})();
