package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

// newTestLogger собирает логгер с тем же хендлером, что и New, но пишущий
// в буфер — чтобы проверять содержимое JSON-записей.
func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(&traceAwareHandler{
		baseHandler: slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}),
	})
}

func TestTraceAwareHandler_AddsTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)

	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	require.NoError(t, err)

	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	}))

	log.InfoContext(ctx, "with span")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.Equal(t, traceID.String(), record["trace_id"])
	assert.Equal(t, spanID.String(), record["span_id"])
}

func TestTraceAwareHandler_NoSpanNoIDs(t *testing.T) {
	var buf bytes.Buffer
	log := newTestLogger(&buf)

	log.Info("without span")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	assert.NotContains(t, record, "trace_id")
	assert.NotContains(t, record, "span_id")
}

func TestTraceAwareHandler_WrappersKeepType(t *testing.T) {
	h := &traceAwareHandler{baseHandler: slog.NewJSONHandler(io.Discard, nil)}

	// WithAttrs/WithGroup должны сохранять обёртку, иначе автокорреляция
	// trace_id теряется после первого .With(...).
	assert.IsType(t, &traceAwareHandler{}, h.WithAttrs([]slog.Attr{slog.String("service", "server")}))
	assert.IsType(t, &traceAwareHandler{}, h.WithGroup("group"))
}
