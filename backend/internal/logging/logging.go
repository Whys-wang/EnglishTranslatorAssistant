// Package logging 提供项目统一的结构化日志。
//
// 基于标准库 log/slog,输出 JSON 行,便于后续采集与排错。
// 特别地,排错时务必记录火山返回的 X-Tt-Logid。
package logging

import (
	"log/slog"
	"os"
)

// New 返回一个写到标准错误的 JSON 结构化 logger。
func New() *slog.Logger {
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	return slog.New(handler)
}

// WithLogid 在日志中附带火山的 X-Tt-Logid,方便和火山侧排错对齐。
func WithLogid(l *slog.Logger, logid string) *slog.Logger {
	return l.With(slog.String("x_tt_logid", logid))
}
