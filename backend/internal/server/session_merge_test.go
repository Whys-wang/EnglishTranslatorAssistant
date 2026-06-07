package server

import (
	"testing"
)

func TestTryMergeRussianUtterance(t *testing.T) {
	s := &session{
		srcLang:  "俄语",
		tgtLang:  "中文",
		segments: map[string]*segState{},
		order:    []string{},
		lastSent: map[string]string{},
	}
	s.segments["seg-1"] = &segState{
		id: "seg-1", source: "Где я", translated: true,
		startTime: 0, endTime: 1000, seq: 0,
	}
	s.order = append(s.order, "seg-1")

	id, src, _, _, ok := s.tryMergeUtterance("seg-2", "говорю медленно", 1100, 1800)
	if !ok || id != "seg-1" {
		t.Fatalf("merge failed: ok=%v id=%s", ok, id)
	}
	if src != "Где я говорю медленно" {
		t.Fatalf("merged source=%q", src)
	}
	st := s.segments["seg-1"]
	if st.translated || !st.revised {
		t.Fatalf("merged seg should retranslate: translated=%v revised=%v", st.translated, st.revised)
	}
	if _, exists := s.segments["seg-2"]; exists {
		t.Fatal("dropped seg-2 should be removed")
	}
}

func TestTryMergeFrenchUtterance(t *testing.T) {
	s := &session{
		srcLang:  "法语",
		segments: map[string]*segState{},
		order:    []string{"seg-1"},
		lastSent: map[string]string{},
	}
	s.segments["seg-1"] = &segState{
		id: "seg-1", source: "Je suis ici", translated: true,
		startTime: 0, endTime: 900,
	}

	id, src, _, _, ok := s.tryMergeUtterance("seg-2", "et je parle", 1000, 1600)
	if !ok || id != "seg-1" {
		t.Fatalf("merge failed: ok=%v id=%s", ok, id)
	}
	if src != "Je suis ici et je parle" {
		t.Fatalf("merged source=%q", src)
	}
}

func TestTryMergeSkipsLongGap(t *testing.T) {
	s := &session{
		srcLang:  "俄语",
		segments: map[string]*segState{},
		order:    []string{"seg-1"},
		lastSent: map[string]string{},
	}
	s.segments["seg-1"] = &segState{
		id: "seg-1", source: "Первая фраза.", translated: true,
		startTime: 0, endTime: 1000,
	}

	_, _, _, _, ok := s.tryMergeUtterance("seg-2", "Новая тема", 4000, 5000)
	if ok {
		t.Fatal("should not merge across long pause")
	}
}
