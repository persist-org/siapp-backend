package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"siapp/pkg/platform/apperr"
)

// JSON writes v as a JSON response with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so all that is left is to record it.
		slog.Error("write json response", "error", err)
	}
}

type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// Error writes err as the standard error response {"error": {code, message, details?}}.
//
// An *apperr.Error is an expected failure and is sent as is. Anything else is a bug or an
// infrastructure failure: it is logged with the request ID and the client only sees a
// generic INTERNAL_ERROR, so internals (SQL, hostnames) never leak.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		slog.ErrorContext(r.Context(), "unhandled error",
			slog.String("error", err.Error()),
			slog.String("requestId", RequestID(r.Context())),
		)
		appErr = apperr.ErrInternal
	}
	writeAppError(w, appErr)
}

func writeAppError(w http.ResponseWriter, e *apperr.Error) {
	JSON(w, e.Status, errorBody{Error: errorPayload{Code: e.Code, Message: e.Message, Details: e.Details}})
}
