package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"

	"siapp/pkg/platform/apperr"
)

const maxBodyBytes = 1 << 20 // 1 MB

// FieldError lists the problems found with one request field.
type FieldError struct {
	Field    string   `json:"field"`
	Messages []string `json:"messages"`
}

// validate is safe for concurrent use and caches struct metadata, so it is shared.
var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	// ⚠ Report the JSON name (what the client sent), not the Go field name, so that
	// {"field": "occurredAt"} matches the API contract instead of "OccurredAt".
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			return ""
		}
		if name == "" {
			// Query structs and untagged fields fall back to the Go name.
			return f.Name
		}
		return name
	})
	// The client-generated IDs of offline creates (idem.OfflineCreate) must be UUIDv7.
	// The error can only be a reserved tag name, which is a programming mistake.
	if err := v.RegisterValidation("uuid7", isUUIDv7); err != nil {
		panic("httpx: register uuid7 validator: " + err.Error())
	}
	if err := v.RegisterValidation("username", func(fl validator.FieldLevel) bool { return ValidUsername(fl.Field().String()) }); err != nil {
		panic("httpx: register username validator: " + err.Error())
	}
	if err := v.RegisterValidation("notblank", func(fl validator.FieldLevel) bool { return strings.TrimSpace(fl.Field().String()) != "" }); err != nil {
		panic("httpx: register notblank validator: " + err.Error())
	}
	return v
}

// usernamePattern is 3 to 50 letters, digits, dots, underscores and hyphens.
//
// ⚠ It must never allow '@'. A login identifier is an email or a username, told apart by
// the '@'; a username that contained one could collide with someone else's email.
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{3,50}$`)

// ValidUsername reports whether s is an acceptable username.
func ValidUsername(s string) bool { return usernamePattern.MatchString(s) }

// isUUIDv7 accepts only the canonical lowercase 36-character form of a version 7 UUID.
//
// ⚠ Canonical form matters because IDs are stored in an ascii_bin (case-sensitive)
// column: "0193…-AB" and "0193…-ab" would be two different rows, and a replay that
// changed the case would create a duplicate instead of being recognised. uuid.Parse alone
// also accepts braces, "urn:uuid:" prefixes and uppercase, so compare the round trip.
func isUUIDv7(fl validator.FieldLevel) bool {
	s := fl.Field().String()
	u, err := uuid.Parse(s)
	return err == nil && u.Version() == 7 && u.String() == s
}

// Decode reads the JSON request body into dst and validates it.
//
// The body is limited to 1 MB, unknown fields are rejected and trailing data after the
// JSON value is an error. Every failure is a *apperr.Error with code VALIDATION_FAILED
// (status 413 when the body is too large), ready for Error.
func Decode(r *http.Request, dst any) error {
	// A nil ResponseWriter is allowed: it only means the server is not told to close
	// the connection after an oversized body.
	body := http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second value ({"a":1}{"b":2}) would otherwise be silently ignored.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return apperr.ErrValidationFailed.WithMessage("Body harus berisi satu objek JSON.")
	}
	return Validate(dst)
}

func decodeError(err error) error {
	var (
		maxBytes *http.MaxBytesError
		syntax   *json.SyntaxError
		typeErr  *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &maxBytes):
		e := *apperr.ErrValidationFailed.WithMessage("Body terlalu besar.")
		e.Status = http.StatusRequestEntityTooLarge
		return &e
	case errors.Is(err, io.EOF):
		return apperr.ErrValidationFailed.WithMessage("Body wajib diisi.")
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
		return apperr.ErrValidationFailed.WithMessage("Body bukan JSON yang valid.")
	case errors.As(err, &typeErr):
		return apperr.ErrValidationFailed.WithDetails([]FieldError{
			{Field: typeErr.Field, Messages: []string{"harus bertipe " + jsonKind(typeErr.Type)}},
		})
	}
	// encoding/json has no typed error for unknown fields; it is reported as text.
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return apperr.ErrValidationFailed.WithDetails([]FieldError{
			{Field: strings.Trim(field, `"`), Messages: []string{"field tidak dikenal"}},
		})
	}
	return apperr.ErrValidationFailed.WithMessage("Body tidak dapat dibaca.")
}

func jsonKind(t reflect.Type) string {
	switch t.Kind() { //nolint:exhaustive // every other kind reads as "nilai yang sesuai"
	case reflect.String:
		return "teks"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "angka"
	case reflect.Slice, reflect.Array:
		return "daftar"
	case reflect.Struct, reflect.Map:
		return "objek"
	default:
		return "nilai yang sesuai"
	}
}

// Validate runs the `validate` struct tags on v and converts failures to a
// VALIDATION_FAILED error with details [{field, messages}].
func Validate(v any) error {
	err := validate.Struct(v)
	if err == nil {
		return nil
	}
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return fmt.Errorf("validate request: %w", err)
	}

	var fields []FieldError
	index := make(map[string]int)
	for _, fe := range verrs {
		// Namespace is "Root.items[0].name"; drop the root type name, keep the path.
		_, path, _ := strings.Cut(fe.Namespace(), ".")
		i, seen := index[path]
		if !seen {
			i = len(fields)
			index[path] = i
			fields = append(fields, FieldError{Field: path})
		}
		fields[i].Messages = append(fields[i].Messages, tagMessage(fe))
	}
	return apperr.ErrValidationFailed.WithDetails(fields)
}

func tagMessage(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "wajib diisi"
	case "uuid", "uuid4", "uuid7":
		return "harus berupa UUID yang valid"
	case "email":
		return "harus berupa email yang valid"
	case "username":
		return "harus 3 sampai 50 karakter: huruf, angka, titik, garis bawah atau tanda hubung"
	case "notblank":
		return "tidak boleh kosong"
	case "printascii":
		return "hanya boleh berisi karakter ASCII yang dapat dicetak"
	case "min":
		return "terlalu kecil atau terlalu pendek (minimum " + fe.Param() + ")"
	case "max":
		return "terlalu besar atau terlalu panjang (maksimum " + fe.Param() + ")"
	case "len":
		return "harus tepat " + fe.Param()
	case "oneof":
		return "harus salah satu dari: " + fe.Param()
	case "gte", "gt", "lte", "lt":
		return "di luar batas yang diizinkan"
	default:
		return "tidak valid"
	}
}
