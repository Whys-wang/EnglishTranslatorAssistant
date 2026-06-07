package translate

import (
	"testing"
	"time"
)

func TestSegmentPolicyEnglishNoMidSplit(t *testing.T) {
	p := SegmentPolicyFor("Good morning everyone, today we talk.", "英语")
	if p.MaxRunes != 0 {
		t.Fatalf("English should not force mid-sentence split, max=%d", p.MaxRunes)
	}
	if !p.DisableClauseFlush {
		t.Fatal("English should use single-line clause flush")
	}
	if p.PromotePartialOnFinal {
		t.Fatal("English must not promote partial on final")
	}
	src := "Good morning everyone, today we will talk about something important."
	clauses, _ := SplitCompletedClausesWithPolicy(src, "", p)
	if len(clauses) != 1 || clauses[0] != src {
		t.Fatalf("expected single sentence clause, got %v", clauses)
	}
}

func TestSegmentPolicyJapaneseSingleLine(t *testing.T) {
	p := SegmentPolicyFor("皆さん、こんにちは。", "日语")
	if !p.DisableClauseFlush {
		t.Fatal("Japanese should use single-line mode like English")
	}
	if p.PartialMinTail > 6 {
		t.Fatalf("JP partial tail too high: %d", p.PartialMinTail)
	}
}

func TestSegmentPolicyJapaneseNoMidSplit(t *testing.T) {
	p := SegmentPolicyFor("皆さん、こんにちは", "日语")
	long := stringsRepeat("皆さん、こんにちは、", 8)
	clauses, locked := SplitCompletedClausesWithPolicy(long, "", p)
	if len(clauses) != 0 {
		t.Fatalf("JP should not mid-flush without strong punct, got %v locked=%q", clauses, locked)
	}
	if RemainingClauseTail(long, locked) != long {
		t.Fatalf("tail=%q", RemainingClauseTail(long, locked))
	}
}

func TestSegmentPolicyKoreanOptimized(t *testing.T) {
	p := SegmentPolicyFor("안녕하세요", "韩语")
	if !p.PromotePartialOnFinal || !p.DisableClauseFlush {
		t.Fatalf("韩语应保持整句单条+秒升: %+v", p)
	}
	if p.PartialMinInterval != 38*time.Millisecond {
		t.Fatalf("韩语预览间隔=%v, want 38ms", p.PartialMinInterval)
	}
	if p.PartialMinTail < 4 {
		t.Fatalf("韩语预览尾部长度应>=4: %d", p.PartialMinTail)
	}
}

func TestDetectPrimaryLanguage(t *testing.T) {
	if got := DetectPrimaryLanguage("皆さんこんにちは"); got != "日语" && got != "中文" {
		// 纯汉字可能被标为中文;含假名时应为日语
	}
	if got := DetectPrimaryLanguage("こんにちは"); got != "日语" {
		t.Fatalf("kana => %q", got)
	}
	if got := DetectPrimaryLanguage("안녕하세요"); got != "韩语" {
		t.Fatalf("hangul => %q", got)
	}
	if got := DetectPrimaryLanguage("Привет мир"); got != "俄语" {
		t.Fatalf("cyrillic => %q", got)
	}
}

func TestSegmentPolicyRussianLowLatency(t *testing.T) {
	p := SegmentPolicyFor("Я сейчас говорю по-русски о семейном подкасте.", "俄语")
	if !p.DisableClauseFlush || !p.PromotePartialOnFinal || !p.MergeShortUtterances {
		t.Fatalf("俄语应整句+秒升+碎段合并: %+v", p)
	}
	if p.SkipPartialPreview {
		t.Fatal("俄语应开启预览以降低延迟")
	}
	if p.PartialMinInterval != 55*time.Millisecond {
		t.Fatalf("俄语预览间隔=%v, want 55ms", p.PartialMinInterval)
	}
}

func TestSegmentPolicyFrenchMerge(t *testing.T) {
	p := SegmentPolicyFor("Je parle lentement en français.", "法语")
	if !p.MergeShortUtterances || !p.PromotePartialOnFinal || !p.DisableClauseFlush {
		t.Fatalf("法语应整句+合并+秒升: %+v", p)
	}
	if p.PartialMinInterval != 50*time.Millisecond {
		t.Fatalf("法语预览间隔=%v, want 50ms", p.PartialMinInterval)
	}
}

func TestSegmentPolicyEuropeanPromote(t *testing.T) {
	for _, lang := range []string{"德语", "西班牙语"} {
		p := SegmentPolicyFor("test source text here.", lang)
		if !p.DisableClauseFlush || !p.PromotePartialOnFinal {
			t.Fatalf("%s should use single-line + promote, got %+v", lang, p)
		}
		if p.PartialMinTail >= 20 {
			t.Fatalf("%s partial tail too high: %d", lang, p.PartialMinTail)
		}
		if p.PartialMinInterval > 100*time.Millisecond {
			t.Fatalf("%s preview interval too slow: %v", lang, p.PartialMinInterval)
		}
	}
}

func TestDetectLatinLanguage(t *testing.T) {
	if got := DetectPrimaryLanguage("C'était très bien, merci."); got != "法语" {
		t.Fatalf("french => %q", got)
	}
	if got := DetectPrimaryLanguage("Schöne Grüße aus München."); got != "德语" {
		t.Fatalf("german => %q", got)
	}
	if got := DetectPrimaryLanguage("¿Cómo estás hoy?"); got != "西班牙语" {
		t.Fatalf("spanish => %q", got)
	}
}

func stringsRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
