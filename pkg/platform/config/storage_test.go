package config

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// s3Env is the complete configuration of a driver that talks S3, on top of the platform
// variables. The signing key of the local driver is dropped on purpose: only the chosen
// driver's variables are required.
func s3Env(driver string) map[string]string {
	environ := validEnv()
	delete(environ, "STORAGE_LOCAL_SIGNING_KEY")
	delete(environ, "MEDIA_PUBLIC_BASE_URL")
	maps.Copy(environ, map[string]string{
		"STORAGE_DRIVER": driver,
		"S3_ENDPOINT":    "http://localhost:9000",
		"S3_REGION":      "ap-southeast-3",
		"S3_ACCESS_KEY":  "minioadmin",
		"S3_SECRET_KEY":  "minioadmin",
	})
	return environ
}

func TestStorageDefaultsToLocal(t *testing.T) {
	cfg, err := Parse(validEnv())
	require.NoError(t, err)
	require.Equal(t, StorageLocal, cfg.Storage.Driver)
	require.True(t, filepath.IsAbs(cfg.Storage.LocalDir))
	require.Equal(t, "persist-media", cfg.Storage.S3Bucket)
}

func TestStorageRejectsAnUnknownDriver(t *testing.T) {
	environ := validEnv()
	environ["STORAGE_DRIVER"] = "gcs"

	_, err := Parse(environ)
	require.Error(t, err)
	require.Contains(t, err.Error(), "STORAGE_DRIVER must be one of local, minio, s3")
}

func TestStorageLocalSigningKey(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(map[string]string)
		wantMsg string
	}{
		{"missing", func(e map[string]string) { delete(e, "STORAGE_LOCAL_SIGNING_KEY") }, "STORAGE_LOCAL_SIGNING_KEY is required"},
		{"empty", func(e map[string]string) { e["STORAGE_LOCAL_SIGNING_KEY"] = "" }, "STORAGE_LOCAL_SIGNING_KEY is required"},
		{"too short", func(e map[string]string) { e["STORAGE_LOCAL_SIGNING_KEY"] = "short-short-short" }, "STORAGE_LOCAL_SIGNING_KEY must be at least 32 bytes"},
		{
			"a placeholder in production",
			func(e map[string]string) {
				e["APP_ENV"] = EnvProduction
				e["STORAGE_LOCAL_SIGNING_KEY"] = "change-me-change-me-change-me-change-me-sign"
			},
			"STORAGE_LOCAL_SIGNING_KEY looks like a placeholder",
		},
		{
			"low entropy in production",
			func(e map[string]string) {
				e["APP_ENV"] = EnvProduction
				e["STORAGE_LOCAL_SIGNING_KEY"] = "abababababababababababababababababab"
			},
			"STORAGE_LOCAL_SIGNING_KEY looks like a placeholder",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			environ := validEnv()
			tc.mutate(environ)

			_, err := Parse(environ)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantMsg)
			// Whatever the key was, it is never echoed.
			if key := environ["STORAGE_LOCAL_SIGNING_KEY"]; len(key) > 8 {
				require.NotContains(t, err.Error(), key)
			}
		})
	}
}

// The example file ships a placeholder key so a fresh clone runs. Development accepts it;
// production refuses it, like the JWT secret.
func TestTheExampleSigningKeyWorksInDevelopmentButNotInProduction(t *testing.T) {
	example := readExampleValue(t, "STORAGE_LOCAL_SIGNING_KEY")

	environ := validEnv()
	environ["STORAGE_LOCAL_SIGNING_KEY"] = example
	_, err := Parse(environ)
	require.NoError(t, err)

	environ["APP_ENV"] = EnvProduction
	_, err = Parse(environ)
	require.Error(t, err)
	require.Contains(t, err.Error(), "STORAGE_LOCAL_SIGNING_KEY looks like a placeholder")
	require.NotContains(t, err.Error(), example)
}

func readExampleValue(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../../.env.example")
	require.NoError(t, err)
	for _, line := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), name+"="); ok {
			require.NotEmpty(t, value)
			return value
		}
	}
	t.Fatalf("no active %s in .env.example", name)
	return ""
}

// strongJWTSecret is a generated-looking JWT secret, which production accepts. The shared
// validEnv has a plain one, fine for development and refused in production.
const strongJWTSecret = "q8Vz3mN0rT1yLw7KfX2pH9cJ4dS6bG5uE0aZ8iO1nM3vR7tY"

