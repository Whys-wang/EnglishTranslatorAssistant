// audio-worklet.js —— 在音频线程中把上下文采样率的单声道音频
// 重采样为 16kHz / 16bit PCM,并按约 100ms 分片通过 port 发送(ArrayBuffer)。
//
// 设计要点:
//   - 输入是 AudioContext 采样率(常见 44.1k/48k)的 Float32,每个 quantum 128 帧。
//   - 用线性插值降采样到 16kHz;跨 process 调用保持读取位置连续,避免拼接处爆音。
//   - 累积满 1 个分片(默认 100ms = 1600 样本 = 3200 字节)就 postMessage 一次。

const TARGET_RATE = 16000;
const CHUNK_MS = 100;
const CHUNK_SAMPLES = (TARGET_RATE * CHUNK_MS) / 1000; // 1600

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
  }

  flush() {
    // 拷贝一份发送,缓冲复用。
    const frame = this.out.slice(0, this.outPos);
    this.port.postMessage(frame.buffer, [frame.buffer]);
    this.outPos = 0;
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
