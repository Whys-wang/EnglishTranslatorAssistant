package translate

import "testing"

func TestSplitCompletedClauses(t *testing.T) {
	clauses, locked := SplitCompletedClauses("Hello world. This is next.", "", 4, 24)
	if len(clauses) != 2 {
		t.Fatalf("got clauses=%v", clauses)
	}
	if clauses[0] != "Hello world." || clauses[1] != "This is next." {
		t.Fatalf("unexpected clauses=%v", clauses)
	}
	if locked != "Hello world. This is next." {
		t.Fatalf("locked=%q", locked)
	}

	clauses, locked = SplitCompletedClauses("Hello world. This is next.", locked, 4, 24)
	if len(clauses) != 0 {
		t.Fatalf("expected no new clauses, got %v", clauses)
	}
	if RemainingClauseTail("Hello world. This is next.", locked) != "" {
		t.Fatal("tail should be empty")
	}
}

func TestSplitCompletedClausesLongNoPunct(t *testing.T) {
	src := "this is a very long sentence without strong punctuation at all here and it keeps going on and on without any period"
	clauses, locked := SplitCompletedClauses(src, "", 4, 80)
	if len(clauses) == 0 {
		t.Fatal("expected forced split")
	}
	if RemainingClauseTail(src, locked) == "" && len(clauses) < 2 {
		t.Fatalf("expected tail or multiple clauses, locked=%q clauses=%v", locked, clauses)
	}
}

func TestSplitCompletedClausesNoCommaSplit(t *testing.T) {
	src := "Good morning everyone, today we will talk about something important."
	p := SegmentPolicyFor(src, "英语")
	clauses, locked := SplitCompletedClausesWithPolicy(src, "", p)
	if len(clauses) != 1 {
		t.Fatalf("expected one clause at period, got %v locked=%q", clauses, locked)
	}
	if clauses[0] != src {
		t.Fatalf("unexpected clause=%q", clauses[0])
	}
}

func TestRemainingClauseTailPartial(t *testing.T) {
	src := "First part done. Still talking"
	clauses, locked := SplitCompletedClauses(src, "", 4, 24)
	if len(clauses) != 1 || clauses[0] != "First part done." {
		t.Fatalf("clauses=%v locked=%q", clauses, locked)
	}
	if tail := RemainingClauseTail(src, locked); tail != "Still talking" {
		t.Fatalf("tail=%q", tail)
	}
}
