// popup.js —— 弹窗控制:开始 / 停止当前标签页的同声传译,并选择翻译方向(源→目标语言)。

const statusEl = document.getElementById("status");
const srcEl = document.getElementById("src");
const tgtEl = document.getElementById("tgt");

function setStatus(text) {
  statusEl.textContent = text;
}

async function currentTabId() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  return tab?.id;
}

// 恢复上次选择的语言方向。
chrome.storage.local.get(["sourceLang", "targetLang"], (r) => {
  if (typeof r.sourceLang === "string") srcEl.value = r.sourceLang;
  if (r.targetLang) tgtEl.value = r.targetLang;
});

// 选择变化即保存(下次打开沿用)。
srcEl.addEventListener("change", () => chrome.storage.local.set({ sourceLang: srcEl.value }));
tgtEl.addEventListener("change", () => chrome.storage.local.set({ targetLang: tgtEl.value }));

document.getElementById("start").addEventListener("click", async () => {
  const tabId = await currentTabId();
  if (tabId == null) return setStatus("找不到当前标签页");
  const sourceLang = srcEl.value;
  const targetLang = tgtEl.value;
  chrome.storage.local.set({ sourceLang, targetLang });
  setStatus("正在连接…");
  chrome.runtime.sendMessage(
    { target: "background", type: "start", tabId, sourceLang, targetLang },
    (resp) => {
      setStatus(resp?.ok ? "已开始" : `失败:${resp?.error || "未知错误"}`);
    }
  );
});

document.getElementById("stop").addEventListener("click", () => {
  chrome.runtime.sendMessage({ target: "background", type: "stop" }, () => {
    setStatus("已停止");
  });
});
