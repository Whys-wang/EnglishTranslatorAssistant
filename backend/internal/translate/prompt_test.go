package translate

import (
	"strings"
	"testing"

	"simul-interpreter/internal/config"
)

func TestWriteDomainAndGlossary_Empty(t *testing.T) {
	defer restorePrompt(config.Prompt)
	config.Prompt = config.PromptConfig{Domain: "", Glossary: map[string]string{}}

	p := buildSystemPrompt("", "")
	if strings.Contains(p, "领域背景") || strings.Contains(p, "术语对照表") {
		t.Fatalf("空配置不应注入领域/术语段:\n%s", p)
	}
}

func TestWriteDomainAndGlossary_DomainAndSortedGlossary(t *testing.T) {
	defer restorePrompt(config.Prompt)
	config.Prompt = config.PromptConfig{
		Domain: "一场关于机器学习的技术分享",
		Glossary: map[string]string{
			"transformer": "Transformer 架构",
			"attention":   "注意力机制",
			"  ":          "应被忽略的空键",
		},
	}

	for _, p := range []string{buildSystemPrompt("", ""), buildReviewSystemPrompt(2, "")} {
		if !strings.Contains(p, "领域背景") || !strings.Contains(p, "一场关于机器学习的技术分享") {
			t.Fatalf("缺少领域背景:\n%s", p)
		}
		if !strings.Contains(p, "attention => 注意力机制") || !strings.Contains(p, "transformer => Transformer 架构") {
			t.Fatalf("缺少术语条目:\n%s", p)
		}
		// 字典序:attention 应排在 transformer 之前。
		if strings.Index(p, "attention =>") > strings.Index(p, "transformer =>") {
			t.Fatalf("术语未按字典序排列:\n%s", p)
		}
		if strings.Contains(p, "应被忽略的空键") {
			t.Fatalf("空白键不应出现:\n%s", p)
		}
	}
}

func restorePrompt(p config.PromptConfig) { config.Prompt = p }
