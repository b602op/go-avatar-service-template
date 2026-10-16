// Package config загружает конфигурацию из переменных окружения.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Config — корневая конфигурация приложения.
type Config struct {
	App      AppConfig
	HTTP     HTTPConfig
	Postgres PostgresConfig
	S3       S3Config
	RabbitMQ RabbitMQConfig

	// LogLevel — уровень логирования, уже распарсенный в slog.Level.
	LogLevel slog.Level

	// Observability: OpenTelemetry (трейсинг через OTLP HTTP → Jaeger).
	OTelEndpoint       string // OTEL_EXPORTER_OTLP_ENDPOINT, напр. http://localhost:4318
	OTelServiceName    string // OTEL_SERVICE_NAME; к нему добавляется суффикс -server/-worker
	OTelServiceVersion string // OTEL_SERVICE_VERSION
	OTelEnvironment    string // OTEL_ENVIRONMENT
}

// AppConfig — общие настройки приложения.
type AppConfig struct {
	Env string // local | prod
}

// HTTPConfig — настройки HTTP-сервера.
type HTTPConfig struct {
	Port         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// PostgresConfig — параметры подключения к PostgreSQL.
type PostgresConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DB       string
	SSLMode  string
}

// S3Config — параметры S3-совместимого хранилища.
type S3Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	UseSSL    bool
}

// RabbitMQConfig — параметры подключения к RabbitMQ.
type RabbitMQConfig struct {
	URL      string
	Exchange string

	// RetryTTL — сколько сообщение лежит в retry-очереди перед возвратом в
	// основную. Ноль означает значение по умолчанию (publisher.DefaultRetryTTL).
	// Вынесено в конфиг, чтобы интеграционные тесты могли использовать короткий TTL.
	RetryTTL time.Duration
}

