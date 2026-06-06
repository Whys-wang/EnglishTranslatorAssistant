package translate

import (
	"strings"
	"testing"

	"simul-interpreter/internal/config"
)

func TestNormalizeLang(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"auto", ""},
		{"自动检测", ""},
		{"中文", "中文"},
		{"Chinese", "中文"},
		{"zh-CN", "中文"},
		{"英语", "英语"},
		{"English", "英语"},
	}
	for _, c := range cases {
		if got := NormalizeLang(c.in); got != c.want {
			t.Fatalf("NormalizeLang(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShouldPassthrough(t *testing.T) {
	if !ShouldPassthrough("今天天气不错", "中文", "中文") {
		t.Fatal("源/目标同为中文应直通")
	}
	if ShouldPassthrough("Hello world", "英语", "中文") {
		t.Fatal("英译中不应直通")
	}
	if !ShouldPassthrough("这是一段中文语音识别结果", "", "中文") {
		t.Fatal("自动识别+中文目标,中文原文应直通")
	}
	if ShouldPassthrough("This is English speech", "", "中文") {
		t.Fatal("自动识别+中文目标,英文原文不应直通")
	}
}

func TestASRLanguageCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"英语", "en-US"},
		{"中文", "zh-CN"},
		{"日语", "ja-JP"},
		{"韩语", "ko-KR"},
		{"俄语", "ru-RU"},
		{"法语", "fr-FR"},
		{"德语", "de-DE"},
		{"西班牙语", "es-MX"},
		{"粤语", "yue-CN"},
		{"", ""},
		{"自动检测", ""},
	}
	for _, c := range cases {
		if got := ASRLanguageCode(c.in); got != c.want {
			t.Fatalf("ASRLanguageCode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestASRUsesAsyncDefault(t *testing.T) {
	for _, lang := range []string{"英语", "中文", "", "自动检测", "English", "auto"} {
		if !ASRUsesAsyncDefault(lang) {
			t.Fatalf("ASRUsesAsyncDefault(%q) should be true", lang)
		}
	}
	for _, lang := range []string{"日语", "韩语", "俄语", "法语", "德语", "西班牙语", "粤语"} {
		if ASRUsesAsyncDefault(lang) {
			t.Fatalf("ASRUsesAsyncDefault(%q) should be false", lang)
		}
	}
}

func TestASRNeedsNostream(t *testing.T) {
	for _, lang := range []string{"日语", "韩语", "俄语", "法语", "德语", "西班牙语", "粤语"} {
		if !ASRNeedsNostream(lang) {
			t.Fatalf("ASRNeedsNostream(%q) should be true", lang)
		}
	}
	for _, lang := range []string{"英语", "中文", "", "自动检测"} {
		if ASRNeedsNostream(lang) {
			t.Fatalf("ASRNeedsNostream(%q) should be false", lang)
		}
	}
}

func TestASREndWindowSizeJapaneseOnly(t *testing.T) {
	if got := ASREndWindowSize("日语"); got != 75 {
		t.Fatalf("日语 end_window=%d, want 75", got)
	}
	if got := ASREndWindowSize("法语"); got != 82 {
		t.Fatalf("法语 end_window=%d, want 82", got)
	}
	if got := ASREndWindowSize("德语"); got != 82 {
		t.Fatalf("德语 end_window=%d, want 82", got)
	}
	if got := ASREndWindowSize("俄语"); got != 82 {
		t.Fatalf("俄语 end_window=%d, want 82", got)
	}
	ko := ASREndWindowSize("韩语")
	if ko != config.ASRRequest.EndWindowSizeNostream {
		t.Fatalf("韩语 end_window=%d, want nostream default %d", ko, config.ASRRequest.EndWindowSizeNostream)
	}
	if got := ASREndWindowSize("英语"); got != config.ASRRequest.EndWindowSize {
		t.Fatalf("英语 end_window=%d, want async %d", got, config.ASRRequest.EndWindowSize)
	}
}

func TestBuildSystemPrompt_TargetLanguageRule(t *testing.T) {
	p := buildSystemPrompt("中文", "英语")
	for _, sub := range []string{"全部使用中文", "严禁夹杂", "最终字幕只能是中文"} {
		if !strings.Contains(p, sub) {
			t.Fatalf("缺少 %q:\n%s", sub, p)
		}
	}
}
