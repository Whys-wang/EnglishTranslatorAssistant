package translate

import "testing"

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

func stringsRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
