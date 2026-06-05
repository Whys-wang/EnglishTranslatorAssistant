// Package config 集中存放本项目的全部配置。
//
// 按照需求,所有密钥与可调参数都以「常量 / 包级变量」的形式写死在本文件中,
// 不读取环境变量,也不读取 .env 文件。需要换 Key 时,直接修改本文件即可。
//
// 安全提示:本文件包含明文密钥,请勿将真实密钥推送到公共仓库。
package config

import "time"

// ───────────────────────────────────────────────────────────────────────────
// ASR —— 火山引擎「大模型流式语音识别」(BigModel SAUC)
// ───────────────────────────────────────────────────────────────────────────
const (
	// UseNewConsoleAuth 控制鉴权方式:
	//   true  = 新版控制台:仅需 X-Api-Key(见 SpeechAPIKey),ASR/TTS 共用;
	//   false = 旧版控制台:使用 X-Api-App-Key + X-Api-Access-Key。
	UseNewConsoleAuth = true

	// SpeechAPIKey 新版控制台的统一鉴权 Key(请求头 X-Api-Key),语音识别/合成共用。
	// 仅当 UseNewConsoleAuth = true 时使用。
	SpeechAPIKey = "PLEASE_FILL_SPEECH_API_KEY" // TODO: 填入真实 API Key

	// ASRAppKey 对应请求头 X-Api-App-Key(旧版控制台)。
	ASRAppKey = "PLEASE_FILL_ASR_APP_KEY" // TODO: 旧版控制台填入 App ID

	// ASRAccessKey 对应请求头 X-Api-Access-Key(旧版控制台)。
	ASRAccessKey = "PLEASE_FILL_ASR_ACCESS_KEY" // TODO: 旧版控制台填入 Access Token

	// ASRResourceID 对应请求头 X-Api-Resource-Id,固定值,无需改动。
	ASRResourceID = "volc.bigasr.sauc.duration"

	// ASRWebSocketURL 是大模型流式 ASR 的 WebSocket 接入地址。
	ASRWebSocketURL = "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel"

	// ASRModelName 模型名称,目前固定为 bigmodel。
	ASRModelName = "bigmodel"
)

// ASRRequestConfig 控制 ASR 识别行为(写死的默认值,详见火山文档)。
type ASRRequestConfig struct {
	EnableITN       bool   // 文本规范化(如「一九七零年」->「1970年」)
	EnablePunc      bool   // 标点
	EnableDDC       bool   // 语义顺滑(去口头语/重复)
	ShowUtterances  bool   // 输出分句信息(text/start_time/end_time/definite)
	ResultType      string // "full"=全量返回;"single"=增量返回
	EndWindowSize   int    // 强制判停时间(ms),静音超过即定稿;0 表示不显式设置
}

// ASRRequest 当前生效的 ASR 识别参数。
var ASRRequest = ASRRequestConfig{
	EnableITN:      true,
	EnablePunc:     true,
	EnableDDC:      false,
	ShowUtterances: true,
	ResultType:     "full",
	EndWindowSize:  800,
}

// ───────────────────────────────────────────────────────────────────────────
// TTS —— 火山引擎「大模型语音合成 双向流式 V3」
// (本里程碑仅占位,功能 7 再正式接入)
// ───────────────────────────────────────────────────────────────────────────
const (
	// TTSAppKey / TTSAccessKey 与 ASR 共用同一套鉴权。
	TTSAppKey    = ASRAppKey
	TTSAccessKey = ASRAccessKey

	// TTSResourceID 对应请求头 X-Api-Resource-Id,固定值,无需改动。
	TTSResourceID = "volc.service_type.10029"

	// TTSVoiceType 中文音色,取值来自控制台「音色列表」。
	TTSVoiceType = "PLEASE_FILL_TTS_VOICE_TYPE" // TODO: 填入真实中文音色

	// TTSHost / TTSPath 双向流式合成接入信息。
	TTSHost = "openspeech.bytedance.com"
	TTSPath = "/api/v3/tts/bidirection"
)

