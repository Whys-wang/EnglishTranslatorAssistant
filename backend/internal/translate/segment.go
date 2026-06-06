package translate

import "time"

// StreamSegmentPolicy 控制「边说边译」何时切句、何时开始预览翻译。
type StreamSegmentPolicy struct {
	MinRunes           int
	MaxRunes           int
	PartialMinTail     int
	PartialMinChars    int           // 预览重译最小增量;0=用全局配置
	PartialMinInterval time.Duration // 预览节流;0=用全局配置
	PartialPromoteWait     time.Duration // 定稿时等待在途预览;0=用全局配置
	PartialCancelGrow      int           // 原文变化几个字就取消慢预览;0=用默认阈值
	PromotePartialOnFinal  bool          // 定稿时是否秒升预览(仅 CJK 等需要;英语关闭)
	SkipBackgroundRefine   bool          // 秒升定稿后不再后台 Pro 精修(仅日语等需提速时)
	AllowWeakSplit         bool
	AllowSpaceSplit    bool
	DisableClauseFlush bool // true=整句一条字幕,预览/定稿共用 segment_id
}
// SegmentPolicyFor 按源语言(或从原文推断)返回切分策略。
func SegmentPolicyFor(source, srcLang string) StreamSegmentPolicy {
	lang := NormalizeLang(srcLang)
	if lang == "" {
		lang = DetectPrimaryLanguage(source)
	}
	switch lang {
	case "日语":
		return StreamSegmentPolicy{
			MinRunes: 10, MaxRunes: 0,
			PartialMinTail: 1, PartialMinChars: 1,
			PartialMinInterval: 35 * time.Millisecond,
			PartialPromoteWait: 400 * time.Millisecond,
			PartialCancelGrow:  1,
			PromotePartialOnFinal: true,
			SkipBackgroundRefine:  true,
			DisableClauseFlush: true,
		}
	case "韩语":
		return StreamSegmentPolicy{
			MinRunes: 10, MaxRunes: 0,
			PartialMinTail: 3, PartialMinChars: 1,
			PartialMinInterval: 60 * time.Millisecond,
			PartialPromoteWait: 450 * time.Millisecond,
			PartialCancelGrow:  1,
			PromotePartialOnFinal: true,
			AllowWeakSplit: true, AllowSpaceSplit: true,
			DisableClauseFlush: true,
		}
	case "中文", "粤语":
		return StreamSegmentPolicy{
			MinRunes: 10, MaxRunes: 0,
			PartialMinTail: 2, PartialMinChars: 1,
			PartialMinInterval: 50 * time.Millisecond,
			PartialPromoteWait: 450 * time.Millisecond,
			PartialCancelGrow:  1,
			PromotePartialOnFinal: true,
			AllowWeakSplit: true, AllowSpaceSplit: false,
			DisableClauseFlush: true,
		}
	case "俄语":
		return StreamSegmentPolicy{
			MinRunes: 18, MaxRunes: 0, PartialMinTail: 20,
			AllowWeakSplit: false, AllowSpaceSplit: true,
			DisableClauseFlush: true,
		}
	case "法语", "德语", "西班牙语", "英语":
		// 英语等:保持用户验证过的「完美」参数,不跟 CJK 优化混用。
		return StreamSegmentPolicy{
			MinRunes: 20, MaxRunes: 0, PartialMinTail: 20,
			PartialMinChars: 10, PartialMinInterval: 250 * time.Millisecond,
			AllowWeakSplit: false, AllowSpaceSplit: true,
			DisableClauseFlush: true,
		}
	default:
		return StreamSegmentPolicy{
			MinRunes: 20, MaxRunes: 96, PartialMinTail: 15,
			AllowWeakSplit: false, AllowSpaceSplit: true,
		}
	}
}

// DetectPrimaryLanguage 从文本脚本比例推断主要语言(用于自动检测源语时的切分策略)。
func DetectPrimaryLanguage(text string) string {
	type score struct {
		lang string
		n    int
	}
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
		case unicodeIsHan(r):
			han++
		default:
			if !unicodeIsSpaceOrPunctOrDigit(r) {
				other++
			}
		}
	}
	scores := []score{
		{"日语", kana + han/2},
		{"韩语", hangul},
		{"俄语", cyrillic},
		{"中文", han},
		{"英语", latin},
	}
	best := "英语"
	max := 0
	for _, s := range scores {
		if s.n > max {
			max = s.n
			best = s.lang
		}
	}
	if max == 0 {
		return "英语"
	}
	if kana > 0 && kana*2 >= han {
		return "日语"
	}
	return best
}

func unicodeIsHan(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FFF
}

func unicodeIsSpaceOrPunctOrDigit(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' ||
		(r >= '0' && r <= '9') ||
		r == '.' || r == ',' || r == '!' || r == '?' || r == ';' || r == ':'
}
