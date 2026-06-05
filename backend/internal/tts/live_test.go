package tts

// 联机测试:真实调用火山 TTS 双向流式 V3,人工验证协议与合成。
// 默认跳过,需显式 TTS_LIVE=1 才运行;音色用 TTS_VOICE 指定(默认取一个文档示例音色)。
//
//	TTS_LIVE=1 TTS_VOICE=zh_female_xxx_bigtts go test ./internal/tts/ -run TestLive -v

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"log/slog"
)

func TestLiveSynthesize(t *testing.T) {
	if os.Getenv("TTS_LIVE") != "1" {
		t.Skip("设置 TTS_LIVE=1 运行联机 TTS 验证")
	}
	voice := os.Getenv("TTS_VOICE")
	if voice == "" {
		voice = "zh_male_M392_conversation_wvae_bigtts" // 文档示例音色,可能未在本账号开通
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClient(log)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	audio, format, err := c.synthesizeWith(ctx, "你好,这是一段语音合成测试。", voice)
	if err != nil {
		t.Fatalf("合成失败(voice=%s): %v", voice, err)
	}
	t.Logf("合成成功:voice=%s format=%s bytes=%d", voice, format, len(audio))
	if len(audio) < 200 {
		t.Fatalf("音频过短(%d 字节),疑似异常", len(audio))
	}

	out := filepath.Join(os.TempDir(), "tts_sample."+format)
	if err := os.WriteFile(out, audio, 0o644); err != nil {
		t.Logf("写文件失败(忽略): %v", err)
	} else {
		t.Logf("已写出样本音频: %s", out)
	}
}
