// offscreen.js —— 在 offscreen 文档中执行音频采集与 WebSocket 推流。
//
// 里程碑 1:建立到后端的 WebSocket 连接,完成「start/ack」控制握手以验证空链路。
// 里程碑 2:用 AudioWorklet 把标签页音频重采样为 16kHz/16bit/单声道 PCM,
//          按 ~100ms 分片以二进制帧发送。

const BACKEND_WS_URL = "ws://localhost:8765/ws";

let ws = null;
let audioContext = null;
let mediaStream = null;
let workletNode = null;

function connectBackend() {
  return new Promise((resolve, reject) => {
    ws = new WebSocket(BACKEND_WS_URL);
    ws.binaryType = "arraybuffer";
    ws.onopen = () => {
      ws.send(JSON.stringify({ type: "start" }));
      resolve();
    };
    ws.onerror = (e) => reject(e);
    ws.onmessage = (ev) => {
      if (typeof ev.data !== "string") return;
      let msg;
      try {
        msg = JSON.parse(ev.data);
      } catch {
        return;
      }
      // 后端回传的字幕事件 -> 交给 background 转发到页面 overlay。
      if (msg.type === "subtitle") {
        chrome.runtime.sendMessage({ target: "page-subtitle", ...msg });
      }
    };
    ws.onclose = () => {
      ws = null;
    };
  });
}

async function start(streamId) {
  await connectBackend();

  // 用 tab 媒体流 id 获取音频流。
  mediaStream = await navigator.mediaDevices.getUserMedia({
    audio: {
      mandatory: {
        chromeMediaSource: "tab",
        chromeMediaSourceId: streamId,
      },
    },
    video: false,
  });

  audioContext = new AudioContext();
  // 关键:保持把标签页声音播放给用户(否则会静音)。
  const source = audioContext.createMediaStreamSource(mediaStream);
  source.connect(audioContext.destination);

  // TODO(里程碑 2): 加载 audio-worklet.js,在 worklet 内重采样为 16kHz PCM,
  // 通过 port 把分片回传到这里再经 ws.send 发送二进制帧。
  // await audioContext.audioWorklet.addModule(chrome.runtime.getURL("audio-worklet.js"));
  // workletNode = new AudioWorkletNode(audioContext, "pcm16k-downsampler");
  // source.connect(workletNode);
  // workletNode.port.onmessage = (e) => { if (ws?.readyState === 1) ws.send(e.data); };
}

function stop() {
  if (workletNode) {
    workletNode.disconnect();
    workletNode = null;
  }
  if (mediaStream) {
    mediaStream.getTracks().forEach((t) => t.stop());
    mediaStream = null;
  }
  if (audioContext) {
    audioContext.close();
    audioContext = null;
  }
  if (ws) {
    try {
      ws.send(JSON.stringify({ type: "stop" }));
    } catch {}
    ws.close();
    ws = null;
  }
}

chrome.runtime.onMessage.addListener((msg) => {
  if (msg?.target !== "offscreen") return;
  if (msg.type === "start") {
    start(msg.streamId).catch((err) => console.error("[offscreen] start failed", err));
  } else if (msg.type === "stop") {
    stop();
  }
});
