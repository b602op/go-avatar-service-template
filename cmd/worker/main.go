package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/b602op/avatars-service/internal/config"
	"github.com/b602op/avatars-service/internal/migrator"
	"github.com/b602op/avatars-service/internal/repository/postgres"
	"github.com/b602op/avatars-service/internal/services/publisher"
	"github.com/b602op/avatars-service/internal/services/s3"
	"github.com/b602op/avatars-service/internal/worker"
	"github.com/b602op/avatars-service/pkg/logger"
	"github.com/b602op/avatars-service/pkg/resizer"
	"github.com/b602op/avatars-service/pkg/telemetry"
)

const (
	// initTimeout — время на инициализацию подключений (БД, S3, RabbitMQ).
	initTimeout = 15 * time.Second
	// migrationTimeout — бюджет на применение миграций. Отдельно от initTimeout:
	// подключение к БД укладывается в секунды, а миграции могут создавать
	// индексы и выполняться заметно дольше.
	migrationTimeout = 60 * time.Second
)

func main() {
	// .env необязателен: если файла нет — работаем с переменными окружения ОС.
	// Если файл есть, но битый — падаем громко с понятной ошибкой.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to load .env file", "error", err)
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	log := logger.New(cfg.LogLevel, "worker")
	// Ошибки OTel SDK пишутся через slog.Warn — без SetDefault они уйдут
	// в stderr, а не в наш JSON-лог.
	slog.SetDefault(log)
	log.Info("starting worker", "config", cfg.String())

	// Контекст отменяется по SIGINT/SIGTERM — запускает graceful shutdown.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Трейсинг: OTLP HTTP → Jaeger. Недоступность коллектора не фатальна:
	// соединение устанавливается лениво, а ошибки экспорта уходят в наш slog
	// через otel.SetErrorHandler.
	tpShutdown, err := telemetry.Init(ctx, telemetry.Config{
		Endpoint:       cfg.OTelEndpoint,
		ServiceName:    cfg.OTelServiceName + "-worker",
		ServiceVersion: cfg.OTelServiceVersion,
		Environment:    cfg.OTelEnvironment,
	})
	if err != nil {
		log.Error("failed to init telemetry", "error", err)
		os.Exit(1)
	}
	// Трейсинг останавливается последним по времени: defer выполняется по LIFO,
	// а этот зарегистрирован раньше остальных, поэтому к моменту flush воркер,
	// publisher и пул БД уже закрыты — их shutdown-спаны попадают в батч.
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(
			context.WithoutCancel(ctx),
			telemetry.ShutdownTimeout,
		)
		defer cancelShutdown()

		// Трейсы — best-effort телеметрия: неудачный flush не должен влиять
		// на код возврата и шуметь Error-уровнем при штатной остановке.
		if shutdownErr := tpShutdown(shutdownCtx); shutdownErr != nil {
			log.Warn("telemetry shutdown failed", "error", shutdownErr)
		}
	}()

	// Отдельный контекст для инициализации зависимостей.
	initCtx, initCancel := context.WithTimeout(ctx, initTimeout)
	defer initCancel()

	pool, err := postgres.NewPool(initCtx, cfg.Postgres.DSN())
	if err != nil {
		log.Error("failed to connect to postgres", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	log.Info("postgres connected")

	// Миграции применяются при старте
	// повторный запуск и параллельный старт server/worker безопасны.
	migCtx, cancelMig := context.WithTimeout(ctx, migrationTimeout)
	err = migrator.Up(migCtx, pool, log)
	cancelMig()
	if err != nil {
		log.Error("failed to apply migrations", "error", err)
		os.Exit(1)
	}
	log.Info("migrations applied")

	storage, err := s3.New(cfg.S3, log)
	if err != nil {
		log.Error("failed to create s3 client", "error", err)
		os.Exit(1)
	}
	if err := storage.EnsureBucket(initCtx); err != nil {
		log.Error("failed to ensure s3 bucket", "error", err)
		os.Exit(1)
	}
	log.Info("s3 bucket ready", "bucket", cfg.S3.Bucket)

	// Сборка слоёв: репозиторий → handler → publisher → cleanup → worker.
	repo := postgres.NewAvatarRepository(pool)
	handler := worker.NewHandler(repo, storage, worker.ResizerFunc(resizer.ResizeBatch), log)

	pub, err := publisher.NewRabbitMQ(cfg.RabbitMQ, log)
	if err != nil {
		log.Error("failed to create publisher", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := pub.Close(); err != nil {
			log.Error("failed to close publisher", "error", err)
		}
	}()

	// Outbox-lite: раз в минуту перепубликуем события для зависших pending-аватарок.
	cleanup := worker.NewCleanup(repo, pub, log, time.Minute, 2*time.Minute)

	w, err := worker.NewWorker(cfg.RabbitMQ, handler, cleanup, log)
	if err != nil {
		log.Error("failed to start worker", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := w.Close(); err != nil {
			log.Error("failed to close worker", "error", err)
		}
	}()
	log.Info("worker listening for events")

	if err := w.Run(ctx); err != nil {
		log.Error("worker stopped with error", "error", err)
		os.Exit(1)
	}
	log.Info("worker stopped gracefully")
}