// Load читает конфигурацию из переменных окружения и валидирует её.
//
// Поведение зависит от APP_ENV:
//   - local — для всех переменных подставляются дефолты (быстрый старт);
//   - prod  — обязательные переменные НЕ получают дефолтов, и если хотя бы
//     одна из них пустая, Load возвращает ошибку со списком ВСЕХ отсутствующих
//     переменных (errors.Join), а также проверяет корректность DSN.
//
// Load никогда не паникует: все проблемы парсинга и валидации возвращаются
// как объединённая ошибка.
func Load() (*Config, error) {
	var errs []error

	env := getEnv("APP_ENV", "local")
	isProd := env == "prod"

	// Типизированный парсинг с накоплением ошибок — до сборки конфига,
	// чтобы не потерять ошибку из-за короткого замыкания.
	readTimeout, err := getEnvDuration("HTTP_READ_TIMEOUT", 10*time.Second)
	if err != nil {
		errs = append(errs, err)
	}
	writeTimeout, err := getEnvDuration("HTTP_WRITE_TIMEOUT", 15*time.Second)
	if err != nil {
		errs = append(errs, err)
	}
	useSSL, err := getEnvBool("S3_USE_SSL", false)
	if err != nil {
		errs = append(errs, err)
	}
	// Значение по умолчанию продублировано литералом: пакет config не может
	// импортировать services/publisher (там обратный импорт config — цикл).
	// Держим 30s синхронно с publisher.DefaultRetryTTL.
	retryTTL, err := getEnvDuration("RABBITMQ_RETRY_TTL", 30*time.Second)
	if err != nil {
		errs = append(errs, err)
	}
	logLevel, err := getEnvLogLevel("LOG_LEVEL", slog.LevelInfo)
	if err != nil {
		errs = append(errs, err)
	}

	cfg := &Config{
		App: AppConfig{
			Env: env,
		},
		LogLevel:           logLevel,
		OTelEndpoint:       getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318"),
		OTelServiceName:    getEnv("OTEL_SERVICE_NAME", "gophprofile"),
		OTelServiceVersion: getEnv("OTEL_SERVICE_VERSION", "dev"),
		OTelEnvironment:    getEnv("OTEL_ENVIRONMENT", "dev"),
		HTTP: HTTPConfig{
			Port:         getEnv("HTTP_PORT", "8080"),
			ReadTimeout:  readTimeout,
			WriteTimeout: writeTimeout,
		},
		Postgres: PostgresConfig{
			Host:     requiredEnv("POSTGRES_HOST", "localhost", isProd),
			Port:     requiredEnv("POSTGRES_PORT", "5432", isProd),
			User:     requiredEnv("POSTGRES_USER", "avatar", isProd),
			Password: requiredEnv("POSTGRES_PASSWORD", "avatar", isProd),
			DB:       requiredEnv("POSTGRES_DB", "avatars", isProd),
			SSLMode:  getEnv("POSTGRES_SSLMODE", "disable"),
		},
		S3: S3Config{
			Endpoint:  requiredEnv("S3_ENDPOINT", "localhost:9000", isProd),
			AccessKey: requiredEnv("S3_ACCESS_KEY", "minioadmin", isProd),
			SecretKey: requiredEnv("S3_SECRET_KEY", "minioadmin", isProd),
			Bucket:    requiredEnv("S3_BUCKET", "avatars", isProd),
			Region:    getEnv("S3_REGION", "us-east-1"),
			UseSSL:    useSSL,
		},
		RabbitMQ: RabbitMQConfig{
			URL:      requiredEnv("RABBITMQ_URL", "amqp://guest:guest@localhost:5672/", isProd),
			Exchange: requiredEnv("RABBITMQ_EXCHANGE", "avatars.exchange", isProd),
			RetryTTL: retryTTL,
		},
	}

	// Проверяем числовой формат портов (пустые значения обработает validateProd).
	if err := validatePort("HTTP_PORT", cfg.HTTP.Port); err != nil {
		errs = append(errs, err)
	}
	if err := validatePort("POSTGRES_PORT", cfg.Postgres.Port); err != nil {
		errs = append(errs, err)
	}

	if isProd {
		errs = append(errs, validateProd(cfg)...)
		if err := cfg.Postgres.ValidateDSN(); err != nil {
			errs = append(errs, err)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	return cfg, nil
}

// requiredProdKeys — переменные, обязательные в prod (фиксированный порядок
// для предсказуемого вывода ошибок).
var requiredProdKeys = []string{
	"POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB",
	"S3_ENDPOINT", "S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_BUCKET",
	"RABBITMQ_URL", "RABBITMQ_EXCHANGE",
}

// validateProd проверяет, что все обязательные для prod переменные заданы.
// Возвращает список ошибок (не первую), чтобы разработчик сразу видел весь список.
func validateProd(cfg *Config) []error {
	values := map[string]string{
		"POSTGRES_HOST":     cfg.Postgres.Host,
		"POSTGRES_PORT":     cfg.Postgres.Port,
		"POSTGRES_USER":     cfg.Postgres.User,
		"POSTGRES_PASSWORD": cfg.Postgres.Password,
		"POSTGRES_DB":       cfg.Postgres.DB,
		"S3_ENDPOINT":       cfg.S3.Endpoint,
		"S3_ACCESS_KEY":     cfg.S3.AccessKey,
		"S3_SECRET_KEY":     cfg.S3.SecretKey,
		"S3_BUCKET":         cfg.S3.Bucket,
		"RABBITMQ_URL":      cfg.RabbitMQ.URL,
		"RABBITMQ_EXCHANGE": cfg.RabbitMQ.Exchange,
	}

	var errs []error
	for _, k := range requiredProdKeys {
		if strings.TrimSpace(values[k]) == "" {
			errs = append(errs, fmt.Errorf("%s is required in prod", k))
		}
	}
	return errs
}

// DSN собирает URL-строку подключения к PostgreSQL.
// Использует net/url — все спецсимволы в user/password экранируются автоматически.
func (c PostgresConfig) DSN() string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.User, c.Password),
		Host:   net.JoinHostPort(c.Host, c.Port),
		Path:   c.DB,
	}
	q := u.Query()
	q.Set("sslmode", c.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// ValidateDSN проверяет, что DSN корректно парсится и содержит критичные поля.
// Вызывается только для prod, чтобы не тянуть проверку в local-путь.
func (c PostgresConfig) ValidateDSN() error {
	parsed, err := pgx.ParseConfig(c.DSN())
	if err != nil {
		return fmt.Errorf("postgres: invalid DSN: %w", err)
	}
	if parsed.User == "" {
		return errors.New("postgres: user is empty after parsing DSN")
	}
	if parsed.Database == "" {
		return errors.New("postgres: database is empty after parsing DSN")
	}
	if parsed.Host == "" {
		return errors.New("postgres: host is empty after parsing DSN")
	}
	return nil
}

// String возвращает человекочитаемое представление конфига с маскировкой секретов.
func (c Config) String() string {
	return fmt.Sprintf(
		"Config{App={Env:%s}, LogLevel:%s, HTTP={Port:%s ReadTimeout:%s WriteTimeout:%s}, "+
			"Postgres={Host:%s Port:%s User:%s Password:%s DB:%s SSLMode:%s}, "+
			"S3={Endpoint:%s Bucket:%s Region:%s AccessKey:%s SecretKey:%s UseSSL:%t}, "+
			"RabbitMQ={URL:%s Exchange:%s}, "+
			"OTel={Endpoint:%s ServiceName:%s ServiceVersion:%s Environment:%s}}",
		c.App.Env, c.LogLevel,
		c.HTTP.Port, c.HTTP.ReadTimeout, c.HTTP.WriteTimeout,
		c.Postgres.Host, c.Postgres.Port, c.Postgres.User, mask(c.Postgres.Password),
		c.Postgres.DB, c.Postgres.SSLMode,
		c.S3.Endpoint, c.S3.Bucket, c.S3.Region, mask(c.S3.AccessKey), mask(c.S3.SecretKey), c.S3.UseSSL,
		maskURL(c.RabbitMQ.URL), c.RabbitMQ.Exchange,
		c.OTelEndpoint, c.OTelServiceName, c.OTelServiceVersion, c.OTelEnvironment,
	)
}

// ─── хелперы ────────────────────────────────────────────────────────────────

// getEnv возвращает значение переменной окружения или дефолт, если она пустая.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// requiredEnv возвращает значение переменной окружения.
// Если переменная не задана, в local применяется дефолт, а в prod — пустая строка,
// чтобы validateProd зафиксировал её как отсутствующую обязательную.
func requiredEnv(key, localDefault string, isProd bool) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	if isProd {
		return ""
	}
	return localDefault
}

