// background.js —— MV3 Service Worker
//
// 职责:
//   - 响应 popup 的「开始/停止」指令;
//   - 通过 chrome.tabCapture.getMediaStreamId 拿到当前标签页音频流 id;
//   - 创建 offscreen 文档,把流 id 交给它去采集、重采样、推送 WebSocket;
//   - 转发后端回传的字幕事件给对应标签页的 content script。
//
// 里程碑 1:打通控制链路与 offscreen 生命周期;音频管线在里程碑 2 完善。

const OFFSCREEN_PATH = "offscreen.html";

let activeTabId = null;

async function ensureOffscreen() {
  const has = await chrome.offscreen.hasDocument?.();
  if (has) return;
  await chrome.offscreen.createDocument({
    url: OFFSCREEN_PATH,
    reasons: ["USER_MEDIA"],
    justification: "采集标签页音频并重采样为 16kHz PCM 推送到后端进行实时翻译。",
  });
}

async function startCapture(tabId) {
  activeTabId = tabId;
  await ensureOffscreen();

  const streamId = await chrome.tabCapture.getMediaStreamId({
    targetTabId: tabId,
  });

  // 交给 offscreen 去真正采集音频。
  chrome.runtime.sendMessage({
    target: "offscreen",
    type: "start",
    streamId,
    tabId,
  });
}

async function stopCapture() {
  chrome.runtime.sendMessage({ target: "offscreen", type: "stop" });
  activeTabId = null;
}

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  // 来自 popup 的控制
  if (msg?.target === "background") {
    if (msg.type === "start") {
      startCapture(msg.tabId).then(
        () => sendResponse({ ok: true }),
        (err) => sendResponse({ ok: false, error: String(err) })
      );
      return true; // async
    }
    if (msg.type === "stop") {
      stopCapture().then(() => sendResponse({ ok: true }));
      return true;
    }
  }

  // 来自 offscreen 的字幕事件 -> 转发给页面 overlay
  if (msg?.target === "page-subtitle" && activeTabId != null) {
    chrome.tabs.sendMessage(activeTabId, msg);
  }
  return false;
});
