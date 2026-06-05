// background.js —— MV3 Service Worker
//
// 职责:
//   - 响应 popup 的「开始/停止」指令;
//   - 通过 chrome.tabCapture.getMediaStreamId 拿到当前标签页音频流 id;
//   - 创建 offscreen 文档,把流 id 交给它去采集、重采样、推送 WebSocket;
//   - 主动把字幕内容脚本注入目标标签页(兼容「先开页面后装扩展」);
//   - 转发后端回传的字幕事件给对应标签页的 content script。
//
// 注意:MV3 Service Worker 会被随时回收,模块级变量不可靠,
// 因此 activeTabId 同时写入 chrome.storage.session 以便重启后恢复。

const OFFSCREEN_PATH = "offscreen.html";

let activeTabId = null;

async function setActiveTab(tabId) {
  activeTabId = tabId;
  await chrome.storage.session.set({ activeTabId: tabId });
}

async function getActiveTab() {
  if (activeTabId != null) return activeTabId;
  const { activeTabId: saved } = await chrome.storage.session.get("activeTabId");
  activeTabId = saved ?? null;
  return activeTabId;
}

async function clearActiveTab() {
  activeTabId = null;
  await chrome.storage.session.remove("activeTabId");
}

async function ensureOffscreen() {
  const has = await chrome.offscreen.hasDocument?.();
  if (has) return;
  await chrome.offscreen.createDocument({
    url: OFFSCREEN_PATH,
    reasons: ["USER_MEDIA"],
    justification: "采集标签页音频并重采样为 16kHz PCM 推送到后端进行实时翻译。",
  });
}

function setBadge(text, color) {
  try {
    chrome.action.setBadgeBackgroundColor({ color });
    chrome.action.setBadgeText({ text });
  } catch (e) {
    /* 忽略 */
  }
}

// 把字幕样式与脚本注入目标标签页。已注入的页面由 content.js 内的守卫保证幂等。
// 注意:这里不再吞掉异常,让注入失败的真实原因冒泡到 popup 显示。
async function ensureContentScript(tabId) {
  await chrome.scripting.insertCSS({ target: { tabId }, files: ["overlay.css"] });
  await chrome.scripting.executeScript({ target: { tabId }, files: ["content.js"] });
}

let rxCount = 0;

async function startCapture(tabId) {
  console.log("[SI] startCapture, tabId =", tabId);
  await setActiveTab(tabId);
  rxCount = 0;
  try {
    await ensureContentScript(tabId);
    console.log("[SI] 内容脚本注入成功");
    setBadge("ON", "#108446");
  } catch (e) {
    console.error("[SI] 内容脚本注入失败", e);
    setBadge("ERR", "#d23b3b");
    chrome.action.setTitle({ title: "字幕注入失败:" + String(e) });
    throw new Error("字幕脚本注入失败(此页面可能禁止注入,请换普通网页):" + String(e));
  }
  await ensureOffscreen();
  console.log("[SI] offscreen 文档已就绪");

  const streamId = await chrome.tabCapture.getMediaStreamId({
    targetTabId: tabId,
  });
  console.log("[SI] 取得 streamId =", streamId);

  // 交给 offscreen 去真正采集音频。
  chrome.runtime.sendMessage({
    target: "offscreen",
    type: "start",
    streamId,
    tabId,
  });
  console.log("[SI] 已通知 offscreen 开始采集");
}

async function stopCapture() {
  chrome.runtime.sendMessage({ target: "offscreen", type: "stop" });
  const tabId = await getActiveTab();
  if (tabId != null) {
    // 通知页面移除字幕 overlay(含绿色状态条)。
    try {
      await chrome.tabs.sendMessage(tabId, { channel: "page-subtitle", type: "clear" });
    } catch (e) {
      /* 标签页可能已关闭 */
    }
  }
  await clearActiveTab();
  setBadge("", "#108446");
}

async function forwardSubtitle(msg) {
  let tabId = null;
  try {
    tabId = await getActiveTab();
  } catch (e) {
    chrome.action.setTitle({ title: "getActiveTab 失败:" + String(e) });
  }
  if (tabId == null) {
    console.warn("[SI] 转发失败:未找到目标标签页(activeTab 丢失)");
    chrome.action.setTitle({ title: "未找到目标标签页(activeTab 丢失)" });
    return;
  }
  try {
    await chrome.tabs.sendMessage(tabId, msg);
    console.log("[SI] 已转发到页面 tab =", tabId);
    chrome.action.setTitle({ title: "同声传译:字幕转发中" });
  } catch (e) {
    console.error("[SI] 转发到页面失败 tab =", tabId, e);
    chrome.action.setTitle({ title: "转发到页面失败:" + String(e) });
  }
}

// offscreen 通过常驻 Port 推送字幕(同时让本 SW 在采集期间保持存活)。
chrome.runtime.onConnect.addListener((port) => {
  if (port.name !== "si-subtitles") return;
  console.log("[SI] offscreen 端口已连接");
  port.onMessage.addListener((msg) => {
    if (msg?.channel === "page-subtitle") {
      rxCount++;
      console.log("[SI] (port) 收到字幕 #" + rxCount, "type =", msg.type, "source =", msg.source);
      setBadge(String(rxCount % 1000), "#108446");
      forwardSubtitle(msg);
    }
  });
  port.onDisconnect.addListener(() => console.log("[SI] offscreen 端口断开"));
});

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  console.log("[SI] 收到消息 target =", msg?.target, "type =", msg?.type);
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
  if (msg?.channel === "page-subtitle") {
    // 同步先把角标 +1:证明「offscreen -> background」这条消息确实到达了。
    rxCount++;
    console.log("[SI] 收到字幕消息 #" + rxCount, "type =", msg?.type, "source =", msg?.source);
    setBadge(String(rxCount % 1000), "#108446");
    forwardSubtitle(msg);
    return false;
  }
  return false;
});