// getEnvDuration парсит duration из переменной окружения.
// Возвращает понятную ошибку с цитатой значения при невалидном формате.
func getEnvDuration(key string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def, fmt.Errorf("%s must be a valid duration (e.g. 10s), got %q", key, raw)
	}
	return d, nil
}

// getEnvLogLevel парсит уровень логирования в slog.Level.
// Пустое значение → def; невалидное — ошибка: лучше упасть на старте,
// чем молча логировать не тем уровнем, который просили.
func getEnvLogLevel(key string, def slog.Level) (slog.Level, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return def, fmt.Errorf("%s must be one of debug|info|warn|error, got %q", key, raw)
	}
}

// getEnvBool парсит bool из переменной окружения.
// Возвращает понятную ошибку с цитатой значения при невалидном формате.
func getEnvBool(key string, def bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def, fmt.Errorf("%s must be a boolean (true|false), got %q", key, raw)
	}
	return v, nil
}

// validatePort проверяет, что значение порта — целое число.
// Пустое значение пропускается: его наличие в prod проверяет validateProd.
func validatePort(key, value string) error {
	if value == "" {
		return nil
	}
	if _, err := strconv.Atoi(value); err != nil {
		return fmt.Errorf("%s must be an integer, got %q", key, value)
	}
	return nil
}

// mask прячет значение, оставляя первые 2 символа — для отладки.
func mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 2 {
		return "***"
	}
	return s[:2] + "***"
}

// maskURL маскирует userinfo в URL целиком: amqp://user:pass@host → amqp://***@host.
// Заменяем именно закодированную часть userinfo, чтобы не ломать схему/хост/путь.
func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return strings.Replace(raw, u.User.String(), "***", 1)
}
