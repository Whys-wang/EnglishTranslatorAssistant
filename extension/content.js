// content.js —— 页面内的「桌宠控制台」+ 流式字幕 overlay。
//
// 通过 manifest 的 content_scripts 在每个页面自动注入:只要扩展开着,
// 打开任意网页桌宠就常驻在页面上。桌宠即控制台:
//   - 点击桌宠展开控制面板:选择源/目标语言、开始/停止翻译、查看状态;
//   - 「开始翻译」既可点桌宠面板里的按钮,也可按快捷键 Alt+Shift+S(默认);
//   - 拖动桌宠可移动(位置记忆);有译文时张嘴提示「在说话」。
//
// 字幕 overlay —— 流式字幕条(Google Live Caption / Apple Live Captions 风格):
//   - 屏幕底部一条固定的字幕条,内容像打字机一样持续追加,永不主动消失;
//   - 数据模型:finals(已定稿句子,按出现顺序拼接) + tentative(当前 partial 译文,
//     接在末尾);ASR 修订时 finals 同 id 覆盖,自然反映纠错;
//   - 截断显示:全文超过 MAX_TARGET_CHARS 就只看「最后 N 字」,前面省略号提示,
//     无论 ASR 多久不切句、单句多长,字幕始终只占 1~2 行,而且一直在更新最新内容;
//   - 因为永远在流动,没有「出现 → 等死 → 消失」的硬节奏,消除「字幕停留太久」
//     和「音画不同步」的别扭感。

