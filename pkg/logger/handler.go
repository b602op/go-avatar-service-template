package logger

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// traceAwareHandler оборачивает slog.Handler и добавляет к каждой записи
// trace_id/span_id активного span'а из контекста — автокорреляция логов
// и трейсов без ручной передачи идентификаторов.
type traceAwareHandler struct {
	baseHandler slog.Handler
}

// Проверяем на этапе компиляции, что хендлер реализует slog.Handler.
var _ slog.Handler = (*traceAwareHandler)(nil)

// Enabled делегирует решение базовому хендлеру.
func (h *traceAwareHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.baseHandler.Enabled(ctx, l)
}

// Handle дополняет запись trace_id/span_id, если в контексте есть валидный span.
func (h *traceAwareHandler) Handle(ctx context.Context, r slog.Record) error {
	// SpanContextFromContext (а не SpanFromContext): нужны только
	// идентификаторы, создавать no-op span не требуется.
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.baseHandler.Handle(ctx, r)
}

// WithAttrs сохраняет автокорреляцию при добавлении постоянных атрибутов.
func (h *traceAwareHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceAwareHandler{baseHandler: h.baseHandler.WithAttrs(attrs)}
}

// WithGroup сохраняет автокорреляцию при открытии группы.
func (h *traceAwareHandler) WithGroup(name string) slog.Handler {
	return &traceAwareHandler{baseHandler: h.baseHandler.WithGroup(name)}
}
