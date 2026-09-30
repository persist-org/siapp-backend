package auth

import (
	"siapp/internal/db"
	"siapp/internal/platform/httpx"
	"siapp/internal/platform/timeutil"
)

// CreateUserRequest is the body of POST /admin/users. There is no password field: the
// password is the birthdate, see DefaultPassword.
type CreateUserRequest struct {
	Email    httpx.Email `json:"email" validate:"required,email,max=255"`
	Username string      `json:"username" validate:"omitempty,username"`
	FullName string      `json:"fullName" validate:"required,notblank,max=150"`
	Phone    string      `json:"phone" validate:"omitempty,max=30"`
	// BirthDate is a pointer so that a missing value is told apart from a zero one.
	BirthDate  *timeutil.Date `json:"birthDate" validate:"required"`
	SystemRole string         `json:"systemRole" validate:"required,oneof=ADMIN MEMBER"`
}

// UpdateUserRequest is the body of PATCH /admin/users/{id}. Every field is optional and a
// missing one is left alone. An empty username or phone clears it.
type UpdateUserRequest struct {
	FullName   *string        `json:"fullName" validate:"omitempty,notblank,max=150"`
	Phone      *string        `json:"phone" validate:"omitempty,max=30"`
	Username   *string        `json:"username" validate:"omitempty,max=50"`
	BirthDate  *timeutil.Date `json:"birthDate"`
	SystemRole *string        `json:"systemRole" validate:"omitempty,oneof=ADMIN MEMBER"`
}

func (r UpdateUserRequest) isEmpty() bool {
	return r.FullName == nil && r.Phone == nil && r.Username == nil && r.BirthDate == nil && r.SystemRole == nil
}

// AdminUserDTO is a user as an admin sees it. It has no password hash. It does include the
// birth date, which admins enter and edit; it is never shown to other users.
type AdminUserDTO struct {
	ID          string        `json:"id"`
	Email       string        `json:"email"`
	Username    *string       `json:"username"`
	FullName    string        `json:"fullName"`
	Phone       *string       `json:"phone"`
	BirthDate   timeutil.Date `json:"birthDate"`
	SystemRole  string        `json:"systemRole"`
	Status      string        `json:"status"`
	LastLoginAt *string       `json:"lastLoginAt"`
	CreatedAt   string        `json:"createdAt"`
}

func newAdminUserDTO(u db.User) AdminUserDTO {
	dto := AdminUserDTO{
		ID: u.ID, Email: u.Email, FullName: u.FullName,
		SystemRole: string(u.SystemRole), Status: string(u.Status),
		CreatedAt: timeutil.FormatTimestamp(u.CreatedAt),
	}
	if u.Username.Valid {
		dto.Username = &u.Username.String
	}
	if u.Phone.Valid {
		dto.Phone = &u.Phone.String
	}
	if u.BirthDate.Valid {
		dto.BirthDate = timeutil.NewDate(u.BirthDate.Time.Year(), u.BirthDate.Time.Month(), u.BirthDate.Time.Day())
	}
	if u.LastLoginAt.Valid {
		s := timeutil.FormatTimestamp(u.LastLoginAt.Time)
		dto.LastLoginAt = &s
	}
	return dto
}

// ForgotPasswordRequest is the body of POST /auth/forgot-password.
type ForgotPasswordRequest struct {
	Email httpx.Email `json:"email" validate:"required,email,max=255"`
}

// ResetPasswordRequest is the body of POST /auth/reset-password.
type ResetPasswordRequest struct {
	// Token is the secret from the emailed link; capped, since real ones are 43 characters.
	Token string `json:"token" validate:"required,max=256"`
	// NewPassword is capped so that a huge value cannot be used to burn CPU in argon2. The
	// password rules themselves are checked by ValidatePassword.
	NewPassword string `json:"newPassword" validate:"required,max=256"`
}

// MessageResponse is a body that only carries a message for the user.
type MessageResponse struct {
	Message string `json:"message"`
}
