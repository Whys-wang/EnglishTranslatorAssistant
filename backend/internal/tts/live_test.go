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
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 连续合成多句,验证「持久连接复用」:首句建连 + 连接级握手,
	// 后续句应在同一连接上仅做会话级流程(耗时通常更短)。
	sentences := []string{
		"你好,这是一段语音合成测试。",
		"第二句继续合成,验证连接是否被复用。",
		"第三句仍然使用同一条连接。",
	}
	var lastFormat string
	var firstAudio []byte
	for i, s := range sentences {
		t0 := time.Now()
		audio, format, err := c.synthesizeWith(ctx, s, voice)
		if err != nil {
			t.Fatalf("第 %d 句合成失败(voice=%s): %v", i+1, voice, err)
		}
		if len(audio) < 200 {
			t.Fatalf("第 %d 句音频过短(%d 字节),疑似异常", i+1, len(audio))
		}
		t.Logf("第 %d 句:format=%s bytes=%d 耗时=%s", i+1, format, len(audio), time.Since(t0))
		lastFormat = format
		if i == 0 {
			firstAudio = audio
		}
	}

	// 多句结束后连接应仍在(被复用而非每句新建)。
	c.mu.Lock()
	reused := c.conn != nil
	c.mu.Unlock()
	if !reused {
		t.Fatalf("期望持久连接在多句合成后仍存活(被复用),但 conn 为空")
	}
	t.Logf("连接复用验证通过:%d 句共用同一连接", len(sentences))

	out := filepath.Join(os.TempDir(), "tts_sample."+lastFormat)
	if err := os.WriteFile(out, firstAudio, 0o644); err != nil {
		t.Logf("写文件失败(忽略): %v", err)
	} else {
		t.Logf("已写出样本音频: %s", out)
	}
}
