// Package logger предоставляет обёртку над log/slog.
package logger

import (
	"log/slog"
	"os"
)

// New создаёт slog.Logger с JSON-хендлером, пишущим в os.Stdout.
// Уровень приходит уже распарсенным (см. config.getEnvLogLevel).
// Каждая запись получает атрибут service, а также trace_id/span_id, если
// в контексте есть активный span (см. traceAwareHandler).
func New(level slog.Level, serviceName string) *slog.Logger {
	base := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	h := &traceAwareHandler{baseHandler: base}
	return slog.New(h).With("service", serviceName)
}
