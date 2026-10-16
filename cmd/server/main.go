package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/b602op/avatars-service/internal/api"
	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/handlers"
	"github.com/b602op/avatars-service/internal/migrator"
	"github.com/b602op/avatars-service/internal/repository/postgres"
	"github.com/b602op/avatars-service/internal/services"
	"github.com/b602op/avatars-service/internal/services/publisher"
	"github.com/b602op/avatars-service/internal/services/s3"
	"github.com/b602op/avatars-service/pkg/logger"
	"github.com/b602op/avatars-service/pkg/telemetry"
)

const (
	// initTimeout — время на инициализацию подключений (БД, S3).
	initTimeout = 15 * time.Second
	// migrationTimeout — бюджет на применение миграций. Отдельно от initTimeout:
	// подключение к БД укладывается в секунды, а миграции могут создавать
	// индексы и выполняться заметно дольше.
	migrationTimeout = 60 * time.Second
	// shutdownTimeout — время на корректное завершение HTTP-сервера.
	shutdownTimeout = 30 * time.Second
	// idleTimeout — время удержания idle-соединений.
	idleTimeout = 60 * time.Second
	// readHeaderTimeout — защита от slowloris: клиент должен прислать заголовки
	// за это время, иначе соединение закрывается.
	readHeaderTimeout = 5 * time.Second
	// writeTimeout — верхняя граница времени записи ответа. С запасом больше
	// таймаута middleware (30s), чтобы большие файлы успели дойти до клиента.
	writeTimeout = 60 * time.Second
)

func main() {
	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// .env необязателен: если файла нет — работаем с переменными окружения ОС.
	// Если файл есть, но битый — падаем громко с понятной ошибкой.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to load .env file: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	log := logger.New(cfg.LogLevel, "server")
	slog.SetDefault(log)

	log.Info("starting server", "config", cfg.String())

	// Регистрируем обработку сигналов до инициализации зависимостей.
	signalCtx, stopSignals := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stopSignals()

	// Трейсинг: OTLP HTTP → Jaeger. Недоступность коллектора не фатальна:
	// соединение устанавливается лениво, а ошибки экспорта уходят в наш slog
	// через otel.SetErrorHandler.
	tpShutdown, err := telemetry.Init(signalCtx, telemetry.Config{
		Endpoint:       cfg.OTelEndpoint,
		ServiceName:    cfg.OTelServiceName + "-server",
		ServiceVersion: cfg.OTelServiceVersion,
		Environment:    cfg.OTelEnvironment,
	})
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	// Трейсинг останавливается последним по времени: defer выполняется по LIFO,
	// а этот зарегистрирован раньше остальных, поэтому к моменту flush уже
	// остановлены HTTP-сервер, пул БД и publisher — их shutdown-спаны попадают
	// в батч. Порядок менять не нужно.
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(
			context.WithoutCancel(signalCtx),
			telemetry.ShutdownTimeout,
		)
		defer cancelShutdown()

		// Трейсы — best-effort телеметрия: неудачный flush не должен
		// менять код возврата и провоцировать рестарт контейнера.
		if shutdownErr := tpShutdown(shutdownCtx); shutdownErr != nil {
			log.Warn("telemetry shutdown failed", "error", shutdownErr)
		}
	}()

	// Контекст только для инициализации зависимостей.
	initCtx, cancelInit := context.WithTimeout(context.Background(), initTimeout)
	defer cancelInit()

	// PostgreSQL.
	pool, err := postgres.NewPool(initCtx, cfg.Postgres.DSN())
	if err != nil {
		return fmt.Errorf("failed to connect to postgres: %w", err)
	}
	defer pool.Close()

	log.Info("postgres connected")

	// Миграции применяются при старте
	// повторный запуск и параллельный старт server/worker безопасны.
	migCtx, cancelMig := context.WithTimeout(signalCtx, migrationTimeout)
	err = migrator.Up(migCtx, pool, log)
	cancelMig()
	if err != nil {
		return fmt.Errorf("failed to apply migrations: %w", err)
	}

	log.Info("migrations applied")

	// S3-хранилище: клиент создаётся всегда, бакет гарантируется идемпотентно.
	storage, err := s3.New(cfg.S3, log)
	if err != nil {
		return fmt.Errorf("failed to create s3 client: %w", err)
	}

	if err := storage.EnsureBucket(initCtx); err != nil {
		return fmt.Errorf("failed to ensure s3 bucket: %w", err)
	}

	log.Info("s3 bucket ready", "bucket", cfg.S3.Bucket)

	// Паблишер событий: реальный RabbitMQ.
	pub, err := publisher.NewRabbitMQ(cfg.RabbitMQ, log)
	if err != nil {
		return fmt.Errorf("failed to create publisher: %w", err)
	}
	defer func() {
		if err := pub.Close(); err != nil {
			log.Error("failed to close RabbitMQ publisher", "error", err)
		}
	}()

	// Инициализация закончена — дальнейшая работа не должна зависеть
	// от таймаута, установленного для старта приложения.
	cancelInit()

	// Сборка слоёв: репозиторий → сервис → хендлер → роутер.
	repo := postgres.NewAvatarRepository(pool)
	svc := services.NewAvatarService(repo, storage, pub, log)
	avatarHandler := handlers.NewAvatarHandler(svc, log)

	// /health проверяет все внешние зависимости; собирается здесь, потому что
	// уровню api (роутеру) pool, storage и брокер недоступны.
	healthHandler := handlers.Health(pool, storage, pub.Conn(), log)

	router := api.NewRouter(api.RouterDeps{
		Log:            log,
		AvatarHandler:  avatarHandler,
		Health:         healthHandler,
		AllowedOrigins: []string{"*"},
	})

	srv := &http.Server{
		Addr:              ":" + cfg.HTTP.Port,
		Handler:           router,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	return serveHTTP(signalCtx, srv, log)
}

func serveHTTP(
	ctx context.Context,
	srv *http.Server,
	log *slog.Logger,
) error {
	// Буфер позволяет горутине сообщить результат Serve
	serveErr := make(chan error, 1)

	go func() {
		log.Info("http server listening", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		// До сигнала остановки сервер не должен завершаться сам.
		if err == nil {
			return errors.New("http server stopped unexpectedly")
		}
		return fmt.Errorf("http server stopped before shutdown: %w", err)

	case <-ctx.Done():
		log.Info("shutting down http server")
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		shutdownTimeout,
	)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		// Shutdown не успел дождаться активных запросов.
		// Принудительно закрываем соединения и дожидаемся завершения
		// горутины Serve, прежде чем run() начнёт освобождать зависимости.
		log.Error("graceful shutdown failed; forcing close", "error", err)

		closeErr := srv.Close()
		serveErr := <-serveErr

		return errors.Join(
			fmt.Errorf("shutdown http server: %w", err),
			wrapCloseError(closeErr),
			unexpectedServeError(serveErr),
		)
	}

	// Shutdown закрывает listener, поэтому Serve должен завершиться
	// с http.ErrServerClosed. Дожидаемся результата горутины.
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server failed during shutdown: %w", err)
	}

	log.Info("http server stopped gracefully")

	return nil
}

func wrapCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("force close http server: %w", err)
}

func unexpectedServeError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("http server serve error: %w", err)
}
