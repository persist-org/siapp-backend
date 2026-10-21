package httpx

import (
	"bytes"
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
	"siapp/pkg/platform/timeutil"
)

const maxBodyBytes = 1 << 20 // 1 MB

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

// Field error codes (FieldError.code). They are stable: clients may map them to messages.
const (
	codeRequired = "REQUIRED"
	codeInvalid  = "INVALID"
	codeTooShort = "TOO_SHORT"
	codeTooLong  = "TOO_LONG"
	codeTooSmall = "TOO_SMALL"
	codeTooLarge = "TOO_LARGE"
)

// Decode reads the JSON request body into dst and validates it.
//
// The body is limited to 1 MB and unknown fields are rejected. Failures are *apperr.Error,
// ready for Error:
//   - not JSON at all, empty, or more than one value: 400 BAD_REQUEST;
//   - over 1 MB: 413 PAYLOAD_TOO_LARGE;
//   - an unknown field, a wrong type or a failed `validate` tag: 422 VALIDATION_ERROR with
//     one errors[] entry per bad field, named by its JSON name.
func Decode(r *http.Request, dst any) error {
	// A nil ResponseWriter is allowed: it only means the server is not told to close
	// the connection after an oversized body.
	data, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		return decodeError(err)
	}
	// ⚠ Postgres cannot store NUL in text: a string with \u0000 would fail deep in a query
	// as a 500. JSON can only carry it escaped, so the escape is what is looked for.
	if bytes.Contains(data, []byte(`\u0000`)) {
		return apperr.ErrBadRequest.WithMessage("Body tidak boleh berisi karakter NUL (\\u0000).")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	// A second value ({"a":1}{"b":2}) would otherwise be silently ignored.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return apperr.ErrBadRequest.WithMessage("Body harus berisi satu objek JSON.")
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
		return apperr.ErrPayloadTooLarge
	case errors.Is(err, io.EOF):
		return apperr.ErrBadRequest.WithMessage("Body wajib diisi.")
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
		return apperr.ErrBadRequest.WithMessage("Body bukan JSON yang valid.")
	case errors.As(err, &typeErr):
		message := "Harus bertipe " + jsonKind(typeErr.Type) + "."
		if typeErr.Type == reflect.TypeFor[timeutil.Date]() {
			message = "Harus berformat YYYY-MM-DD."
		}
		return apperr.ErrValidation.WithFields(apperr.FieldError{Field: typeErr.Field, Code: codeInvalid, Message: message})
	}
	// encoding/json has no typed error for unknown fields; it is reported as text.
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return apperr.ErrValidation.WithFields(apperr.FieldError{
			Field: strings.Trim(field, `"`), Code: codeInvalid, Message: "Field tidak dikenal.",
		})
	}
	return apperr.ErrBadRequest.WithMessage("Body tidak dapat dibaca.")
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
// VALIDATION_ERROR with one errors[] entry per bad field (the first failing tag wins).
func Validate(v any) error {
	err := validate.Struct(v)
	if err == nil {
		return nil
	}
	verrs, ok := errors.AsType[validator.ValidationErrors](err)
	if !ok {
		return fmt.Errorf("validate request: %w", err)
	}

	var fields []apperr.FieldError
	seen := make(map[string]bool)
	for _, fe := range verrs {
		// Namespace is "Root.items[0].name"; drop the root type name, keep the path.
		_, path, _ := strings.Cut(fe.Namespace(), ".")
		if seen[path] {
			continue
		}
		seen[path] = true
		code, msg := describeTag(fe)
		fields = append(fields, apperr.FieldError{Field: path, Code: code, Message: msg})
	}
	return apperr.ErrValidation.WithFields(fields...)
}

// describeTag maps a failed validator tag to the field error code and an Indonesian message.
func describeTag(fe validator.FieldError) (code, msg string) {
	text := fe.Kind() == reflect.String || fe.Kind() == reflect.Slice || fe.Kind() == reflect.Map
	switch fe.Tag() {
	case "required", "required_if":
		return codeRequired, "Wajib diisi."
	case "uuid", "uuid4", "uuid7":
		return codeInvalid, "Harus berupa UUID yang valid."
	case "email":
		return codeInvalid, "Harus berupa email yang valid."
	case "username":
		return codeInvalid, "Harus 3 sampai 50 karakter: huruf, angka, titik, garis bawah atau tanda hubung."
	case "notblank":
		return codeRequired, "Tidak boleh kosong."
	case "printascii":
		return codeInvalid, "Hanya boleh berisi karakter ASCII yang dapat dicetak."
	case "min", "gte", "gt":
		if text {
			return codeTooShort, "Terlalu pendek (minimum " + fe.Param() + ")."
		}
		return codeTooSmall, "Terlalu kecil (minimum " + fe.Param() + ")."
	case "max", "lte", "lt":
		if text {
			return codeTooLong, "Terlalu panjang (maksimum " + fe.Param() + ")."
		}
		return codeTooLarge, "Terlalu besar (maksimum " + fe.Param() + ")."
	case "len":
		return codeInvalid, "Harus tepat " + fe.Param() + "."
	case "oneof":
		return codeInvalid, "Harus salah satu dari: " + fe.Param() + "."
	default:
		return codeInvalid, "Tidak valid."
	}
}
