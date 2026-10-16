package config

import (
	"log/slog"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://siapp_app:siapp_app@localhost:5432/siapp?sslmode=disable",
		"REDIS_URL":    "redis://localhost:6379/0",
		// 36 bytes: over the 32-byte minimum.
		"JWT_SECRET":               "test-secret-test-secret-test-secret",
		"PERSONEL_BASE_URL":        "http://localhost:8081/api/v3/integrations/siapp",
		"PERSONEL_INTEGRATION_KEY": "test-integration-key",
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
			name:     "missing PERSONEL_BASE_URL",
			drop:     []string{"PERSONEL_BASE_URL"},
			wantMsgs: []string{`"PERSONEL_BASE_URL"`},
		},
		{
			name:     "missing PERSONEL_INTEGRATION_KEY",
			drop:     []string{"PERSONEL_INTEGRATION_KEY"},
			wantMsgs: []string{`"PERSONEL_INTEGRATION_KEY"`},
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

// ⚠ A secret copied from the repository must not reach production: anyone who read the
// repository could sign access tokens for any person of any organization.
func TestProductionRefusesAPlaceholderOrWeakJWTSecret(t *testing.T) {
	exampleValue := readExampleJWTSecret(t)

	for name, secret := range map[string]string{
		"the value in .env.example":  exampleValue,
		"another change-me value":    "CHANGE-ME-please-to-something-long-enough",
		"a replace-with placeholder": "replace-with-a-long-random-secret-value-ok",
		"one repeated character":     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"a short repeated pattern":   "12341234123412341234123412341234",
	} {
		t.Run(name, func(t *testing.T) {
			environ := validEnv()
			environ["APP_ENV"] = EnvProduction
			environ["JWT_SECRET"] = secret

			_, err := Parse(environ)
			require.Error(t, err)
			require.Contains(t, err.Error(), "JWT_SECRET looks like a placeholder")
			require.NotContains(t, err.Error(), secret, "the secret is never echoed")
		})
	}
}

func TestProductionAcceptsAGeneratedJWTSecret(t *testing.T) {
	for name, secret := range map[string]string{
		"base64, as openssl rand -base64 48": "q8Vz3mN0rT1yLw7KfX2pH9cJ4dS6bG5uE0aZ8iO1nM3vR7tY",
		"hex, as openssl rand -hex 32":       "9f2c4e7a1b0d8e6f3a5c7b9d1e2f4a6c8b0d2e4f6a8c0e2a4b6d8f0a2c4e6b8d",
	} {
		t.Run(name, func(t *testing.T) {
			environ := validEnv()
			environ["APP_ENV"] = EnvProduction
			environ["JWT_SECRET"] = secret
			_, err := Parse(environ)
			require.NoError(t, err)
		})
	}
}

func TestDevelopmentKeepsWorkingWithTheExampleJWTSecret(t *testing.T) {
	environ := validEnv()
	environ["APP_ENV"] = EnvDevelopment
	environ["JWT_SECRET"] = readExampleJWTSecret(t)
	_, err := Parse(environ)
	require.NoError(t, err)
}

// readExampleJWTSecret reads the JWT_SECRET of .env.example, so that the test follows the
// file if its example value ever changes.
func readExampleJWTSecret(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../../.env.example")
	require.NoError(t, err)
	for _, line := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "JWT_SECRET="); ok {
			require.NotEmpty(t, value)
			return value
		}
	}
	t.Fatal("no JWT_SECRET in .env.example")
	return ""
}

func TestParseNeverEchoesTheJWTSecret(t *testing.T) {
	environ := validEnv()
	environ["JWT_SECRET"] = "too-short-secret"

	_, err := Parse(environ)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "too-short-secret")
}

func TestParseClientIPHeader(t *testing.T) {
	environ := validEnv()
	environ["CLIENT_IP_HEADER"] = "X-Forwarded-For"

	cfg, err := Parse(environ)
	require.NoError(t, err)
	require.Equal(t, "X-Forwarded-For", cfg.ClientIPHeader)
}
