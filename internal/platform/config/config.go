// Package config loads the process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	netmail "net/mail"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/caarlos0/env/v11"

	"siapp/internal/mail"
)

// App environments accepted in APP_ENV.
const (
	EnvDevelopment = "development"
	EnvTest        = "test"
	EnvProduction  = "production"
)

var appEnvs = []string{EnvDevelopment, EnvTest, EnvProduction}

// Config is the full process configuration. Every field maps to one environment variable.
type Config struct {
	AppEnv      string     `env:"APP_ENV" envDefault:"development"`
	HTTPAddr    string     `env:"HTTP_ADDR" envDefault:":8080"`
	MySQLDSN    string     `env:"MYSQL_DSN,required,notEmpty"`
	RedisURL    string     `env:"REDIS_URL,required,notEmpty"`
	CORSOrigins []string   `env:"CORS_ORIGINS" envSeparator:","`
	LogLevel    slog.Level `env:"LOG_LEVEL" envDefault:"info"`
	// MigrateOnStart runs pending migrations when the API starts. Development only:
	// production migrates as a separate deploy step (make migrate).
	MigrateOnStart bool `env:"MIGRATE_ON_START" envDefault:"false"`
	// JWTSecret signs access tokens (HS256). At least 32 bytes of randomness, e.g.
	// `openssl rand -base64 48`. Changing it logs everyone out.
	JWTSecret string `env:"JWT_SECRET,required,notEmpty"`
	// ClientIPHeader names the request header in which a trusted reverse proxy puts the
	// real client address (e.g. X-Forwarded-For). Empty means use the connection address.
	// Only set it when a proxy you control overwrites that header on every request,
	// otherwise clients can spoof their IP and dodge the login rate limit.
	ClientIPHeader string `env:"CLIENT_IP_HEADER"`

	// AppResetURL is the page of the client app that takes a password reset token. Reset
	// emails link to it as ?token=...
	AppResetURL string `env:"APP_RESET_URL" envDefault:"http://localhost:3000/reset-password"`

	// Outgoing mail. The defaults point at Mailpit from docker-compose.
	SMTPHost     string `env:"SMTP_HOST" envDefault:"localhost"`
	SMTPPort     int    `env:"SMTP_PORT" envDefault:"1025"`
	SMTPUsername string `env:"SMTP_USERNAME"` // empty: no authentication
	SMTPPassword string `env:"SMTP_PASSWORD"`
	SMTPFrom     string `env:"SMTP_FROM" envDefault:"SiAPP <noreply@siapp.local>"`
	// SMTPTLS is none, starttls or tls. Production must not use none.
	SMTPTLS string `env:"SMTP_TLS" envDefault:"none"`
}

// minJWTSecretBytes is the shortest accepted HS256 key: the size of the SHA-256 output.
const minJWTSecretBytes = 32

// WorkerConfig is the configuration of cmd/worker, which needs only the database, Redis
// (the job queue) and the log level.
type WorkerConfig struct {
	MySQLDSN string     `env:"MYSQL_DSN,required,notEmpty"`
	RedisURL string     `env:"REDIS_URL,required,notEmpty"`
	LogLevel slog.Level `env:"LOG_LEVEL" envDefault:"info"`
}

// LoadWorker parses the worker configuration from the process environment.
func LoadWorker() (WorkerConfig, error) {
	var cfg WorkerConfig
	err := env.ParseWithOptions(&cfg, env.Options{Environment: env.ToMap(os.Environ())})
	if err != nil {
		return WorkerConfig{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// SeedConfig is the configuration of cmd/seed, which needs far less than the API.
type SeedConfig struct {
	MySQLDSN      string `env:"MYSQL_DSN,required,notEmpty"`
	AdminEmail    string `env:"SEED_ADMIN_EMAIL,required,notEmpty"`
	AdminPassword string `env:"SEED_ADMIN_PASSWORD,required,notEmpty"`
}

// LoadSeed parses the seed configuration from the process environment.
func LoadSeed() (SeedConfig, error) {
	var cfg SeedConfig
	err := env.ParseWithOptions(&cfg, env.Options{Environment: env.ToMap(os.Environ())})
	if err != nil {
		return SeedConfig{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// Load parses the configuration from the process environment.
func Load() (Config, error) {
	return Parse(env.ToMap(os.Environ()))
}

// Parse parses the configuration from the given variables. It reports every missing or
// invalid variable at once, so a misconfigured deployment can be fixed in one pass.
func Parse(environ map[string]string) (Config, error) {
	var cfg Config
	var problems []string

	err := env.ParseWithOptions(&cfg, env.Options{Environment: environ})
	if err != nil {
		var agg env.AggregateError
		if errors.As(err, &agg) {
			for _, e := range agg.Errors {
				problems = append(problems, describe(e))
			}
		} else {
			problems = append(problems, err.Error())
		}
	}

	if !slices.Contains(appEnvs, cfg.AppEnv) {
		problems = append(problems, fmt.Sprintf("APP_ENV must be one of %s, got %q", strings.Join(appEnvs, ", "), cfg.AppEnv))
	}
	if cfg.MigrateOnStart && cfg.AppEnv == EnvProduction {
		problems = append(problems, "MIGRATE_ON_START must not be true when APP_ENV is production")
	}
	problems = append(problems, checkMail(cfg)...)
	// A missing secret is already reported above; only judge the length of a present one.
	if cfg.JWTSecret != "" && len(cfg.JWTSecret) < minJWTSecretBytes {
		problems = append(problems, fmt.Sprintf("JWT_SECRET must be at least %d bytes, got %d", minJWTSecretBytes, len(cfg.JWTSecret)))
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// checkMail validates the mail and reset link settings. Production is held to a higher
// standard because reset links carry a secret: the link must be https, and the mail must not
// cross the network in plain text.
func checkMail(cfg Config) []string {
	var problems []string

	u, err := url.Parse(cfg.AppResetURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		problems = append(problems, fmt.Sprintf("APP_RESET_URL must be an absolute URL, got %q", cfg.AppResetURL))
	} else if cfg.AppEnv == EnvProduction && u.Scheme != "https" {
		problems = append(problems, "APP_RESET_URL must use https in production, because the link carries a secret")
	}

	if cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
		problems = append(problems, fmt.Sprintf("SMTP_PORT must be between 1 and 65535, got %d", cfg.SMTPPort))
	}
	if _, err := mail.ParseTLSMode(cfg.SMTPTLS); err != nil {
		problems = append(problems, err.Error())
	} else if cfg.AppEnv == EnvProduction && cfg.SMTPTLS == string(mail.TLSNone) {
		problems = append(problems, "SMTP_TLS must be starttls or tls in production, not none")
	}
	if _, err := netmail.ParseAddress(cfg.SMTPFrom); err != nil {
		problems = append(problems, fmt.Sprintf("SMTP_FROM is not a valid address: %q", cfg.SMTPFrom))
	}
	return problems
}

// describe rewrites parse errors to name the environment variable instead of the Go field,
// because the variable is what an operator has to fix.
func describe(err error) string {
	var pe env.ParseError
	if errors.As(err, &pe) {
		if f, ok := reflect.TypeFor[Config]().FieldByName(pe.Name); ok {
			key, _, _ := strings.Cut(f.Tag.Get("env"), ",")
			return fmt.Sprintf("%s is invalid: %v", key, pe.Err)
		}
	}
	return strings.TrimPrefix(err.Error(), "env: ")
}
