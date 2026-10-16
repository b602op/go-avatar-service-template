package config

import (
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allEnvKeys — полный список переменных окружения, которые читает Load.
// Используется в clearEnv, чтобы каждый тест стартовал с чистого окружения.
var allEnvKeys = []string{
	"APP_ENV", "LOG_LEVEL",
	"HTTP_PORT", "HTTP_READ_TIMEOUT", "HTTP_WRITE_TIMEOUT",
	"POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB", "POSTGRES_SSLMODE",
	"S3_ENDPOINT", "S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_BUCKET", "S3_REGION", "S3_USE_SSL",
	"RABBITMQ_URL", "RABBITMQ_EXCHANGE", "RABBITMQ_RETRY_TTL",
	"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_SERVICE_NAME", "OTEL_SERVICE_VERSION", "OTEL_ENVIRONMENT",
}

// clearEnv сбрасывает все переменные окружения, влияющие на Load.
// Используем t.Setenv (а не os.Setenv), чтобы Go автоматически восстановил
// исходные значения после теста.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range allEnvKeys {
		t.Setenv(key, "")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "local", cfg.App.Env)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)

	assert.Equal(t, "http://localhost:4318", cfg.OTelEndpoint)
	assert.Equal(t, "gophprofile", cfg.OTelServiceName)
	assert.Equal(t, "dev", cfg.OTelServiceVersion)
	assert.Equal(t, "dev", cfg.OTelEnvironment)

	assert.Equal(t, "8080", cfg.HTTP.Port)
	assert.Equal(t, 10*time.Second, cfg.HTTP.ReadTimeout)
	assert.Equal(t, 15*time.Second, cfg.HTTP.WriteTimeout)

	assert.Equal(t, "localhost", cfg.Postgres.Host)
	assert.Equal(t, "5432", cfg.Postgres.Port)
	assert.Equal(t, "avatar", cfg.Postgres.User)
	assert.Equal(t, "avatar", cfg.Postgres.Password)
	assert.Equal(t, "avatars", cfg.Postgres.DB)
	assert.Equal(t, "disable", cfg.Postgres.SSLMode)

	assert.Equal(t, "localhost:9000", cfg.S3.Endpoint)
	assert.Equal(t, "minioadmin", cfg.S3.AccessKey)
	assert.Equal(t, "minioadmin", cfg.S3.SecretKey)
	assert.Equal(t, "avatars", cfg.S3.Bucket)
	assert.Equal(t, "us-east-1", cfg.S3.Region)
	assert.False(t, cfg.S3.UseSSL)

	assert.Equal(t, "amqp://guest:guest@localhost:5672/", cfg.RabbitMQ.URL)
	assert.Equal(t, "avatars.exchange", cfg.RabbitMQ.Exchange)
	assert.Equal(t, 30*time.Second, cfg.RabbitMQ.RetryTTL)
}

func TestLoad_RabbitMQRetryTTL(t *testing.T) {
	clearEnv(t)
	t.Setenv("RABBITMQ_RETRY_TTL", "45s")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, cfg.RabbitMQ.RetryTTL)
}

func TestLoad_RabbitMQRetryTTL_Invalid(t *testing.T) {
	clearEnv(t)
	t.Setenv("RABBITMQ_RETRY_TTL", "не-число")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RABBITMQ_RETRY_TTL")
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	clearEnv(t)
	t.Setenv("LOG_LEVEL", "verbose")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LOG_LEVEL")
	assert.Contains(t, err.Error(), "verbose")
}

func TestLoad_InvalidPort(t *testing.T) {
	clearEnv(t)
	t.Setenv("HTTP_PORT", "abc")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP_PORT")
	assert.Contains(t, err.Error(), "abc")
}

func TestLoad_Prod_MissingRequired(t *testing.T) {
	clearEnv(t)
	t.Setenv("APP_ENV", "prod")

	_, err := Load()
	require.Error(t, err)

	// Ошибка должна содержать ВСЕ отсутствующие обязательные переменные,
	// а не только первую попавшуюся.
	for _, key := range requiredProdKeys {
		assert.Contains(t, err.Error(), key, "в ошибке нет ключа %s", key)
	}
}

func TestLoad_Prod_OK(t *testing.T) {
	clearEnv(t)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("POSTGRES_HOST", "db")
	t.Setenv("POSTGRES_PORT", "5432")
	t.Setenv("POSTGRES_USER", "avatar")
	t.Setenv("POSTGRES_PASSWORD", "secret")
	t.Setenv("POSTGRES_DB", "avatars")
	t.Setenv("S3_ENDPOINT", "minio:9000")
	t.Setenv("S3_ACCESS_KEY", "key")
	t.Setenv("S3_SECRET_KEY", "secret")
	t.Setenv("S3_BUCKET", "avatars")
	t.Setenv("RABBITMQ_URL", "amqp://guest:guest@rabbit:5672/")
	t.Setenv("RABBITMQ_EXCHANGE", "avatars.exchange")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "prod", cfg.App.Env)
	require.NoError(t, cfg.Postgres.ValidateDSN())
}

func TestPostgres_DSN_ComplexPassword(t *testing.T) {
	password := "p@ss:w0rd=!;$#"

	p := PostgresConfig{
		Host:     "localhost",
		Port:     "5432",
		User:     "avatar",
		Password: password,
		DB:       "avatars",
		SSLMode:  "disable",
	}

	dsn := p.DSN()

	// net/url экранирует спецсимволы в userinfo.
	// ВАЖНО: '=' в userinfo НЕ экранируется (RFC разрешает его там), поэтому
	// %3D в DSN отсутствует — это ожидаемое поведение stdlib.
	assert.Contains(t, dsn, "%40")       // @
	assert.Contains(t, dsn, "%3A")       // :
	assert.NotContains(t, dsn, password) // сырой пароль не должен попадать в DSN

	parsed, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)

	// pgx должен раскодировать пароль обратно в исходный.
	assert.Equal(t, password, parsed.Password)
	assert.Equal(t, "avatar", parsed.User)
	assert.Equal(t, "avatars", parsed.Database)
	assert.Equal(t, "localhost", parsed.Host)
}

func TestConfig_String_MasksSecrets(t *testing.T) {
	clearEnv(t)
	t.Setenv("POSTGRES_PASSWORD", "pg-SuperSecret-123")
	t.Setenv("S3_ACCESS_KEY", "AKIA-SECRET-ACCESS")
	t.Setenv("S3_SECRET_KEY", "s3-SuperSecret-Key")
	t.Setenv("RABBITMQ_URL", "amqp://admin:RabbitSuperSecret@localhost:5672/")

	cfg, err := Load()
	require.NoError(t, err)

	s := cfg.String()

	assert.NotContains(t, s, "pg-SuperSecret-123")
	assert.NotContains(t, s, "AKIA-SECRET-ACCESS")
	assert.NotContains(t, s, "s3-SuperSecret-Key")
	assert.NotContains(t, s, "RabbitSuperSecret")
	// Пароль в userinfo RabbitMQ маскируется целиком.
	assert.NotContains(t, s, "admin:RabbitSuperSecret")
}
