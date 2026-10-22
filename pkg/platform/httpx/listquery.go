package httpx

import (
	"net/http"
	"strconv"
	"strings"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/timeutil"
)

const maxSearchLen = 100

// maxPage bounds page so that the offset, (page-1)*limit, stays far inside an int32: a huge
// page would otherwise overflow into a negative OFFSET, which the database refuses (a 500).
const maxPage = 1_000_000

// ListQuery is the shared query string of list endpoints. Endpoints ignore the fields
// they do not support.
type ListQuery struct {
	Q          string        // free-text search, trimmed; "" when absent
	Date       timeutil.Date // zero when absent
	Month      string        // "YYYY-MM", "" when absent
	CategoryID int64         // 0 when absent
	Page       int           // DefaultPage when absent
	Limit      int           // DefaultLimit when absent, at most MaxLimit
}

// Offset is the SQL OFFSET for the requested page.
func (q ListQuery) Offset() int { return (q.Page - 1) * q.Limit }

// WithTotal returns the Page block for a response, given the total row count.
func (q ListQuery) WithTotal(total int) Page { return Page{Page: q.Page, Limit: q.Limit, Total: total} }

// ParseListQuery reads q, date, month, category_id, page and limit from the URL query.
// All problems are reported together as one VALIDATION_ERROR.
func ParseListQuery(r *http.Request) (ListQuery, error) {
	values := r.URL.Query()
	q := ListQuery{Page: DefaultPage, Limit: DefaultLimit}
	var problems []apperr.FieldError
	fail := func(field, code, msg string) {
		problems = append(problems, apperr.FieldError{Field: field, Code: code, Message: msg})
	}

	q.Q = strings.TrimSpace(values.Get("q"))
	if len([]rune(q.Q)) > maxSearchLen {
		fail("q", codeTooLong, "Maksimum "+strconv.Itoa(maxSearchLen)+" karakter.")
	}

	dateStr, monthStr := values.Get("date"), values.Get("month")
	switch {
	case dateStr != "" && monthStr != "":
		fail("date", codeInvalid, "Tidak boleh dikirim bersama month.")
		fail("month", codeInvalid, "Tidak boleh dikirim bersama date.")
	default:
		if dateStr != "" {
			d, err := timeutil.ParseDate(dateStr)
			if err != nil {
				fail("date", codeInvalid, "Harus berformat YYYY-MM-DD.")
			}
			q.Date = d
		}
		if monthStr != "" {
			if _, _, err := timeutil.MonthRange(monthStr); err != nil {
				fail("month", codeInvalid, "Harus berformat YYYY-MM.")
			}
			q.Month = monthStr
		}
	}

	if s := values.Get("category_id"); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id < 1 {
			fail("category_id", codeInvalid, "Harus berupa angka positif.")
		}
		q.CategoryID = id
	}

	if s := values.Get("page"); s != "" {
		n, err := strconv.Atoi(s)
		switch {
		case err != nil || n < 1:
			fail("page", codeInvalid, "Harus berupa angka minimal 1.")
		case n > maxPage:
			fail("page", codeTooLarge, "Maksimum "+strconv.Itoa(maxPage)+".")
		default:
			q.Page = n
		}
	}

	// The contract's maximum is 100; above it the request is rejected, not clamped, so a
	// client that asks for more finds out instead of silently getting less.
	if s := values.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		switch {
		case err != nil || n < 1:
			fail("limit", codeInvalid, "Harus berupa angka minimal 1.")
		case n > MaxLimit:
			fail("limit", codeTooLarge, "Maksimum "+strconv.Itoa(MaxLimit)+".")
		default:
			q.Limit = n
		}
	}

	if len(problems) > 0 {
		return ListQuery{}, apperr.ErrValidation.WithFields(problems...)
	}
	return q, nil
}
