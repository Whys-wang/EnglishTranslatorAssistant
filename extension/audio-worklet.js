// audio-worklet.js —— 在音频线程中把任意采样率的单声道音频
// 重采样为 16kHz/16bit PCM,并按约 100ms 分片通过 port 发送。
//
// 里程碑 1:占位定义,真正的重采样与分片在里程碑 2 完善。

class PCM16kDownsampler extends AudioWorkletProcessor {
  constructor() {
    super();
    this.targetRate = 16000;
    this.chunkMs = 100;
    // TODO(里程碑 2): 累积重采样后的样本,凑满 ~100ms 后 postMessage(ArrayBuffer)。
  }

  process(inputs) {
    // const input = inputs[0];
    // ... 线性插值/抽取重采样到 16kHz,转 Int16,分片 postMessage ...
    return true; // 保持处理器存活
  }
}

registerProcessor("pcm16k-downsampler", PCM16kDownsampler);
