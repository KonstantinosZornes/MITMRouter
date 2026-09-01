package reqid

import (
	"context"
	"log/slog"
)

// Handler 为带请求上下文的结构化日志添加 req_id。
// 后台任务没有对应请求，因此刻意不添加该字段。
type Handler struct {
	next slog.Handler
}

// NewHandler 使用请求 ID 增强功能包装 next。
func NewHandler(next slog.Handler) *Handler { return &Handler{next: next} }

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	if id := From(ctx); id != "" {
		record.AddAttrs(slog.String("req_id", id))
	}
	return h.next.Handle(ctx, record)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name)}
}
