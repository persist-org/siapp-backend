package httpx

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"siapp/internal/platform/apperr"
	"siapp/internal/platform/timeutil"
)

const maxSearchLen = 100

// ListQuery is the shared query string of list endpoints. Endpoints ignore the fields
// they do not support.
type ListQuery struct {
	Q          string        // free-text search, trimmed
	Date       timeutil.Date // zero when absent
	Month      string        // "YYYY-MM", "" when absent
	CategoryID string        // UUID, "" when absent
	Cursor     *Cursor       // nil for the first page
	Limit      int           // DefaultLimit when absent, capped at MaxLimit
}

// ParseListQuery reads q, date, month, categoryId, cursor and limit from the URL query.
// All problems are reported together as one VALIDATION_FAILED error.
func ParseListQuery(r *http.Request) (ListQuery, error) {
	values := r.URL.Query()
	q := ListQuery{Limit: DefaultLimit}
	var problems []FieldError
	fail := func(field, msg string) {
		problems = append(problems, FieldError{Field: field, Messages: []string{msg}})
	}

	q.Q = strings.TrimSpace(values.Get("q"))
	if len([]rune(q.Q)) > maxSearchLen {
		fail("q", "maksimum "+strconv.Itoa(maxSearchLen)+" karakter")
	}

	dateStr, monthStr := values.Get("date"), values.Get("month")
	switch {
	case dateStr != "" && monthStr != "":
		fail("date", "tidak boleh dikirim bersama month")
		fail("month", "tidak boleh dikirim bersama date")
	default:
		if dateStr != "" {
			d, err := timeutil.ParseDate(dateStr)
			if err != nil {
				fail("date", "harus berformat YYYY-MM-DD")
			}
			q.Date = d
		}
		if monthStr != "" {
			if _, _, err := timeutil.MonthRange(monthStr); err != nil {
				fail("month", "harus berformat YYYY-MM")
			}
			q.Month = monthStr
		}
	}

	if id := values.Get("categoryId"); id != "" {
		if _, err := uuid.Parse(id); err != nil {
			fail("categoryId", "harus berupa UUID yang valid")
		}
		q.CategoryID = id
	}

	if s := values.Get("cursor"); s != "" {
		c, err := DecodeCursor(s)
		if err != nil {
			fail("cursor", "tidak valid")
		}
		q.Cursor = &c
	}

	// A limit above the maximum is clamped, not rejected: asking for "as many as
	// allowed" is a reasonable thing for a client to do. Zero, negative or non-numeric
	// values are mistakes and are rejected.
	if s := values.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		switch {
		case err != nil || n < 1:
			fail("limit", "harus berupa angka minimal 1")
		default:
			q.Limit = min(n, MaxLimit)
		}
	}

	if len(problems) > 0 {
		return ListQuery{}, apperr.ErrValidationFailed.WithDetails(problems)
	}
	return q, nil
}
