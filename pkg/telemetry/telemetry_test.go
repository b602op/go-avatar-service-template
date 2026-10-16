package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
)

// TestInit_CollectorUnavailable проверяет главное требование: недоступность
// коллектора (Jaeger) не мешает старту приложения — Init не возвращает ошибку,
// а экспортёр ретраит отправку в фоне.
func TestInit_CollectorUnavailable(t *testing.T) {
	// Порт, на котором заведомо никто не слушает.
	shutdown, err := Init(context.Background(), Config{
		Endpoint:       "http://localhost:14318",
		ServiceName:    "avatars-test",
		ServiceVersion: "test",
		Environment:    "test",
	})
	require.NoError(t, err)
	require.NotNil(t, shutdown)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx))
}

// TestInit_EndpointWithoutScheme проверяет, что endpoint без схемы
// (host:port, как ждёт otlptracehttp) тоже принимается.
func TestInit_EndpointWithoutScheme(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{
		Endpoint:    "localhost:14318",
		ServiceName: "avatars-test",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, shutdown(ctx))
}

// TestShutdown_SlowCollectorIsBounded проверяет сценарий «экспорт дольше
// бюджета остановки»: медленный коллектор не блокирует процесс навсегда —
// shutdown упирается в таймаут контекста и возвращает ошибку.
//
// Именно эту ошибку main() логирует как Warning (best-effort) и не включает
// в код возврата, иначе штатная остановка давала бы exit 1.
func TestShutdown_SlowCollectorIsBounded(t *testing.T) {
	// Коллектор, который принимает запрос и не отвечает, пока тест не разрешит.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	shutdown, err := Init(context.Background(), Config{
		Endpoint:    srv.URL,
		ServiceName: "avatars-test",
	})
	require.NoError(t, err)

	// Завершаем span: BSP поставит его в очередь, и остановка провайдера
	// попытается дослать батч.
	_, span := otel.Tracer("test").Start(context.Background(), "test-span")
	span.End()

	// Бюджет остановки заведомо меньше времени ответа коллектора.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, shutdown(ctx), context.DeadlineExceeded)
}
