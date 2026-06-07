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
	SkipPartialPreview     bool          // true=定稿前不推预览,避免碎词闪烁
	MergeShortUtterances   bool          // true=把 ASR 连续短定稿并入上一条(俄语等)
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
			PartialMinInterval: 22 * time.Millisecond,
			PartialPromoteWait: 240 * time.Millisecond,
			PartialCancelGrow:  1,
			PromotePartialOnFinal: true,
			SkipBackgroundRefine:  true,
			DisableClauseFlush: true,
		}
	case "韩语":
		return StreamSegmentPolicy{
			MinRunes: 10, MaxRunes: 0,
			PartialMinTail: 5, PartialMinChars: 1,
			PartialMinInterval: 38 * time.Millisecond,
			PartialPromoteWait: 320 * time.Millisecond,
			PartialCancelGrow:  2,
			PromotePartialOnFinal: true,
			AllowWeakSplit: true, AllowSpaceSplit: true,
			DisableClauseFlush: true,
		}
	case "中文", "粤语":
		return StreamSegmentPolicy{
			MinRunes: 10, MaxRunes: 0,
			PartialMinTail: 2, PartialMinChars: 1,
			PartialMinInterval: 42 * time.Millisecond,
			PartialPromoteWait: 360 * time.Millisecond,
			PartialCancelGrow:  1,
			PromotePartialOnFinal: true,
			AllowWeakSplit: true, AllowSpaceSplit: false,
			DisableClauseFlush: true,
		}
	case "俄语":
		// 俄语→中:碎段合并兜底可读;预览参数对齐法德,避免过慢。
		return StreamSegmentPolicy{
			MinRunes: 16, MaxRunes: 0,
			PartialMinTail: 8, PartialMinChars: 2,
			PartialMinInterval: 55 * time.Millisecond,
			PartialPromoteWait: 260 * time.Millisecond,
			PartialCancelGrow:  2,
			MergeShortUtterances:  true,
			PromotePartialOnFinal: true,
			AllowWeakSplit: false, AllowSpaceSplit: true,
			DisableClauseFlush: true,
		}
	case "法语":
		// 法语→中:碎段合并 + 预览秒升;略快于德/西,减少短句两行。
		return StreamSegmentPolicy{
			MinRunes: 16, MaxRunes: 0,
			PartialMinTail: 7, PartialMinChars: 2,
			PartialMinInterval: 50 * time.Millisecond,
			PartialPromoteWait: 260 * time.Millisecond,
			PartialCancelGrow:  2,
			MergeShortUtterances:  true,
			PromotePartialOnFinal: true,
			AllowWeakSplit: false, AllowSpaceSplit: true,
			DisableClauseFlush: true,
		}
	case "德语":
		// 德→中:整句单条,预览跟嘴,定稿秒升后 Pro 精修。
		return StreamSegmentPolicy{
			MinRunes: 18, MaxRunes: 0,
			PartialMinTail: 8, PartialMinChars: 2,
			PartialMinInterval: 55 * time.Millisecond,
			PartialPromoteWait: 300 * time.Millisecond,
			PartialCancelGrow:  2,
			PromotePartialOnFinal: true,
			AllowWeakSplit: false, AllowSpaceSplit: true,
			DisableClauseFlush: true,
		}
	case "英语":
		// 英→中:已验收,勿改。
		return StreamSegmentPolicy{
			MinRunes: 20, MaxRunes: 0, PartialMinTail: 20,
			PartialMinChars: 10, PartialMinInterval: 250 * time.Millisecond,
			AllowWeakSplit: false, AllowSpaceSplit: true,
			DisableClauseFlush: true,
		}
	case "西班牙语":
		// 西语→中:与法德同档,整句单条 + 预览秒升 + 后台精修。
		return StreamSegmentPolicy{
			MinRunes: 16, MaxRunes: 0,
			PartialMinTail: 8, PartialMinChars: 2,
			PartialMinInterval: 55 * time.Millisecond,
			PartialPromoteWait: 300 * time.Millisecond,
			PartialCancelGrow:  2,
			PromotePartialOnFinal: true,
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
	if latin > 0 && best == "英语" {
		if guessed := detectLatinLanguage(text); guessed != "" {
			return guessed
		}
	}
	return best
}

// detectLatinLanguage 根据变音符号等特征区分法/德/西(自动检测时避免一律走英语策略)。
func detectLatinLanguage(text string) string {
	var fr, de, es int
	for _, r := range text {
		switch r {
		case 'é', 'è', 'ê', 'ë', 'à', 'â', 'î', 'ï', 'ô', 'ù', 'û', 'ç', 'œ', 'æ',
			'É', 'È', 'Ê', 'À', 'Â', 'Î', 'Ô', 'Ù', 'Ç':
			fr++
		case 'ä', 'ö', 'ü', 'ß', 'Ä', 'Ö', 'Ü':
			de++
		case 'ñ', 'Ñ', '¿', '¡':
			es++
		}
	}
	if fr >= 2 && fr >= de && fr >= es {
		return "法语"
	}
	if de >= 2 && de >= fr && de >= es {
		return "德语"
	}
	if es >= 1 && es >= fr && es >= de {
		return "西班牙语"
	}
	return ""
}

func unicodeIsHan(r rune) bool {
	return r >= 0x4E00 && r <= 0x9FFF
}

func unicodeIsSpaceOrPunctOrDigit(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' ||
		(r >= '0' && r <= '9') ||
		r == '.' || r == ',' || r == '!' || r == '?' || r == ';' || r == ':'
}
