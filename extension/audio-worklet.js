// audio-worklet.js —— 在音频线程中把上下文采样率的单声道音频
// 重采样为 16kHz / 16bit PCM,并按约 100ms 分片通过 port 发送(ArrayBuffer)。
//
// 设计要点:
//   - 输入是 AudioContext 采样率(常见 44.1k/48k)的 Float32,每个 quantum 128 帧。
//   - 用线性插值降采样到 16kHz;跨 process 调用保持读取位置连续,避免拼接处爆音。
//   - 累积满 1 个分片(默认 100ms = 1600 样本 = 3200 字节)就 postMessage 一次。
//   - 顺带在音频线程里跑一个极简 VAD:对每个分片算 RMS,连续静音时长达到
//     SILENCE_TRIGGER_MS 就发一条 `{type:"vad", event:"silence"}`(转入静音);
//     重新出现声音时发 `{type:"vad", event:"speech"}`(转出静音)。
//     这样上层 (offscreen → content) 不必再等 ASR 切句,就能"抓住说话人的停顿"
//     立刻让字幕谢幕,完全跳过 ASR 200-400ms 的内部判停延迟。

const TARGET_RATE = 16000;
const CHUNK_MS = 100;
const CHUNK_SAMPLES = (TARGET_RATE * CHUNK_MS) / 1000; // 1600

// VAD 阈值:本分片的 RMS(归一化到 [0,1])低于此值视为「无声」。
// 0.008 ≈ -42dB,在标签页音频上能可靠分辨「人停止说话」与「有人在低音量说话」。
// 太低 -> 把低声调说话判成静音(字幕会被误清);太高 -> 漏判微停顿。
const VAD_RMS_THRESHOLD = 0.008;
// 连续静音累计达到此时长才算「真静音」,触发 silence 事件。
// 240ms 大约是「自然句子停顿」的下限,再小会把单词与单词间的微缝隙也判成停顿。
const SILENCE_TRIGGER_MS = 240;

class PCM16kDownsampler extends AudioWorkletProcessor {
  constructor() {
    super();
    // sampleRate 是 worklet 全局,等于 AudioContext 采样率。
    this.ratio = sampleRate / TARGET_RATE;

    // 待重采样的输入样本(Float32),用普通数组累积,按需裁剪。
    this.pending = [];
    // 下一个输出样本在 pending 中的(可为小数的)读取位置。
    this.readPos = 0;

    // 输出分片缓冲(Int16),累满后发送。
    this.out = new Int16Array(CHUNK_SAMPLES);
    this.outPos = 0;

    // VAD 状态。
    this.silenceMs = 0;
    this.inSilence = false;
  }

  flush() {
    // 1) 算 RMS(基于本分片的 Int16,归一化到 [-1,1])。
    let acc = 0;
    for (let i = 0; i < this.outPos; i++) {
      const v = this.out[i] / 0x8000;
      acc += v * v;
    }
    const rms = this.outPos ? Math.sqrt(acc / this.outPos) : 0;

    // 2) 把 PCM 分片下发。先拷贝再 transfer,缓冲复用。
    const frame = this.out.slice(0, this.outPos);
    this.port.postMessage(frame.buffer, [frame.buffer]);
    this.outPos = 0;

    // 3) VAD 状态机:静音累计达阈值触发 silence;一旦回到有声立刻触发 speech。
    if (rms < VAD_RMS_THRESHOLD) {
      this.silenceMs += CHUNK_MS;
      if (!this.inSilence && this.silenceMs >= SILENCE_TRIGGER_MS) {
        this.inSilence = true;
        this.port.postMessage({ type: "vad", event: "silence" });
      }
    } else {
      if (this.inSilence) {
        this.port.postMessage({ type: "vad", event: "speech" });
      }
      this.inSilence = false;
      this.silenceMs = 0;
    }
  }

  process(inputs) {
    const input = inputs[0];
    if (!input || input.length === 0) {
      return true; // 没有输入,保持存活。
    }
    const channel = input[0]; // 单声道(若多声道,只取第 0 声道)。
    if (channel && channel.length) {
      for (let i = 0; i < channel.length; i++) this.pending.push(channel[i]);
    }

    // 线性插值降采样:在 pending 上以 ratio 步进取样。
    while (this.readPos + 1 < this.pending.length) {
      const idx = Math.floor(this.readPos);
      const frac = this.readPos - idx;
      const sample = this.pending[idx] * (1 - frac) + this.pending[idx + 1] * frac;

      // Float -> Int16(带 clamp)。
      let s = sample;
      if (s > 1) s = 1;
      else if (s < -1) s = -1;
      this.out[this.outPos++] = s < 0 ? s * 0x8000 : s * 0x7fff;

      if (this.outPos >= CHUNK_SAMPLES) this.flush();

      this.readPos += this.ratio;
    }

    // 丢弃已消费的输入样本,保留插值所需的尾部。
    const consumed = Math.floor(this.readPos);
    if (consumed > 0) {
      this.pending.splice(0, consumed);
      this.readPos -= consumed;
    }

    return true;
  }
}

registerProcessor("pcm16k-downsampler", PCM16kDownsampler);