// ───────────────────────────────────────────────────────────────────────────
// 翻译 —— 火山方舟 Ark(OpenAI 兼容)
// ───────────────────────────────────────────────────────────────────────────
const (
	// ArkAPIKey 作为 Bearer Token 使用。
	ArkAPIKey = "PLEASE_FILL_ARK_API_KEY" // TODO: 填入真实 API Key

	// ArkModel 模型 / 接入点 ID(如 doubao 系列的 endpoint id 或模型名)。
	ArkModel = "PLEASE_FILL_ARK_MODEL_OR_ENDPOINT_ID" // TODO: 填入真实接入点/模型

	// ArkEndpoint Chat Completions 接口地址(如地域不同请自行修改)。
	ArkEndpoint = "https://ark.cn-beijing.volces.com/api/v3/chat/completions"
)

// ───────────────────────────────────────────────────────────────────────────
// 服务端 / 中继
// ───────────────────────────────────────────────────────────────────────────
const (
	// ListenAddr 后端 WebSocket 中继监听地址。
	ListenAddr = "0.0.0.0:8765"

	// ClientWSPath 前端扩展连接后端的 WebSocket 路径。
	ClientWSPath = "/ws"

	// HealthPath 健康检查路径。
	HealthPath = "/healthz"
)

// ───────────────────────────────────────────────────────────────────────────
// 翻译目标语言 & 分句
// ───────────────────────────────────────────────────────────────────────────
const (
	// TargetLanguage 目标语言,本项目固定翻译为中文。
	TargetLanguage = "中文"
)

// ───────────────────────────────────────────────────────────────────────────
// 纠错策略(本项目核心,做成可配置)
// ───────────────────────────────────────────────────────────────────────────

// CorrectionConfig 定义两层纠错能力的开关与参数。
type CorrectionConfig struct {
	// EnableASRRevision 开启「ASR 修订重译」:当 ASR 对已 final 的早先片段
	// 返回修订时,把该 segment 标记为 dirty 并重新翻译、原地替换。
	EnableASRRevision bool

	// EnablePeriodicReview 开启「周期性 LLM 复审」:带最近 N 段上下文,
	// 让 LLM 用后文澄清的信息校正早先译文。
	EnablePeriodicReview bool

	// ReviewContextWindow 复审滑动窗口大小(最近 N 段「原文+译文」)。
	ReviewContextWindow int

	// ReviewInterval 周期性复审的时间间隔(到句子边界或每隔该时间触发一次)。
	ReviewInterval time.Duration

	// OverwriteOnlyIfMoreConfident 仅当复审结果置信度更高时才覆盖旧译文。
	OverwriteOnlyIfMoreConfident bool
}

// Correction 当前生效的纠错配置(写死的默认值)。
var Correction = CorrectionConfig{
	EnableASRRevision:            true,
	EnablePeriodicReview:         true,
	ReviewContextWindow:          5,
	ReviewInterval:               3 * time.Second,
	OverwriteOnlyIfMoreConfident: true,
}

// ───────────────────────────────────────────────────────────────────────────
// 连接 / 重连 / 节流
// ───────────────────────────────────────────────────────────────────────────

// ReconnectConfig 断线自动重连(指数退避)参数。
type ReconnectConfig struct {
	InitialBackoff time.Duration // 初始退避
	MaxBackoff     time.Duration // 最大退避
	Multiplier     float64       // 退避倍数
	MaxRetries     int           // 最大重试次数,0 表示无限重试
}

// Reconnect 当前生效的重连配置。
var Reconnect = ReconnectConfig{
	InitialBackoff: 500 * time.Millisecond,
	MaxBackoff:     15 * time.Second,
	Multiplier:     2.0,
	MaxRetries:     0,
}

// ───────────────────────────────────────────────────────────────────────────
// 音频参数(需与前端采集保持一致)
// ───────────────────────────────────────────────────────────────────────────
const (
	AudioSampleRate = 16000 // 16kHz
	AudioBitDepth   = 16     // 16bit
	AudioChannels   = 1      // 单声道
	AudioChunkMs    = 100    // 约 100ms 一片
)
