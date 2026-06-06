package translate

import (
	"strings"
	"unicode"
)

// SplitCompletedClauses 从 source 中切出 locked 之后「已说完」的小句(遇强标点,或过长时在弱标点/空格处切)。
// 返回小句列表与新的 locked 前缀(不含尾部仍在说的片段)。
func SplitCompletedClauses(source, locked string, minRunes, maxRunes int) (clauses []string, newLocked string) {
	return SplitCompletedClausesWithPolicy(source, locked, StreamSegmentPolicy{
		MinRunes: minRunes, MaxRunes: maxRunes,
		AllowWeakSplit: false, AllowSpaceSplit: true,
	})
}

// SplitCompletedClausesWithPolicy 按语种策略切分(见 SegmentPolicyFor)。
func SplitCompletedClausesWithPolicy(source, locked string, policy StreamSegmentPolicy) (clauses []string, newLocked string) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, locked
	}
	if !strings.HasPrefix(source, locked) {
		locked = ""
	}
	rest := strings.TrimSpace(source[len(locked):])
	if rest == "" {
		return nil, locked
	}
	minRunes := policy.MinRunes
	if minRunes <= 0 {
		minRunes = 4
	}
	maxRunes := policy.MaxRunes

	runes := []rune(rest)
	start := 0
	for i := 0; i < len(runes); i++ {
		clauseLen := i - start + 1
		r := runes[i]
		if isStrongClauseEnd(r) && clauseLen >= minRunes {
			clauses = append(clauses, strings.TrimSpace(string(runes[start:i+1])))
			start = i + 1
			continue
		}
		if maxRunes > 0 && clauseLen >= maxRunes {
			split := findClauseSplit(runes[start:i+1], minRunes, policy)
			if split <= 0 {
				split = clauseLen
			}
			clauses = append(clauses, strings.TrimSpace(string(runes[start:start+split])))
			start = start + split
			i = start - 1
		}
	}
	if start > 0 {
		newLocked = locked + string(runes[:start])
	} else {
		newLocked = locked
	}
	return clauses, strings.TrimSpace(newLocked)
}

// RemainingClauseTail 返回 locked 之后仍在说、尚未切出去的部分。
func RemainingClauseTail(source, locked string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return ""
	}
	if !strings.HasPrefix(source, locked) {
		return source
	}
	return strings.TrimSpace(source[len(locked):])
}

func isStrongClauseEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '；', '.', '!', '?', ';':
		return true
	default:
		return false
	}
}

func isWeakClauseEnd(r rune) bool {
	switch r {
	case '，', ',', '：', ':', '、':
		return true
	default:
		return false
	}
}

func findClauseSplit(runes []rune, minRunes int, policy StreamSegmentPolicy) int {
	if len(runes) <= minRunes {
		return len(runes)
	}
	for i := len(runes) - 1; i >= minRunes; i-- {
		if isStrongClauseEnd(runes[i]) {
			return i + 1
		}
	}
	if policy.AllowWeakSplit {
		for i := len(runes) - 1; i >= minRunes; i-- {
			if isWeakClauseEnd(runes[i]) {
				return i + 1
			}
		}
	}
	if policy.AllowSpaceSplit {
		for i := len(runes) - 1; i >= minRunes; i-- {
			if unicode.IsSpace(runes[i]) {
				return i + 1
			}
		}
	}
	return len(runes)
}
