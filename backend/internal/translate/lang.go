package translate

import (
	"strings"
	"unicode"

	"simul-interpreter/internal/config"
)

// NormalizeLang 把前端/配置里的语言名规范成统一写法(空串表示自动识别)。
func NormalizeLang(lang string) string {
	lang = strings.TrimSpace(lang)
	if lang == "" || lang == "自动检测" || strings.EqualFold(lang, "auto") {
		return ""
	}
	key := strings.ToLower(lang)
	aliases := map[string]string{
		"中文": "中文", "汉语": "中文", "国语": "中文",
		"chinese": "中文", "zh": "中文", "zh-cn": "中文", "zh-hans": "中文",
		"英语": "英语", "英文": "英语",
		"english": "英语", "en": "英语", "en-us": "英语",
		"日语": "日语", "日文": "日语",
		"japanese": "日语", "ja": "日语",
		"韩语": "韩语", "韩文": "韩语",
		"korean": "韩语", "ko": "韩语",
		"法语": "法语", "法文": "法语",
		"french": "法语", "fr": "法语",
		"德语": "德语", "德文": "德语",
		"german": "德语", "de": "德语",
		"西班牙语": "西班牙语", "西语": "西班牙语",
		"spanish": "西班牙语", "es": "西班牙语",
		"俄语": "俄语", "俄文": "俄语",
		"russian": "俄语", "ru": "俄语",
		"粤语": "粤语",
		"cantonese": "粤语", "yue": "粤语",
	}
	if c, ok := aliases[key]; ok {
		return c
	}
	if c, ok := aliases[lang]; ok {
		return c
	}
	return lang
}

// LangsEquivalent 判断两种语言设置是否实质相同。
func LangsEquivalent(a, b string) bool {
	na, nb := NormalizeLang(a), NormalizeLang(b)
	return na != "" && nb != "" && na == nb
}

// ShouldPassthrough 判断是否应直接把 ASR 原文作为字幕(不调用 LLM)。
// 用于源/目标相同,或自动识别时原文已是目标语言,避免模型混出其他语言。
func ShouldPassthrough(source, srcLang, tgtLang string) bool {
	source = strings.TrimSpace(source)
	if source == "" {
		return false
	}
	tgt := NormalizeLang(tgtLang)
	if tgt == "" {
		return false
	}
	if LangsEquivalent(srcLang, tgtLang) {
		return true
	}
	if NormalizeLang(srcLang) != "" {
		return false
	}
	return textPrimarilyInLanguage(source, tgt)
}

// ASRLanguageCode 把统一语言名映射为火山 ASR audio.language 代码(空串=默认中英文模型)。
// 注意:audio.language 仅在 bigmodel_nostream 端点生效。
func ASRLanguageCode(lang string) string {
	switch NormalizeLang(lang) {
	case "中文":
		return "zh-CN"
	case "英语":
		return "en-US"
	case "日语":
		return "ja-JP"
	case "韩语":
		return "ko-KR"
	case "法语":
		return "fr-FR"
	case "德语":
		return "de-DE"
	case "西班牙语":
		return "es-MX"
	case "俄语":
		return "ru-RU"
	case "粤语":
		return "yue-CN"
	default:
		return ""
	}
}

// ASRUsesAsyncDefault 英语/中文/自动检测走 bigmodel_async + enable_nonstream(英→中同款)。
func ASRUsesAsyncDefault(srcLang string) bool {
	switch NormalizeLang(srcLang) {
	case "", "英语", "中文":
		return true
	default:
		return false
	}
}

// ASRNeedsNostream 非英/中/自动的显式源语言走 bigmodel_nostream + audio.language。
func ASRNeedsNostream(srcLang string) bool {
	return !ASRUsesAsyncDefault(srcLang)
}

// ASREndWindowSize 返回 ASR 判停(ms)。仅对指定语种微调;英/韩/中等其余语种走 config 默认。
// 判停过短会在句中停顿处过早 definite,导致一句拆成多条字幕;法德俄西略拉长以整句输出。
func ASREndWindowSize(srcLang string) int {
	switch NormalizeLang(srcLang) {
	case "日语":
		return 60
	case "韩语":
		return 175
	case "俄语":
		// 此前 280ms 过长;碎段由 MergeShortUtterances 兜底,判停可与法德接近。
		return 160
	case "法语":
		return 145
	case "德语", "西班牙语":
		return 130
	}
	if ASRNeedsNostream(srcLang) {
		if config.ASRRequest.EndWindowSizeNostream > 0 {
			return config.ASRRequest.EndWindowSizeNostream
		}
	}
	if config.ASRRequest.EndWindowSize > 0 {
		return config.ASRRequest.EndWindowSize
	}
	return 0
}

func textPrimarilyInLanguage(text, lang string) bool {
	var han, kana, hangul, cyrillic, latin, other int
	for _, r := range text {
		switch {
		case isKana(r):
			kana++
		case isHangul(r):
			hangul++
		case isCyrillic(r):
			cyrillic++
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			latin++
		case unicode.Is(unicode.Han, r):
			han++
		default:
			if !unicode.IsSpace(r) && !unicode.IsPunct(r) && !unicode.IsDigit(r) {
				other++
			}
		}
	}
	total := han + kana + hangul + cyrillic + latin + other
	if total == 0 {
		return false
	}
	ratio := func(n int) float64 { return float64(n) / float64(total) }

	switch lang {
	case "中文", "粤语":
		return ratio(han) > 0.45 && ratio(kana) < 0.15
	case "日语":
		return ratio(kana) > 0.08 || (ratio(han)+ratio(kana)) > 0.5
	case "韩语":
		return ratio(hangul) > 0.35
	case "俄语":
		return ratio(cyrillic) > 0.35
	case "英语", "法语", "德语", "西班牙语":
		return ratio(latin) > 0.45
	default:
		return false
	}
}

func isKana(r rune) bool {
	return unicode.In(r, unicode.Hiragana, unicode.Katakana)
}

func isHangul(r rune) bool {
	return unicode.Is(unicode.Hangul, r)
}

func isCyrillic(r rune) bool {
	return unicode.Is(unicode.Cyrillic, r)
}

func hasKana(text string) bool {
	for _, r := range text {
		if isKana(r) {
			return true
		}
	}
	return false
}
