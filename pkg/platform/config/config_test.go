package config

import (
	"log/slog"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://siapp_app:siapp_app@localhost:5432/siapp?sslmode=disable",
		"REDIS_URL":    "redis://localhost:6379/0",
	}
}

func TestParse(t *testing.T) {
	cfg, err := Parse(validEnv())
	require.NoError(t, err)
	require.Equal(t, EnvDevelopment, cfg.AppEnv)
	require.Equal(t, ":8080", cfg.HTTPAddr)
	require.Equal(t, slog.LevelInfo, cfg.LogLevel)
	require.Empty(t, cfg.MigrateDSN)
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
			name:     "missing DATABASE_URL",
			drop:     []string{"DATABASE_URL"},
			wantMsgs: []string{"invalid configuration", `"DATABASE_URL"`},
		},
		{
			name:     "empty DATABASE_URL",
			override: map[string]string{"DATABASE_URL": ""},
			wantMsgs: []string{`"DATABASE_URL"`},
		},
		{
			name:     "DATABASE_URL left over from MySQL",
			override: map[string]string{"DATABASE_URL": "siapp:siapp@tcp(localhost:3306)/siapp?parseTime=true"},
			wantMsgs: []string{"DATABASE_URL must be a URL like postgres://"},
		},
		{
			name:     "MIGRATE_DSN with the wrong scheme",
			override: map[string]string{"MIGRATE_DSN": "mysql://root@localhost/siapp"},
			wantMsgs: []string{"MIGRATE_DSN must be a URL like postgres://"},
		},
		{
			name:     "REDIS_URL without a scheme",
			override: map[string]string{"REDIS_URL": "localhost:6379"},
			wantMsgs: []string{"REDIS_URL must be a URL like redis://"},
		},
		{
			name:     "MIGRATE_ON_START in production",
			override: map[string]string{"MIGRATE_ON_START": "true", "APP_ENV": "production", "MIGRATE_DSN": "postgres://o:o@localhost/siapp"},
			wantMsgs: []string{"MIGRATE_ON_START", "production"},
		},
		{
			name:     "MIGRATE_ON_START without MIGRATE_DSN",
			override: map[string]string{"MIGRATE_ON_START": "true"},
			wantMsgs: []string{"MIGRATE_ON_START=true needs MIGRATE_DSN"},
		},
		{
			name:     "PERSONEL_BASE_URL without a scheme",
			override: map[string]string{"PERSONEL_BASE_URL": "localhost:8081", "PERSONEL_INTEGRATION_KEY": "k"},
			wantMsgs: []string{"PERSONEL_BASE_URL must be a URL like http://"},
		},
		{
			name:     "PERSONEL_BASE_URL without a key",
			override: map[string]string{"PERSONEL_BASE_URL": "http://localhost:8081/api/v3/integrations/siapp"},
			wantMsgs: []string{"PERSONEL_BASE_URL and PERSONEL_INTEGRATION_KEY must be set together"},
		},
		{
			name:     "PERSONEL_INTEGRATION_KEY without a URL",
			override: map[string]string{"PERSONEL_INTEGRATION_KEY": "k"},
			wantMsgs: []string{"must be set together"},
		},
		{
			name:     "all problems reported together",
			drop:     []string{"DATABASE_URL", "REDIS_URL"},
			override: map[string]string{"LOG_LEVEL": "loud", "APP_ENV": "staging"},
			wantMsgs: []string{`"DATABASE_URL"`, `"REDIS_URL"`, "LOG_LEVEL", "APP_ENV"},
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

func TestParseNeverEchoesPasswords(t *testing.T) {
	environ := validEnv()
	environ["DATABASE_URL"] = "postgres-typo://siapp_app:s3cret-pw@localhost/siapp"

	_, err := Parse(environ)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cret-pw")
}

func TestParseAcceptsOwnerDSN(t *testing.T) {
	environ := validEnv()
	environ["MIGRATE_DSN"] = "postgresql://siapp_owner:siapp_owner@localhost:5432/siapp?sslmode=disable"

	cfg, err := Parse(environ)
	require.NoError(t, err)
	require.Equal(t, environ["MIGRATE_DSN"], cfg.MigrateDSN)
}

func TestParseCORSOrigins(t *testing.T) {
	environ := validEnv()
	environ["CORS_ORIGINS"] = "http://localhost:3000,https://admin.example.com"

	cfg, err := Parse(environ)
	require.NoError(t, err)
	require.Equal(t, []string{"http://localhost:3000", "https://admin.example.com"}, cfg.CORSOrigins)
}

func TestParsePersonel(t *testing.T) {
	environ := validEnv()
	environ["PERSONEL_BASE_URL"] = "http://localhost:8081/api/v3/integrations/siapp"
	environ["PERSONEL_INTEGRATION_KEY"] = "s3cret-key"

	cfg, err := Parse(environ)
	require.NoError(t, err)
	require.Equal(t, environ["PERSONEL_BASE_URL"], cfg.PersonelBaseURL)
	require.Equal(t, "s3cret-key", cfg.PersonelIntegrationKey)
}

func TestParseNeverEchoesTheIntegrationKey(t *testing.T) {
	environ := validEnv()
	environ["PERSONEL_BASE_URL"] = "not a url"
	environ["PERSONEL_INTEGRATION_KEY"] = "s3cret-key"

	_, err := Parse(environ)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cret-key")
}
