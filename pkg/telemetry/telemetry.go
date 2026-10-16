// Package telemetry инициализирует OpenTelemetry SDK: трейсинг через OTLP HTTP.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// ShutdownTimeout — бюджет на досыл накопленного батча спанов при остановке.
//
// Равен дефолтному таймауту экспорта OTLP HTTP (env OTEL_EXPORTER_OTLP_TIMEOUT,
// 10000 мс): меньший бюджет обрывал бы штатный экспорт, который ещё не сдался
// сам, а больший без пользы затягивал бы остановку процесса.
const ShutdownTimeout = 10 * time.Second

// Config — параметры инициализации трейсинга.
type Config struct {
	// Endpoint — адрес OTLP HTTP коллектора, например http://localhost:4318.
	Endpoint string
	// ServiceName — логическое имя сервиса (атрибут service.name).
	ServiceName string
	// ServiceVersion — версия сервиса (атрибут service.version).
	ServiceVersion string
	// Environment — окружение (атрибут deployment.environment.name).
	Environment string
}

// Init инициализирует глобальный OTel TracerProvider с OTLP HTTP exporter.
//
// Возвращает shutdown-функцию для graceful-останова: она досылает накопленный
// батч спанов и останавливает фоновый экспортёр.
//
// Ошибка возвращается только если не удалось создать resource или exporter
// (например, невалидный endpoint). Недоступность коллектора (Jaeger) ошибкой
// не считается: соединение устанавливается лениво, а неудачный экспорт батча
// приложение не роняет — ошибка уходит в SetErrorHandler, то есть в наш slog.
// Повторы экспортёр делает только для «ретраибельных» ответов (429/502/503/504,
// с учётом Retry-After); отказ соединения возвращается сразу, без ретраев.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			semconv.DeploymentEnvironmentNameKey.String(cfg.Environment),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create otel resource: %w", err)
	}

	// otlptracehttp.WithEndpoint ожидает host:port БЕЗ схемы, поэтому
	// "http://localhost:4318" приводим к "localhost:4318".
	endpoint := cfg.Endpoint
	if u, parseErr := url.Parse(cfg.Endpoint); parseErr == nil && u.Host != "" {
		endpoint = u.Host
	}

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		// Локальный Jaeger принимает OTLP по HTTP без TLS.
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter: %w", err)
	}

	// Осознанно используем стандартный BatchSpanProcessor (WithBatcher без
	// опций): интервал flush по умолчанию — 5 секунд. Это компромисс между
	// свежестью трейсов и нагрузкой на коллектор: спаны уходят пачками,
	// а не отдельным запросом на каждый span.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)

	// Ошибки самого SDK (например, неудачная отправка батча) направляем в наш
	// slog, чтобы они попадали в общий JSON-лог, а не терялись в stderr.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("otel error", "error", err)
	}))

	return func(ctx context.Context) error { return tp.Shutdown(ctx) }, nil
}