(() => {
  if (window.__simulInterpreterInjected) return;
  window.__simulInterpreterInjected = true;

  const OVERLAY_ID = "__simul_interpreter_overlay__";
  const PET_ID = "__simul_interpreter_pet__";
  // 字幕条最多显示多少字符:超过就只看最后 MAX_TARGET_CHARS 个字,前面用 … 占位。
  // 60 字大约对应 1~2 行屏幕宽度,既不会撑屏也不会跳得太快。
  const MAX_TARGET_CHARS = 60;
  // 内存里最多保留多少条已定稿的译文。屏幕只显示最后 N 字,但内存里多留几句,
  // 便于 ASR / LLM 复审在已"滚出屏幕"的句子上做修订时仍能正确更新。
  const FINAL_BUFFER_SIZE = 50;

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

  // ── 流式字幕 overlay ────────────────────────────────────────────────
  // finals:按"首次出现"的顺序保存已定稿的句子。Map 自带插入顺序,既能拼接,
  //   又允许 ASR / LLM 复审用同 id 原地覆盖。超过 FINAL_BUFFER_SIZE 时丢弃最早的。
  // tentative:当前说话中的句子(partial)的最新译文。同 id 的 final 一到就清掉,
  //   避免重复拼接。
  const finals = new Map();
  const tentative = { segId: null, target: "" };

  // 字幕外观偏好:用户拖到哪 / 颜色选的什么,都会持久化到 chrome.storage.local。
  // captionPos 为 null 时表示用默认「底部居中」(由 CSS 控制)。
  let captionPos = null; // {left:number, top:number} 或 null
  let captionColor = DEFAULT_CAPTION_COLOR;

  function ensureOverlay() {
    let el = document.getElementById(OVERLAY_ID);
    if (el) return el;
    el = document.createElement("div");
    el.id = OVERLAY_ID;
    // 单条字幕条:committed 部分(已定稿,全色)与 tentative 部分(说话中,略淡)
    // 分两个 span,视觉上区分稳定与不稳定,但都在同一行内随末尾流动。
    el.innerHTML = `<div class="si-caption"><span class="si-committed"></span><span class="si-tentative"></span></div>`;
    document.documentElement.appendChild(el);
    applyCaptionPos(el);
    applyCaptionColor(el);
    makeCaptionDraggable(el);
    return el;
  }

  function removeOverlay() {
    const el = document.getElementById(OVERLAY_ID);
    if (el) el.remove();
    finals.clear();
    tentative.segId = null;
    tentative.target = "";
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
      chrome.storage?.local?.get(["captionPos", "captionColor"], (r) => {
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
        const overlay = document.getElementById(OVERLAY_ID);
        if (overlay) {
          applyCaptionPos(overlay);
          applyCaptionColor(overlay);
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

  // 取当前字幕流的 committed(已定稿拼接)与 tentative(当前 partial)。
  function buildStream() {
    const committedParts = [];
    for (const txt of finals.values()) {
      if (txt) committedParts.push(txt);
    }
    let tentativeText = "";
    if (
      tentative.target &&
      (!tentative.segId || !finals.has(tentative.segId))
    ) {
      tentativeText = tentative.target;
    }
    return { committed: committedParts.join(" "), tentative: tentativeText };
  }

  // 字幕「滑动窗口」截断:整条字幕只保留最后 MAX_TARGET_CHARS 个字符。
  // 优先级:tentative 是最新内容,优先完整保留;committed 从尾部反推留多少。
  // 但 tentative 自身若超过 MAX_TARGET_CHARS(ASR 长时间不切句、partial 堆积时),
  // 也要截断到最后 MAX_TARGET_CHARS,以免视觉上仍撑屏。
  // 按 Unicode 码点切,避免砍坏中文 / emoji。
  function clipStream(committedJoined, tentativeText) {
    const committedChars = Array.from(committedJoined);
    const tentativeChars = Array.from(tentativeText || "");
    const sepLen = committedJoined && tentativeText ? 1 : 0;
    const totalLen = committedChars.length + sepLen + tentativeChars.length;
    if (totalLen <= MAX_TARGET_CHARS) {
      return { ellipsis: false, committed: committedJoined, tentative: tentativeText || "" };
    }
    // tentative 自身就超了:committed 整段隐藏,tentative 只留末尾 N 个字。
    if (tentativeChars.length >= MAX_TARGET_CHARS) {
      return {
        ellipsis: true,
        committed: "",
        tentative: tentativeChars.slice(-MAX_TARGET_CHARS).join(""),
      };
    }
    // tentative 没超:它完整显示,committed 从尾部反推留多少。
    const reserveForTentative = tentativeText ? tentativeChars.length + sepLen : 0;
    const committedKeep = Math.max(0, MAX_TARGET_CHARS - reserveForTentative);
    return {
      ellipsis: true,
      committed: committedChars.slice(-committedKeep).join(""),
      tentative: tentativeText || "",
    };
  }

  function render() {
    const overlay = ensureOverlay();
    const committedEl = overlay.querySelector(".si-committed");
    const tentativeEl = overlay.querySelector(".si-tentative");

    const { committed, tentative: tentativeText } = buildStream();
    const clipped = clipStream(committed, tentativeText);
    const prefix = clipped.ellipsis ? "… " : "";
    const sep = clipped.committed && clipped.tentative ? " " : "";

    committedEl.textContent = prefix + clipped.committed + sep;
    tentativeEl.textContent = clipped.tentative;

    // 字幕条整体可见性:有任何内容才显示,完全没有就隐藏(避免空黑条)。
    overlay.classList.toggle("si-empty", !clipped.committed && !clipped.tentative);
  }

  // ── 桌宠控制台(可拖动) ─────────────────────────────────────────────
  let petTalkTimer = null;

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
              if (statusEl) statusEl.textContent = "启动失败:" + chrome.runtime.lastError.message;
              return;
            }
            if (resp?.ok) {
              setRunning(true);
            } else {
              const err = resp?.error || "未知错误";
              // tabCapture 受浏览器限制时退回提示按快捷键。
              if (statusEl) statusEl.textContent = "启动失败:" + err;
            }
          }
        );
      } catch (e) {
        if (statusEl) statusEl.textContent = "启动失败:" + String(e);
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
      render();
      // 有新译文就让桌宠张嘴做一下「正在说话」动画(不再弹气泡,避免遮挡页面)。
      if (msg.target) petTalk();
    } else if (msg.type === "translate_error") {
      // 翻译失败:不打断字幕流,只在 finals 里给该句留下一个轻提示占位。
      // 句子文本仍是空(已有原文则保留),不影响后续译文/纠错回填覆盖。
      // 这里不放醒目报错,避免在流式字幕中插入一个无法滚掉的"⚠"字样。
    } else if (msg.type === "vad") {
      // VAD 仅做 worklet 端的静音检测信号;在新的流式字幕里不再用它清屏
      // (字幕本来就只滚动不消失,无需"停顿即换"的硬清空)。保留为预留接口。
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
