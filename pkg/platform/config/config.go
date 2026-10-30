// Package config loads the process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
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

// RedisKeyPrefix goes in front of every Redis key SiAPP writes (revocation list, failure
// counters). ⚠ The API and the worker must agree on it: the worker's people resync writes
// revocations that the API's authentication reads, under the same keys.
const RedisKeyPrefix = "siapp:"

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

	// JWTSecret signs SiAPP's own access tokens (HS256). Never logged. At least
	// auth.MinSecretBytes bytes: generate one with `openssl rand -base64 48`.
	JWTSecret string `env:"JWT_SECRET,required,notEmpty"`
	// ClientIPHeader names the header a reverse proxy sets with the real client address
	// (for example X-Forwarded-For). Empty uses the connection's remote address, which is
	// right when nothing sits in front of the API.
	ClientIPHeader string `env:"CLIENT_IP_HEADER"`

	// PersonelBaseURL is the integration root of kpi-personalia-api, e.g.
	// http://localhost:8081/api/v3/integrations/siapp. Login goes through it (Phase 01.1),
	// so the API cannot start without it.
	PersonelBaseURL string `env:"PERSONEL_BASE_URL,required,notEmpty"`
	// PersonelIntegrationKey is the shared secret sent as X-Integration-Key. Never logged.
	PersonelIntegrationKey string `env:"PERSONEL_INTEGRATION_KEY,required,notEmpty"`

	// GroupCompanyIDs are the Personalia company ids (m_companies.id) of the PTs that make up
	// the group: KPI, IDS and PCD (D17). Their people all belong to ONE organization; anyone
	// employed by another company cannot sign in.
	GroupCompanyIDs []int64 `env:"GROUP_COMPANY_IDS,required,notEmpty" envSeparator:","`
	// GroupOrganizationID is the id of that one organization (organizations.id).
	GroupOrganizationID int64 `env:"GROUP_ORGANIZATION_ID" envDefault:"1"`
	// GroupOrganizationName is its display name.
	GroupOrganizationName string `env:"GROUP_ORGANIZATION_NAME" envDefault:"KPI Group"`

	// Storage is where uploaded photos live (Phase 04.1).
	Storage Storage
}

// Storage drivers accepted in STORAGE_DRIVER.
const (
	// StorageLocal keeps files on the disk of the machine the API runs on.
	StorageLocal = "local"
	// StorageMinIO and StorageS3 are the same S3 protocol with different defaults.
	StorageMinIO = "minio"
	StorageS3    = "s3"
)

var storageDrivers = []string{StorageLocal, StorageMinIO, StorageS3}

// Storage is the object storage configuration. Only the variables of the chosen driver are
// required: a laptop running `local` needs no S3 variables, and a deployment on S3 needs no
// signing key.
type Storage struct {
	Driver string `env:"STORAGE_DRIVER" envDefault:"local"`

	// LocalDir is the root of the files with STORAGE_DRIVER=local. Parse resolves it to an
	// absolute path, so a relative value means "relative to the working directory the
	// process started in", once, and the log line at boot shows where that was.
	LocalDir string `env:"STORAGE_LOCAL_DIR" envDefault:"./data/media"`
	// LocalSigningKey keys the HMAC of the blob URLs. Never logged. At least minSigningKeyBytes.
	LocalSigningKey string `env:"STORAGE_LOCAL_SIGNING_KEY"`
	// PublicBaseURL is the API's origin AS THE DEVICE SEES IT: blob URLs are built from it.
	//
	// ⚠ The local-dev catch: "localhost" on a phone is the phone itself. An Android emulator
	// reaches the laptop at http://10.0.2.2:8080, and a real phone on the same Wi-Fi needs the
	// laptop's LAN address (for example http://192.168.1.10:8080). On a server it is the
	// public HTTPS origin behind the proxy (for example https://siapp.example.com).
	PublicBaseURL string `env:"MEDIA_PUBLIC_BASE_URL"`

	S3Endpoint string `env:"S3_ENDPOINT"`
	// S3PublicEndpoint is the host presigned URLs are signed for, when it differs from the
	// host the API itself talks to (S3Endpoint). Optional.
	S3PublicEndpoint string `env:"S3_PUBLIC_ENDPOINT"`
	S3Region         string `env:"S3_REGION"`
	S3Bucket         string `env:"S3_BUCKET" envDefault:"persist-media"`
	S3AccessKey      string `env:"S3_ACCESS_KEY"`
	S3SecretKey      string `env:"S3_SECRET_KEY"`
	// S3ForcePathStyle overrides the driver's default (true for minio, false for s3).
	S3ForcePathStyle *bool `env:"S3_FORCE_PATH_STYLE"`
}

