package translate

import (
	"strings"
	"unicode"
)

// SanitizeTranslation 去掉模型偶发输出的注释/释义/词典说明,只保留字幕译文。
func SanitizeTranslation(raw, source string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// 多行时优先取第一行像正文的;后面若是注释则丢弃。
	if nl := strings.IndexByte(raw, '\n'); nl > 0 {
		rest := strings.TrimSpace(raw[nl+1:])
		if rest != "" && (looksLikeTranslationMeta(rest, source) || strings.Contains(rest, "注：")) {
			raw = strings.TrimSpace(raw[:nl])
		}
	}

	for _, cut := range []string{"注：", "（注", "(注", "说明：", "解释："} {
		if i := strings.Index(raw, cut); i > 0 {
			raw = strings.TrimSpace(raw[:i])
		}
	}

	raw = strings.Trim(raw, "\"'")
	for _, p := range []string{"译文：", "翻译：", "中文：", "英文：", "输出："} {
		raw = strings.TrimPrefix(raw, p)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || looksLikeTranslationMeta(raw, source) {
		return ""
	}
	return raw
}

func looksLikeTranslationMeta(text, source string) bool {
	markers := []string{
		"注：", "常见意思", "如果是单独", "通常翻译为", "需要结合", "具体指代",
		"原文不完整", "这个词是", "本身没有独立", "若补充完整", "可根据所指",
		"词典", "释义", "以上是根据", "常见的对应", "固定释义",
		"祈使语气", "系表结构", "动词短语", "表语",
	}
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	srcLen := len([]rune(strings.TrimSpace(source)))
	outLen := len([]rune(text))
	if srcLen > 0 && srcLen <= 12 && outLen > srcLen*5 {
		if strings.Count(text, "；") >= 2 || strings.Count(text, "。") >= 3 {
			return true
		}
	}
	// 整段像词条罗列:大量分号/序号且几乎无口语短句。
	if strings.Count(text, "；") >= 3 && outLen > 40 {
		return true
	}
	if strings.HasPrefix(text, "1.") || strings.HasPrefix(text, "①") {
		return true
	}
	// 纯元说明(无中文译文实质内容)。
	if srcLen <= 4 && outLen > 30 && !containsHan(text) && strings.Contains(text, "翻译") {
		return true
	}
	return false
}

func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
