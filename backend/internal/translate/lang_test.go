package translate

import (
	"strings"
	"testing"
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

func TestBuildSystemPrompt_TargetLanguageRule(t *testing.T) {
	p := buildSystemPrompt("中文", "英语")
	for _, sub := range []string{"全部使用中文", "严禁夹杂", "最终字幕只能是中文"} {
		if !strings.Contains(p, sub) {
			t.Fatalf("缺少 %q:\n%s", sub, p)
		}
	}
}