// defaultPublicBaseURL is where a laptop's API listens, used when nothing else is configured
// outside production.
const defaultPublicBaseURL = "http://localhost:8080"

// defaultMinIORegion is what MinIO signs for when no region is configured.
const defaultMinIORegion = "us-east-1"

// minSigningKeyBytes is the shortest STORAGE_LOCAL_SIGNING_KEY: the size of an HMAC-SHA256 key.
const minSigningKeyBytes = 32

// minJWTSecretBytes mirrors auth.MinSecretBytes. It is repeated here, not imported, so that
// the config package depends on nothing: auth depends on httpx, which must not pull config in.
const minJWTSecretBytes = 32

// placeholderMarkers are fragments of the example values shipped in the repository
// (.env.example) and of the usual ones. A secret containing one was copied, not generated.
var placeholderMarkers = []string{"change-me", "changeme", "change_me", "replace-with", "example", "placeholder"}

// minDistinctSecretBytes is the fewest different byte values a generated secret has. A
// random 32-byte secret has far more, even hex-encoded (16 symbols): this only catches values
// like "aaaa..." or "1234123412341234...".
const minDistinctSecretBytes = 10

// weakSecret reports whether secret is a copied placeholder or has too little variety to be
// random.
//
// ⚠ HS256 tokens are only as safe as this key. The example value in .env.example is public,
// so a production deployment that kept it would let anyone who read the repository sign
// access tokens for any person of any organization. Development keeps working with the
// example value; production refuses to start.
func weakSecret(secret string) bool {
	lower := strings.ToLower(secret)
	for _, marker := range placeholderMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	distinct := map[byte]struct{}{}
	for i := range len(secret) {
		distinct[secret[i]] = struct{}{}
	}
	return len(distinct) < minDistinctSecretBytes
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
	problems = append(problems, checkGroup(cfg)...)
	// The value is never echoed in a problem: it is a secret.
	switch {
	case cfg.JWTSecret == "":
		// Already reported above as missing; judging an absent value would only repeat it.
	case len(cfg.JWTSecret) < minJWTSecretBytes:
		problems = append(problems, fmt.Sprintf("JWT_SECRET must be at least %d bytes (generate one with: openssl rand -base64 48)", minJWTSecretBytes))
	case cfg.AppEnv == EnvProduction && weakSecret(cfg.JWTSecret):
		problems = append(problems, "JWT_SECRET looks like a placeholder or a low-entropy value, which is refused in production (generate one with: openssl rand -base64 48)")
	}

	problems = append(problems, checkStorage(&cfg)...)

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

// checkStorage judges the storage settings of the chosen driver and fills in what they
// default to: the public base URL outside production, the absolute directory, and MinIO's
// region. Variables of the other drivers are ignored, even when they are set (the example
// .env carries the MinIO block commented, and compose sets S3_ENDPOINT for every service).
func checkStorage(cfg *Config) []string {
	s := &cfg.Storage
	switch s.Driver {
	case StorageLocal:
		return checkLocalStorage(cfg)
	case StorageMinIO, StorageS3:
		return checkS3Storage(s)
	default:
		return []string{fmt.Sprintf("STORAGE_DRIVER must be one of %s, got %q", strings.Join(storageDrivers, ", "), s.Driver)}
	}
}

func checkLocalStorage(cfg *Config) []string {
	s := &cfg.Storage
	var problems []string

	// The key is never echoed in a problem: it signs every blob URL, so whoever has it can
	// mint a URL that reads or writes any file.
	switch {
	case s.LocalSigningKey == "":
		problems = append(problems, "STORAGE_LOCAL_SIGNING_KEY is required when STORAGE_DRIVER=local (generate one with: openssl rand -base64 48)")
	case len(s.LocalSigningKey) < minSigningKeyBytes:
		problems = append(problems, fmt.Sprintf("STORAGE_LOCAL_SIGNING_KEY must be at least %d bytes (generate one with: openssl rand -base64 48)", minSigningKeyBytes))
	case cfg.AppEnv == EnvProduction && weakSecret(s.LocalSigningKey):
		problems = append(problems, "STORAGE_LOCAL_SIGNING_KEY looks like a placeholder or a low-entropy value, which is refused in production (generate one with: openssl rand -base64 48)")
	}

	if strings.TrimSpace(s.LocalDir) == "" {
		problems = append(problems, "STORAGE_LOCAL_DIR must not be empty")
	} else if abs, err := filepath.Abs(s.LocalDir); err != nil {
		problems = append(problems, "STORAGE_LOCAL_DIR cannot be resolved to an absolute path")
	} else {
		s.LocalDir = abs
	}

	if s.PublicBaseURL == "" {
		if cfg.AppEnv == EnvProduction {
			problems = append(problems, "MEDIA_PUBLIC_BASE_URL is required in production with STORAGE_DRIVER=local: the public https origin of the API, as devices see it")
		} else {
			s.PublicBaseURL = defaultPublicBaseURL
		}
	}
	if s.PublicBaseURL != "" {
		if origin, ok := originOf(s.PublicBaseURL); ok {
			s.PublicBaseURL = origin
		} else {
			problems = append(problems, "MEDIA_PUBLIC_BASE_URL must be an origin like https://siapp.example.com, with no path, query or credentials")
		}
	}
	return problems
}

func checkS3Storage(s *Storage) []string {
	var problems []string
	if s.Driver == StorageMinIO && s.S3Endpoint == "" {
		problems = append(problems, "S3_ENDPOINT is required when STORAGE_DRIVER=minio")
	}
	if s.S3Region == "" {
		if s.Driver == StorageS3 {
			problems = append(problems, "S3_REGION is required when STORAGE_DRIVER=s3")
		} else {
			s.S3Region = defaultMinIORegion
		}
	}
	if strings.TrimSpace(s.S3Bucket) == "" {
		problems = append(problems, "S3_BUCKET must not be empty")
	}
	if s.S3AccessKey == "" {
		problems = append(problems, "S3_ACCESS_KEY is required when STORAGE_DRIVER is minio or s3")
	}
	if s.S3SecretKey == "" {
		problems = append(problems, "S3_SECRET_KEY is required when STORAGE_DRIVER is minio or s3")
	}
	problems = append(problems, checkURL("S3_ENDPOINT", s.S3Endpoint, httpSchemes)...)
	problems = append(problems, checkURL("S3_PUBLIC_ENDPOINT", s.S3PublicEndpoint, httpSchemes)...)
	return problems
}

// originOf returns raw as scheme://host[:port], and false when it is anything more than that
// (a path, a query, credentials) or not an http(s) URL. One trailing slash is tolerated.
func originOf(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || !slices.Contains(httpSchemes, u.Scheme) || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// checkGroup judges the group settings. A missing GROUP_COMPANY_IDS is already reported by
// the parser, so an empty list says nothing more here.
func checkGroup(cfg Config) []string {
	var problems []string
	for i, id := range cfg.GroupCompanyIDs {
		switch {
		case id <= 0:
			problems = append(problems, fmt.Sprintf("GROUP_COMPANY_IDS must list positive company ids, got %d", id))
		case slices.Contains(cfg.GroupCompanyIDs[:i], id):
			problems = append(problems, fmt.Sprintf("GROUP_COMPANY_IDS lists company %d twice", id))
		}
	}
	if cfg.GroupOrganizationID <= 0 {
		problems = append(problems, "GROUP_ORGANIZATION_ID must be a positive number")
	}
	if strings.TrimSpace(cfg.GroupOrganizationName) == "" {
		problems = append(problems, "GROUP_ORGANIZATION_NAME must not be empty")
	}
	return problems
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
		if key, ok := envKey(reflect.TypeFor[Config](), pe.Name); ok {
			return fmt.Sprintf("%s is invalid: %v", key, pe.Err)
		}
	}
	return strings.TrimPrefix(err.Error(), "env: ")
}

// envKey finds the environment variable of the field called name, looking inside nested
// structs such as Storage. Field names are unique across Config for this reason.
func envKey(t reflect.Type, name string) (string, bool) {
	for f := range t.Fields() {
		if f.Name == name {
			if tag, has := f.Tag.Lookup("env"); has {
				key, _, _ := strings.Cut(tag, ",")
				return key, true
			}
		}
		if f.Type.Kind() == reflect.Struct {
			if key, ok := envKey(f.Type, name); ok {
				return key, true
			}
		}
	}
	return "", false
}
