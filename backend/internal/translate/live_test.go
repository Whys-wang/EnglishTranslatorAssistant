package translate

// 这是一个「联机」测试:真实调用方舟(Ark)接口,用于人工验证翻译与复审纠错。
// 默认跳过,需显式设置环境变量 ARK_LIVE=1 才会运行(避免普通 CI / go test 误打接口)。
//
//	ARK_LIVE=1 go test ./internal/translate/ -run TestLive -v

import (
	"context"
	"os"
	"testing"
	"time"

	"log/slog"
)

func liveLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestLiveTranslateAndReview(t *testing.T) {
	if os.Getenv("ARK_LIVE") != "1" {
		t.Skip("设置 ARK_LIVE=1 运行联机 Ark 验证")
	}
	if !Configured() {
		t.Fatal("Ark 未配置(ArkAPIKey/ArkModel 仍是占位符)")
	}

	c := NewClient(liveLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1) 基础翻译验证。
	out, err := c.Translate(ctx, "The patient was prescribed a beta blocker for hypertension.", nil)
	if err != nil {
		t.Fatalf("Translate 失败: %v", err)
	}
	t.Logf("Translate => %q", out)
	if out == "" {
		t.Fatal("译文为空")
	}

	// 2) 复审纠错验证:第 3 句给一个明显错译(把 apple 误成「苹果公司」),
	//    看复审是否:① 返回等长 JSON 数组(证明解析正确);② 能借上下文纠正。
	items := []ReviewItem{
		{Source: "He works at Apple.", Target: "他在苹果公司工作。"},
		{Source: "The keynote starts at 10am.", Target: "主题演讲上午十点开始。"},
		{Source: "I ate an apple before the meeting.", Target: "我在会议前吃了一个苹果公司。"},
	}
	rev, err := c.Review(ctx, items)
	if err != nil {
		t.Fatalf("Review 失败: %v", err)
	}
	if len(rev) != len(items) {
		t.Fatalf("Review 长度不符: got %d want %d", len(rev), len(items))
	}
	for i := range items {
		t.Logf("Review[%d] 原文=%q 旧译=%q 新译=%q", i, items[i].Source, items[i].Target, rev[i])
	}
	if rev[2] == items[2].Target {
		t.Logf("提示: 第 3 句未被改写(模型选择保留),不视为失败")
	}
}