func TestStorageProductionAcceptsAGeneratedSigningKey(t *testing.T) {
	environ := validEnv()
	environ["APP_ENV"] = EnvProduction
	environ["JWT_SECRET"] = strongJWTSecret
	environ["STORAGE_LOCAL_SIGNING_KEY"] = "9f2c4e7a1b0d8e6f3a5c7b9d1e2f4a6c8b0d2e4f6a8c0e2a4b6d8f0a2c4e6b8d"
	environ["MEDIA_PUBLIC_BASE_URL"] = "https://siapp.example.com"

	_, err := Parse(environ)
	require.NoError(t, err)
}

func TestStoragePublicBaseURL(t *testing.T) {
	t.Run("defaults to localhost outside production", func(t *testing.T) {
		for _, appEnv := range []string{EnvDevelopment, EnvTest} {
			environ := validEnv()
			environ["APP_ENV"] = appEnv
			delete(environ, "MEDIA_PUBLIC_BASE_URL")

			cfg, err := Parse(environ)
			require.NoError(t, err)
			require.Equal(t, "http://localhost:8080", cfg.Storage.PublicBaseURL)
		}
	})
	t.Run("is required in production", func(t *testing.T) {
		environ := validEnv()
		environ["APP_ENV"] = EnvProduction
		delete(environ, "MEDIA_PUBLIC_BASE_URL")

		_, err := Parse(environ)
		require.Error(t, err)
		require.Contains(t, err.Error(), "MEDIA_PUBLIC_BASE_URL is required in production")
	})
	t.Run("is not needed by the other drivers", func(t *testing.T) {
		environ := s3Env(StorageMinIO)
		environ["APP_ENV"] = EnvProduction
		environ["JWT_SECRET"] = strongJWTSecret

		_, err := Parse(environ)
		require.NoError(t, err)
	})
	t.Run("is an origin, with one trailing slash tolerated", func(t *testing.T) {
		environ := validEnv()
		environ["MEDIA_PUBLIC_BASE_URL"] = "http://10.0.2.2:8080/"

		cfg, err := Parse(environ)
		require.NoError(t, err)
		require.Equal(t, "http://10.0.2.2:8080", cfg.Storage.PublicBaseURL)
	})
	for name, bad := range map[string]string{
		"a path":         "https://siapp.example.com/api",
		"a query":        "https://siapp.example.com?x=1",
		"credentials":    "https://user:pw@siapp.example.com",
		"no scheme":      "siapp.example.com",
		"another scheme": "ftp://siapp.example.com",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			environ := validEnv()
			environ["MEDIA_PUBLIC_BASE_URL"] = bad

			_, err := Parse(environ)
			require.Error(t, err)
			require.Contains(t, err.Error(), "MEDIA_PUBLIC_BASE_URL must be an origin")
			require.NotContains(t, err.Error(), "pw@", "credentials are never echoed")
		})
	}
}

func TestStorageLocalDirIsResolvedToAnAbsolutePath(t *testing.T) {
	t.Run("a relative path is resolved against the working directory", func(t *testing.T) {
		environ := validEnv()
		environ["STORAGE_LOCAL_DIR"] = "./data/media"

		cfg, err := Parse(environ)
		require.NoError(t, err)

		wd, err := os.Getwd()
		require.NoError(t, err)
		require.Equal(t, filepath.Join(wd, "data", "media"), cfg.Storage.LocalDir)
	})
	t.Run("an absolute path is kept", func(t *testing.T) {
		dir := t.TempDir()
		environ := validEnv()
		environ["STORAGE_LOCAL_DIR"] = dir

		cfg, err := Parse(environ)
		require.NoError(t, err)
		require.Equal(t, dir, cfg.Storage.LocalDir)
	})
	t.Run("Parse does not create it: the storage does, at start-up", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "not", "yet")
		environ := validEnv()
		environ["STORAGE_LOCAL_DIR"] = dir

		_, err := Parse(environ)
		require.NoError(t, err)
		require.NoDirExists(t, dir)
	})
	t.Run("an empty value is refused", func(t *testing.T) {
		environ := validEnv()
		environ["STORAGE_LOCAL_DIR"] = " "

		_, err := Parse(environ)
		require.Error(t, err)
		require.Contains(t, err.Error(), "STORAGE_LOCAL_DIR must not be empty")
	})
}

