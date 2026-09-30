package config

import (
	"log/slog"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

func validEnv() map[string]string {
	return map[string]string{
		"MYSQL_DSN":  "siapp:siapp@tcp(localhost:3306)/siapp?parseTime=true&loc=UTC",
		"REDIS_URL":  "redis://localhost:6379/0",
		"JWT_SECRET": "0123456789abcdef0123456789abcdef",
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
		{
			name:     "missing JWT_SECRET",
			drop:     []string{"JWT_SECRET"},
			wantMsgs: []string{`"JWT_SECRET"`},
		},
		{
			name:     "JWT_SECRET too short",
			override: map[string]string{"JWT_SECRET": "short"},
			wantMsgs: []string{"JWT_SECRET must be at least 32 bytes"},
		},
		{
			name:     "migrate on start in production",
			override: map[string]string{"APP_ENV": "production", "MIGRATE_ON_START": "true"},
			wantMsgs: []string{"MIGRATE_ON_START"},
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

func TestParseClientIPHeader(t *testing.T) {
	cfg, err := Parse(validEnv())
	require.NoError(t, err)
	require.Empty(t, cfg.ClientIPHeader)

	environ := validEnv()
	environ["CLIENT_IP_HEADER"] = "X-Forwarded-For"
	cfg, err = Parse(environ)
	require.NoError(t, err)
	require.Equal(t, "X-Forwarded-For", cfg.ClientIPHeader)
}

func TestParseCORSOrigins(t *testing.T) {
	environ := validEnv()
	environ["CORS_ORIGINS"] = "http://localhost:3000,https://admin.example.com"

	cfg, err := Parse(environ)
	require.NoError(t, err)
	require.Equal(t, []string{"http://localhost:3000", "https://admin.example.com"}, cfg.CORSOrigins)
}
