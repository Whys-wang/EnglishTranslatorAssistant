# AI 同声传译助手(火山引擎版)

把单向外语音频流实时翻译成**中文双语字幕**(可选 TTS 语音)的助手。核心难点是**低延迟**与**自动纠正此前的识别/翻译错误**。

> 当前进度:里程碑 4 —— 接入方舟翻译(final 分句异步翻译、上下文窗口、回填 segment)。

## 架构

```
┌──────────────────────────── Chrome 扩展 (MV3) ────────────────────────────┐
│  popup.js ──start/stop──▶ background.js ──streamId──▶ offscreen.js         │
│                                                         │                  │
│                                       tabCapture 音频 ──▶ AudioWorklet      │
│                                       重采样 16k/16bit/单声道 PCM,~100ms 分片 │
│                                                         │ (binary frames)  │
│  content.js ◀── 字幕事件 ── background.js ◀── subtitle ──┘                  │
│  (页面 overlay 双语字幕)                                  │                  │
└─────────────────────────────────────────────────────────┼──────────────────┘
                                                           │ WebSocket /ws
                                                           ▼
┌──────────────────────────────── Go 后端中继 ─────────────────────────────┐
│  server  ──PCM──▶ 火山 ASR(流式)──partial/final/修订──▶ 分句               │
│            ──原文──▶ 方舟翻译(Ark, OpenAI 兼容)──译文──▶ 回填 segment_id     │
│            ──纠错:(a) ASR 修订重译  (b) 周期性 LLM 复审──▶ 原地更新           │
│            ──(可选)译文──▶ 火山 TTS 双向流式 V3 ──音频──▶ 前端播放            │
└────────────────────────────────────────────────────────────────────────┘
```

### 目录结构

```
.
├── backend/                  # Go 后端(WebSocket 中继 + 纠错逻辑)
│   ├── main.go
│   ├── go.mod
│   └── internal/
│       ├── config/           # ★ 所有密钥与可调参数写死在此(不读环境变量)
│       ├── logging/          # 结构化日志(记录 X-Tt-Logid)
│       └── server/           # WebSocket 中继骨架
└── extension/                # Chrome 扩展 (Manifest V3)
    ├── manifest.json
    ├── background.js         # Service Worker:控制 & 转发
    ├── offscreen.html/.js    # 音频采集 + WebSocket 推流
    ├── audio-worklet.js      # 16kHz PCM 重采样(里程碑 2)
    ├── content.js + overlay.css  # 页面字幕 overlay
    └── popup.html/.js        # 开始/停止控制
```

## 配置(写死,不用环境变量)

所有密钥与参数都在 `backend/internal/config/config.go`,以常量/包级变量形式写死:

- **ASR**:`ASRAppKey`、`ASRAccessKey`、`ASRResourceID=volc.bigasr.sauc.duration`
- **TTS**:与 ASR 共用鉴权,`TTSResourceID=volc.service_type.10029`、`TTSVoiceType`
- **翻译(方舟 Ark)**:`ArkAPIKey`、`ArkModel`、`ArkEndpoint`
- **纠错策略**:`Correction`(开关、`ReviewContextWindow=5`、`ReviewInterval=3s` 等)
- **重连**:`Reconnect`(指数退避)

> ⚠️ 运行前请把 `PLEASE_FILL_*` 占位符替换为控制台中的真实值。

## 如何跑

### 后端

```bash
cd backend
go mod tidy        # 拉取 gorilla/websocket、google/uuid
go run .           # 监听 0.0.0.0:8765,WS 路径 /ws,健康检查 /healthz
```

健康检查:

```bash
curl http://localhost:8765/healthz
```

### 前端扩展

1. Chrome 打开 `chrome://extensions`,开启「开发者模式」。
2. 「加载已解压的扩展程序」,选择本仓库的 `extension/` 目录。
3. 打开任意带音频的标签页,点击扩展图标 →「开始翻译」。

点击「开始」后:offscreen 连上后端 `ws://localhost:8765/ws`,发送 `start`(含音频参数),并开始用 AudioWorklet 把标签页音频重采样为 16kHz/16bit/单声道 PCM、按 ~100ms 分片以二进制帧持续发送;断线会指数退避自动重连。后端日志(`audio received`)会打印累计帧数、字节数与按 16kHz 估算的音频时长,可据此核对采样率是否正确。

> 翻译(里程碑 4):每当一句被 ASR 定稿(final),后端会带上最近 `Translate.ContextWindow` 段「原文 => 译文」作为上下文,异步调用方舟(Ark)翻译,再以相同 `segment_id` 下发一条带 `target` 的 `subtitle` 事件原地回填译文。**需要先把 `config.go` 中的 `ArkAPIKey` 与 `ArkModel` 填成真实值**,否则后端会打印 `translation disabled` 警告并只显示原文。

## 如何演示

(里程碑 9 补充完整演示脚本。)当前可演示:启动后端 → 加载扩展 → 开始 → 后端结构化日志显示客户端连接、控制消息与音频帧统计。

## 开发计划

1. [x] chore: 项目脚手架(前后端目录、写死配置、README 骨架)
2. [x] feat: 前端音频采集与分片(tabCapture→16k PCM→WebSocket)
3. [x] feat: 接入火山 ASR 流式(二进制协议解析,partial/final)
4. [x] feat: 接入方舟翻译(分句、上下文窗口、回填 segment)
5. [ ] feat: 双语字幕 UI(partial 灰显、final 定稿、原地更新)
6. [ ] feat: 纠错能力(ASR 修订重译 + 周期性 LLM 复审)
7. [ ] feat: 接入火山 TTS 双向流式(可开关)
8. [ ] feat: 术语表/领域提示词 + perf: 延迟优化与重连
9. [ ] docs: 演示脚本与使用文档
