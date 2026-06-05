// offscreen.js —— 在 offscreen 文档中执行音频采集与 WebSocket 推流。
//
// 里程碑 2:
//   - 用 tabCapture 媒体流创建 AudioContext,接 AudioWorklet 重采样为 16kHz PCM;
//   - 同时把原始音频接到扬声器,保证用户仍能听到标签页声音;
//   - worklet 输出的 PCM(Int16, ~100ms/帧)以二进制帧经 WebSocket 发往后端;
//   - WebSocket 断线指数退避自动重连;断开期间丢弃实时帧(不积压,保低延迟)。

const BACKEND_WS_URL = "ws://localhost:8765/ws";
const TARGET_SAMPLE_RATE = 16000;

// 指数退避重连参数(与后端 config.Reconnect 保持一致量级)。
const RECONNECT = {
  initialMs: 500,
  maxMs: 15000,
  multiplier: 2,
};

let ws = null;
let audioContext = null;
let mediaStream = null;
let sourceNode = null;
let workletNode = null;

let running = false; // 是否处于「采集中」(stop 后为 false,停止重连)。
let reconnectDelay = RECONNECT.initialMs;
let reconnectTimer = null;

// 与 background(Service Worker)之间的常驻连接:
//   - 让 SW 在采集期间保持存活(否则会被回收,字幕消息无人接收);
//   - 作为字幕事件的可靠通道。端口约 5 分钟会被系统回收,断开后自动重连。
let swPort = null;

function ensurePort() {
  if (swPort) return swPort;
  swPort = chrome.runtime.connect({ name: "si-subtitles" });
  swPort.onDisconnect.addListener(() => {
    swPort = null;
    if (running) setTimeout(ensurePort, 500); // 会话仍在进行则重连
  });
  return swPort;
}

function sendSubtitleToBackground(msg) {
  try {
    ensurePort().postMessage(msg);
  } catch (e) {
    // 端口异常时退回普通消息通道。
    swPort = null;
    try {
      chrome.runtime.sendMessage(msg);
    } catch {}
  }
}

function wsConnected() {
  return ws && ws.readyState === WebSocket.OPEN;
}

// ── 译文语音(TTS)播放 ───────────────────────────────────────────────
// 后端把每句译文合成的音频(base64)经 WS 下发;这里顺序排队播放,避免重叠。
let ttsNextTime = 0; // 下一句应开始播放的 AudioContext 时间

function base64ToArrayBuffer(b64) {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes.buffer;
}

async function playTTSAudio(b64) {
  if (!audioContext || !b64) return;
  try {
    const buf = await audioContext.decodeAudioData(base64ToArrayBuffer(b64));
    const src = audioContext.createBufferSource();
    src.buffer = buf;
    src.connect(audioContext.destination);
    const now = audioContext.currentTime;
    const startAt = Math.max(now, ttsNextTime);
    src.start(startAt);
    ttsNextTime = startAt + buf.duration; // 紧接上一句,顺序播放
  } catch (e) {
    console.error("[offscreen] TTS 播放失败", e);
  }
}

function scheduleReconnect() {
  if (!running) return;
  if (reconnectTimer) return;
  const delay = reconnectDelay;
  console.warn(`[offscreen] WS 断开,${delay}ms 后重连`);
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    reconnectDelay = Math.min(reconnectDelay * RECONNECT.multiplier, RECONNECT.maxMs);
    connectBackend();
  }, delay);
}

function connectBackend() {
  ws = new WebSocket(BACKEND_WS_URL);
  ws.binaryType = "arraybuffer";

  ws.onopen = () => {
    reconnectDelay = RECONNECT.initialMs; // 成功后重置退避。
    // 告知后端音频参数,便于上游 ASR 配置。
    ws.send(
      JSON.stringify({
        type: "start",
        audio: {
          sampleRate: TARGET_SAMPLE_RATE,
          bitDepth: 16,
          channels: 1,
          format: "pcm_s16le",
        },
      })
    );
    console.info("[offscreen] WS 已连接");
  };

  ws.onmessage = (ev) => {
    if (typeof ev.data !== "string") return;
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return;
    }
    // 译文语音:直接在 offscreen 解码播放(不必经 background/页面)。
    if (msg.type === "tts_audio") {
      playTTSAudio(msg.audio);
      return;
    }
    // 后端回传的字幕 / 翻译错误事件 -> 经常驻 Port 交给 background 转发到页面 overlay。
    // 注意:用 channel 作路由键,绝不能复用 msg.target(那是译文,展开后会覆盖路由)。
    if (msg.type === "subtitle" || msg.type === "translate_error") {
      sendSubtitleToBackground({ channel: "page-subtitle", ...msg });
    }
  };

  ws.onerror = () => {
    // 错误后通常会触发 onclose,由 onclose 统一安排重连。
  };

  ws.onclose = () => {
    ws = null;
    scheduleReconnect();
  };
}

async function start(streamId) {
  if (running) stop(); // 幂等:先清理旧会话。
  running = true;
  reconnectDelay = RECONNECT.initialMs;
  ttsNextTime = 0; // 重置译文语音播放时钟
  ensurePort(); // 建立与 SW 的常驻连接(保活 + 字幕通道)
  connectBackend();

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
  sourceNode = audioContext.createMediaStreamSource(mediaStream);
  // 关键:保持把标签页声音播放给用户(否则会静音)。
  sourceNode.connect(audioContext.destination);

  // 加载重采样 worklet。
  await audioContext.audioWorklet.addModule(chrome.runtime.getURL("audio-worklet.js"));
  workletNode = new AudioWorkletNode(audioContext, "pcm16k-downsampler");
  sourceNode.connect(workletNode);
  // 接到 destination 以确保该节点被音频图拉动而持续 process(输出为静音)。
  workletNode.connect(audioContext.destination);

  workletNode.port.onmessage = (e) => {
    // e.data 是一段 16kHz/16bit/单声道 PCM 的 ArrayBuffer。
    if (wsConnected()) {
      ws.send(e.data);
    }
    // 未连接时直接丢弃,避免积压、保持低延迟。
  };

  console.info("[offscreen] 采集已开始", { contextRate: audioContext.sampleRate });
}

function stop() {
  running = false;
  if (swPort) {
    try {
      swPort.disconnect();
    } catch {}
    swPort = null;
  }
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  if (workletNode) {
    workletNode.port.onmessage = null;
    workletNode.disconnect();
    workletNode = null;
  }
  if (sourceNode) {
    sourceNode.disconnect();
    sourceNode = null;
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
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "stop" }));
    } catch {}
    ws.onclose = null; // 主动停止,不再触发重连。
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