func TestStorageS3Drivers(t *testing.T) {
	t.Run("minio needs no local variables and defaults its region", func(t *testing.T) {
		environ := s3Env(StorageMinIO)
		delete(environ, "S3_REGION")

		cfg, err := Parse(environ)
		require.NoError(t, err)
		require.Equal(t, "us-east-1", cfg.Storage.S3Region)
		require.Equal(t, "persist-media", cfg.Storage.S3Bucket)
		require.Nil(t, cfg.Storage.S3ForcePathStyle, "the driver picks its own default")
	})
	t.Run("s3 works with the AWS endpoint of its region", func(t *testing.T) {
		environ := s3Env(StorageS3)
		delete(environ, "S3_ENDPOINT")

		cfg, err := Parse(environ)
		require.NoError(t, err)
		require.Empty(t, cfg.Storage.S3Endpoint)
		require.Equal(t, "ap-southeast-3", cfg.Storage.S3Region)
	})
	t.Run("the path style can be overridden", func(t *testing.T) {
		environ := s3Env(StorageS3)
		environ["S3_FORCE_PATH_STYLE"] = "true"

		cfg, err := Parse(environ)
		require.NoError(t, err)
		require.NotNil(t, cfg.Storage.S3ForcePathStyle)
		require.True(t, *cfg.Storage.S3ForcePathStyle)
	})
	t.Run("an invalid path style names the variable", func(t *testing.T) {
		environ := s3Env(StorageS3)
		environ["S3_FORCE_PATH_STYLE"] = "sometimes"

		_, err := Parse(environ)
		require.Error(t, err)
		require.Contains(t, err.Error(), "S3_FORCE_PATH_STYLE is invalid")
	})

	for name, tc := range map[string]struct {
		driver  string
		drop    []string
		wantMsg string
	}{
		"minio without an endpoint":   {StorageMinIO, []string{"S3_ENDPOINT"}, "S3_ENDPOINT is required when STORAGE_DRIVER=minio"},
		"s3 without a region":         {StorageS3, []string{"S3_REGION"}, "S3_REGION is required when STORAGE_DRIVER=s3"},
		"minio without an access key": {StorageMinIO, []string{"S3_ACCESS_KEY"}, "S3_ACCESS_KEY is required"},
		"s3 without a secret key":     {StorageS3, []string{"S3_SECRET_KEY"}, "S3_SECRET_KEY is required"},
	} {
		t.Run(name, func(t *testing.T) {
			environ := s3Env(tc.driver)
			for _, k := range tc.drop {
				delete(environ, k)
			}
			_, err := Parse(environ)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantMsg)
		})
	}

	t.Run("an endpoint that is not a URL", func(t *testing.T) {
		environ := s3Env(StorageMinIO)
		environ["S3_ENDPOINT"] = "minio:9000"
		environ["S3_PUBLIC_ENDPOINT"] = "localhost:9000"

		_, err := Parse(environ)
		require.Error(t, err)
		require.Contains(t, err.Error(), "S3_ENDPOINT must be a URL")
		require.Contains(t, err.Error(), "S3_PUBLIC_ENDPOINT must be a URL")
	})
	t.Run("the keys are never echoed", func(t *testing.T) {
		environ := s3Env(StorageMinIO)
		environ["S3_ACCESS_KEY"] = "AKIAEXAMPLEKEYVALUE"
		environ["S3_SECRET_KEY"] = "super-secret-s3-value"
		environ["S3_ENDPOINT"] = "not-a-url"

		_, err := Parse(environ)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "AKIAEXAMPLEKEYVALUE")
		require.NotContains(t, err.Error(), "super-secret-s3-value")
	})
}

// Only the chosen driver's variables are required. A laptop on `local` that still has the
// MinIO block of the example file uncommented must start, and so must an S3 deployment
// without a signing key.
func TestStorageIgnoresTheVariablesOfOtherDrivers(t *testing.T) {
	t.Run("local with S3 variables present and even wrong", func(t *testing.T) {
		environ := validEnv()
		environ["S3_ENDPOINT"] = "not-a-url"
		environ["S3_FORCE_PATH_STYLE"] = "true"

		_, err := Parse(environ)
		require.NoError(t, err)
	})
	t.Run("s3 without any local variable", func(t *testing.T) {
		environ := s3Env(StorageS3)
		environ["STORAGE_LOCAL_DIR"] = ""

		_, err := Parse(environ)
		require.NoError(t, err)
	})
}
