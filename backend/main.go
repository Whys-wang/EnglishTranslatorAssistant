// Command backend 是 AI 同声传译助手的后端 WebSocket 中继入口。
//
// 它串联火山 ASR -> 方舟翻译 -> 火山 TTS,并承载纠错逻辑。
// 里程碑 1 仅打通空链路:启动服务、接受前端连接、记录音频/控制消息。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"simul-interpreter/internal/logging"
	"simul-interpreter/internal/server"
)

func main() {
	log := logging.New()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(log)
	if err := srv.Run(ctx); err != nil {
		log.Error("server exited with error", "err", err)
		os.Exit(1)
	}
	log.Info("server stopped cleanly")
}
