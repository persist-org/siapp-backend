package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/timeutil"
)

// Every response of /api/v1/* is one of the contract's four envelopes (SuccessEnvelope,
// ListEnvelope, DeltaMeta shape, ErrorEnvelope), described in docs/api-response.md.

type meta struct {
	RequestID string `json:"request_id"`
}

type successEnvelope struct {
	Success bool `json:"success"`
	Data    any  `json:"data"`
	Meta    meta `json:"meta"`
}

type listEnvelope[T any] struct {
	Success    bool       `json:"success"`
	Data       []T        `json:"data"`
	Pagination pagination `json:"pagination"`
	Meta       meta       `json:"meta"`
}

type pagination struct {
	Page       int `json:"page"`
	Limit      int `json:"limit"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

type deltaEnvelope[T, D any] struct {
	Success bool      `json:"success"`
	Data    []T       `json:"data"`
	Deleted []D       `json:"deleted"`
	Meta    deltaMeta `json:"meta"`
}

type deltaMeta struct {
	RequestID  string `json:"request_id"`
	ServerTime string `json:"server_time"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

type errorEnvelope struct {
	Success bool         `json:"success"`
	Error   errorPayload `json:"error"`
	Meta    meta         `json:"meta"`
}

type errorPayload struct {
	Code    apperr.Code         `json:"code"`
	Message string              `json:"message"`
	Details map[string]any      `json:"details,omitempty"`
	Errors  []apperr.FieldError `json:"errors,omitempty"`
}

// JSON writes v as a JSON response with the given status. Use OK, List, Delta and Error
// for API responses; JSON is for the few bodies that are not envelopes (the health check).
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so all that is left is to record it.
		slog.Error("write json response", "error", err)
	}
}

// OK writes a SuccessEnvelope. Use http.StatusCreated for creates and http.StatusOK for
// everything else. An action with nothing to return passes nil, which sends "data": null.
func OK(w http.ResponseWriter, r *http.Request, status int, data any) {
	JSON(w, status, successEnvelope{Success: true, Data: data, Meta: meta{RequestID: RequestID(r.Context())}})
}

// List writes a ListEnvelope with status 200. A nil data slice is sent as [], never null.
func List[T any](w http.ResponseWriter, r *http.Request, data []T, page Page) {
	if data == nil {
		data = []T{}
	}
	JSON(w, http.StatusOK, listEnvelope[T]{
		Success:    true,
		Data:       data,
		Pagination: pagination{Page: page.Page, Limit: page.Limit, Total: page.Total, TotalPages: page.TotalPages()},
		Meta:       meta{RequestID: RequestID(r.Context())},
	})
}

// DeltaMeta is the sync bookkeeping of a delta response (DL-34, DL-57).
type DeltaMeta struct {
	// ServerTime is the watermark the client sends as the next `since`. Mandatory.
	ServerTime time.Time
	// NextCursor is set only when HasMore is true.
	NextCursor string
	HasMore    bool
}

// Delta writes the offline-sync response with status 200: rows changed since `since`, the
// tombstones of rows removed since then, and meta.server_time. It carries no pagination.
// Nil slices are sent as [].
func Delta[T, D any](w http.ResponseWriter, r *http.Request, data []T, deleted []D, m DeltaMeta) {
	if data == nil {
		data = []T{}
	}
	if deleted == nil {
		deleted = []D{}
	}
	JSON(w, http.StatusOK, deltaEnvelope[T, D]{
		Success: true,
		Data:    data,
		Deleted: deleted,
		Meta: deltaMeta{
			RequestID:  RequestID(r.Context()),
			ServerTime: timeutil.FormatTimestamp(m.ServerTime),
			NextCursor: m.NextCursor,
			HasMore:    m.HasMore,
		},
	})
}

// Error writes err as an ErrorEnvelope.
//
// An *apperr.Error is an expected failure and is sent as is. Anything else is a bug or an
// infrastructure failure: it is logged with the request ID and the client only sees a
// generic INTERNAL_ERROR, so internals (SQL, hostnames) never leak.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	appErr, ok := errors.AsType[*apperr.Error](err)
	if !ok {
		slog.ErrorContext(r.Context(), "unhandled error",
			slog.String("error", err.Error()),
			slog.String("requestId", RequestID(r.Context())),
		)
		appErr = apperr.ErrInternal
	}
	writeAppError(w, r, appErr)
}

func writeAppError(w http.ResponseWriter, r *http.Request, e *apperr.Error) {
	var details map[string]any
	if e.Reason != "" || len(e.Details) > 0 {
		details = make(map[string]any, len(e.Details)+1)
		maps.Copy(details, e.Details)
		if e.Reason != "" {
			details["reason"] = e.Reason
		}
	}
	JSON(w, e.Status, errorEnvelope{
		Success: false,
		Error:   errorPayload{Code: e.Code, Message: e.Message, Details: details, Errors: e.Fields},
		Meta:    meta{RequestID: RequestID(r.Context())},
	})
}
