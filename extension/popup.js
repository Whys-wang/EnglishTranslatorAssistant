// popup.js —— 弹窗控制:开始 / 停止当前标签页的同声传译。

const statusEl = document.getElementById("status");

function setStatus(text) {
  statusEl.textContent = text;
}

async function currentTabId() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  return tab?.id;
}

document.getElementById("start").addEventListener("click", async () => {
  const tabId = await currentTabId();
  if (tabId == null) return setStatus("找不到当前标签页");
  setStatus("正在连接…");
  chrome.runtime.sendMessage({ target: "background", type: "start", tabId }, (resp) => {
    setStatus(resp?.ok ? "已开始" : `失败:${resp?.error || "未知错误"}`);
  });
});

document.getElementById("stop").addEventListener("click", () => {
  chrome.runtime.sendMessage({ target: "background", type: "stop" }, () => {
    setStatus("已停止");
  });
});
