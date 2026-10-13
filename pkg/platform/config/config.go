// Package config loads the process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/caarlos0/env/v11"
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
	AppEnv   string `env:"APP_ENV" envDefault:"development"`
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
	// DatabaseURL connects as siapp_app, which is not the table owner and has NOBYPASSRLS,
	// so row-level security always applies to the API and the worker.
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
	// MigrateDSN connects as the owner. Only cmd/migrate and the tests read it: the API
	// itself never needs owner rights.
	MigrateDSN string `env:"MIGRATE_DSN"`
	// MigrateOnStart makes the API apply pending migrations at startup. Development only:
	// production runs cmd/migrate as a separate step, so that a slow or failing migration
	// cannot take the API down with it.
	MigrateOnStart bool       `env:"MIGRATE_ON_START"`
	RedisURL       string     `env:"REDIS_URL,required,notEmpty"`
	CORSOrigins    []string   `env:"CORS_ORIGINS" envSeparator:","`
	LogLevel       slog.Level `env:"LOG_LEVEL" envDefault:"info"`

	// PersonelBaseURL is the integration root of kpi-personalia-api, e.g.
	// http://localhost:8081/api/v3/integrations/siapp. Optional until the first module that
	// calls Personalia (Phase 01.3) needs it; when one of the two Personel settings is
	// set, the other must be too.
	PersonelBaseURL string `env:"PERSONEL_BASE_URL"`
	// PersonelIntegrationKey is the shared secret sent as X-Integration-Key. Never logged.
	PersonelIntegrationKey string `env:"PERSONEL_INTEGRATION_KEY"`
}

var (
	postgresSchemes = []string{"postgres", "postgresql"}
	redisSchemes    = []string{"redis", "rediss"}
	httpSchemes     = []string{"http", "https"}
)

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
	// A missing value is already reported above; only judge the shape of a present one.
	problems = append(problems, checkURL("DATABASE_URL", cfg.DatabaseURL, postgresSchemes)...)
	problems = append(problems, checkURL("MIGRATE_DSN", cfg.MigrateDSN, postgresSchemes)...)
	problems = append(problems, checkURL("REDIS_URL", cfg.RedisURL, redisSchemes)...)
	problems = append(problems, checkURL("PERSONEL_BASE_URL", cfg.PersonelBaseURL, httpSchemes)...)
	if (cfg.PersonelBaseURL == "") != (cfg.PersonelIntegrationKey == "") {
		problems = append(problems, "PERSONEL_BASE_URL and PERSONEL_INTEGRATION_KEY must be set together")
	}

	if cfg.MigrateOnStart {
		if cfg.AppEnv == EnvProduction {
			problems = append(problems, "MIGRATE_ON_START is development only and is refused when APP_ENV=production")
		}
		if cfg.MigrateDSN == "" {
			problems = append(problems, "MIGRATE_ON_START=true needs MIGRATE_DSN")
		}
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// checkURL catches the most common mistake early (a MySQL DSN or a bare host:port left
// over in .env), so it fails at startup with the variable's name instead of at the first
// connection with a driver error. The value itself is never echoed: it holds a password.
func checkURL(name, value string, schemes []string) []string {
	if value == "" {
		return nil
	}
	u, err := url.Parse(value)
	if err != nil || !slices.Contains(schemes, u.Scheme) || u.Host == "" {
		return []string{fmt.Sprintf("%s must be a URL like %s://user:password@host:port/..., check its format", name, schemes[0])}
	}
	return nil
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
