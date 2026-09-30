package config

import (
	"log/slog"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

func validEnv() map[string]string {
	return map[string]string{
		"MYSQL_DSN": "siapp:siapp@tcp(localhost:3306)/siapp?parseTime=true&loc=UTC",
		"REDIS_URL": "redis://localhost:6379/0",
	}
}

func TestParse(t *testing.T) {
	cfg, err := Parse(validEnv())
	require.NoError(t, err)
	require.Equal(t, EnvDevelopment, cfg.AppEnv)
	require.Equal(t, ":8080", cfg.HTTPAddr)
	require.Equal(t, slog.LevelInfo, cfg.LogLevel)
	require.Empty(t, cfg.CORSOrigins)
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name     string
		override map[string]string
		drop     []string
		wantMsgs []string
	}{
		{
			name:     "missing MYSQL_DSN",
			drop:     []string{"MYSQL_DSN"},
			wantMsgs: []string{"invalid configuration", `"MYSQL_DSN"`},
		},
		{
			name:     "empty MYSQL_DSN",
			override: map[string]string{"MYSQL_DSN": ""},
			wantMsgs: []string{`"MYSQL_DSN"`},
		},
		{
			name:     "all problems reported together",
			drop:     []string{"MYSQL_DSN", "REDIS_URL"},
			override: map[string]string{"LOG_LEVEL": "loud", "APP_ENV": "staging"},
			wantMsgs: []string{`"MYSQL_DSN"`, `"REDIS_URL"`, "LOG_LEVEL", "APP_ENV"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			environ := validEnv()
			for _, k := range tc.drop {
				delete(environ, k)
			}
			maps.Copy(environ, tc.override)

			_, err := Parse(environ)
			require.Error(t, err)
			for _, msg := range tc.wantMsgs {
				require.Contains(t, err.Error(), msg)
			}
		})
	}
}

func TestParseCORSOrigins(t *testing.T) {
	environ := validEnv()
	environ["CORS_ORIGINS"] = "http://localhost:3000,https://admin.example.com"

	cfg, err := Parse(environ)
	require.NoError(t, err)
	require.Equal(t, []string{"http://localhost:3000", "https://admin.example.com"}, cfg.CORSOrigins)
}
