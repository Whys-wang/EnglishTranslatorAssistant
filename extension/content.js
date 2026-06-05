// content.js —— 在页面中注入双语字幕 overlay。
//
// 里程碑 1:建立 overlay 容器,响应来自后端的字幕事件(占位渲染)。
// 里程碑 5:实现 partial 灰显、final 定稿、原文/译文双语、按 segment_id 原地更新。

const OVERLAY_ID = "__simul_interpreter_overlay__";

function ensureOverlay() {
  let el = document.getElementById(OVERLAY_ID);
  if (el) return el;
  el = document.createElement("div");
  el.id = OVERLAY_ID;
  el.innerHTML = `<div class="si-subtitle-list"></div>`;
  document.documentElement.appendChild(el);
  return el;
}

// segments: segment_id -> { source, target, status }
const segments = new Map();

function renderSegment(seg) {
  const overlay = ensureOverlay();
  const list = overlay.querySelector(".si-subtitle-list");
  let row = list.querySelector(`[data-seg="${seg.segment_id}"]`);
  if (!row) {
    row = document.createElement("div");
    row.className = "si-seg";
    row.dataset.seg = seg.segment_id;
    row.innerHTML = `<div class="si-source"></div><div class="si-target"></div>`;
    list.appendChild(row);
  }
  row.classList.toggle("si-partial", seg.status === "partial");
  row.classList.toggle("si-final", seg.status === "final");
  row.querySelector(".si-source").textContent = seg.source || "";
  row.querySelector(".si-target").textContent = seg.target || "";
}

chrome.runtime.onMessage.addListener((msg) => {
  if (msg?.target !== "page-subtitle") return;
  if (msg.type === "subtitle") {
    segments.set(msg.segment_id, msg);
    renderSegment(msg);
  }
});
