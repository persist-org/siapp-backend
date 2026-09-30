// Package config loads the process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
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
	AppEnv      string     `env:"APP_ENV" envDefault:"development"`
	HTTPAddr    string     `env:"HTTP_ADDR" envDefault:":8080"`
	MySQLDSN    string     `env:"MYSQL_DSN,required,notEmpty"`
	RedisURL    string     `env:"REDIS_URL,required,notEmpty"`
	CORSOrigins []string   `env:"CORS_ORIGINS" envSeparator:","`
	LogLevel    slog.Level `env:"LOG_LEVEL" envDefault:"info"`
	// MigrateOnStart runs pending migrations when the API starts. Development only:
	// production migrates as a separate deploy step (make migrate).
	MigrateOnStart bool `env:"MIGRATE_ON_START" envDefault:"false"`
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

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
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
