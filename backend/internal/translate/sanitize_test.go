package translate

import "testing"

func TestSanitizeTranslationStripsNote(t *testing.T) {
	raw := "那些人。注：具体指代需结合上下文，可根据所指对象调整。"
	got := SanitizeTranslation(raw, "those people")
	if got != "那些人。" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeTranslationRejectsDictionary(t *testing.T) {
	raw := "需要结合具体语境哦，它常见的意思有：向上；朝上；起床；起来。"
	got := SanitizeTranslation(raw, "up")
	if got != "" {
		t.Fatalf("expected reject, got %q", got)
	}
}

func TestSanitizeTranslationKeepsNormal(t *testing.T) {
	raw := "好了，各位。"
	got := SanitizeTranslation(raw, "All right, everyone.")
	if got != "好了，各位。" {
		t.Fatalf("got %q", got)
	}
}
