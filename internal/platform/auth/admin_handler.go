package auth

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"siapp/internal/db"
	"siapp/internal/platform/apperr"
	"siapp/internal/platform/httpx"
)

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	actor, _ := From(r.Context()) // RequireAdmin has already made sure there is one

	user, err := h.service.CreateUser(r.Context(), CreateUserInput{
		ActorID:   actor.UserID,
		Email:     string(req.Email),
		Username:  req.Username,
		FullName:  req.FullName,
		Phone:     req.Phone,
		BirthDate: *req.BirthDate, // "required" in the request rules guarantees it is set
		Role:      db.UsersSystemRole(req.SystemRole),
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, newAdminUserDTO(user))
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	query, err := httpx.ParseListQuery(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	status := db.UsersStatus(r.URL.Query().Get("status"))
	if status != "" && status != db.UsersStatusACTIVE && status != db.UsersStatusDISABLED {
		httpx.Error(w, r, apperr.ErrValidationFailed.WithDetails([]httpx.FieldError{
			{Field: "status", Messages: []string{"harus salah satu dari: ACTIVE, DISABLED"}},
		}))
		return
	}

	users, err := h.service.ListUsers(r.Context(), ListUsersInput{
		Search: query.Q, Status: status, Cursor: query.Cursor, Limit: query.Limit,
	})
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	page := httpx.NewPage(users, query.Limit, func(u db.User) httpx.Cursor {
		return httpx.Cursor{T: u.CreatedAt, ID: u.ID}
	})
	httpx.JSON(w, http.StatusOK, httpx.Page[AdminUserDTO]{
		Items:      mapSlice(page.Items, newAdminUserDTO),
		NextCursor: page.NextCursor,
	})
}

func (h *Handler) getUser(w http.ResponseWriter, r *http.Request) {
	id, ok := userIDParam(w, r)
	if !ok {
		return
	}
	user, err := h.service.GetUser(r.Context(), id)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, newAdminUserDTO(user))
}

func (h *Handler) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := userIDParam(w, r)
	if !ok {
		return
	}
	var req UpdateUserRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if req.isEmpty() {
		httpx.Error(w, r, apperr.ErrValidationFailed.WithMessage("Tidak ada perubahan yang dikirim."))
		return
	}
	// A present username must be valid; an empty one clears it, so the request rules cannot
	// check it the way they do on create.
	if req.Username != nil && *req.Username != "" && !httpx.ValidUsername(strings.TrimSpace(*req.Username)) {
		httpx.Error(w, r, apperr.ErrValidationFailed.WithDetails([]httpx.FieldError{
			{Field: "username", Messages: []string{"harus 3 sampai 50 karakter: huruf, angka, titik, garis bawah atau tanda hubung"}},
		}))
		return
	}
	actor, _ := From(r.Context())

	in := UpdateUserInput{
		ActorID: actor.UserID, ID: id,
		FullName: req.FullName, Phone: req.Phone, Username: req.Username, BirthDate: req.BirthDate,
	}
	if req.SystemRole != nil {
		role := db.UsersSystemRole(*req.SystemRole)
		in.Role = &role
	}
	user, err := h.service.UpdateUser(r.Context(), in)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, newAdminUserDTO(user))
}

func (h *Handler) disableUser(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, db.UsersStatusDISABLED)
}

func (h *Handler) enableUser(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, db.UsersStatusACTIVE)
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request, status db.UsersStatus) {
	id, ok := userIDParam(w, r)
	if !ok {
		return
	}
	actor, _ := From(r.Context())
	user, err := h.service.SetUserStatus(r.Context(), actor.UserID, id, status)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, newAdminUserDTO(user))
}

// userIDParam reads the {id} of the path. A malformed ID cannot belong to any user, so it gets
// the same USER_NOT_FOUND as an unknown one.
func userIDParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.Error(w, r, ErrUserNotFound)
		return "", false
	}
	return id, true
}

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}
