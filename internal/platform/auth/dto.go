package auth

import (
	"siapp/internal/db"
	"siapp/internal/platform/timeutil"
)

// LoginRequest is the body of POST /auth/login.
type LoginRequest struct {
	// Identifier is the user's email or username.
	Identifier string `json:"identifier" validate:"required,max=255"`
	// Password is capped so a huge value cannot be used to burn CPU in argon2.
	Password   string `json:"password" validate:"required,max=256"`
	RememberMe bool   `json:"rememberMe"`
}

// LoginResponse is the body returned by a successful login.
type LoginResponse struct {
	AccessToken          string  `json:"accessToken"`
	AccessTokenExpiresAt string  `json:"accessTokenExpiresAt"`
	RefreshToken         string  `json:"refreshToken"`
	User                 UserDTO `json:"user"`
}

// UserDTO is the user as the API shows it. It deliberately has no password hash and no
// birthDate: the birthdate is personal data and doubles as the default password.
type UserDTO struct {
	ID         string  `json:"id"`
	Email      string  `json:"email"`
	Username   *string `json:"username"`
	FullName   string  `json:"fullName"`
	Phone      *string `json:"phone"`
	SystemRole string  `json:"systemRole"`
	Status     string  `json:"status"`
}

func newUserDTO(u db.User) UserDTO {
	dto := UserDTO{
		ID:         u.ID,
		Email:      u.Email,
		FullName:   u.FullName,
		SystemRole: string(u.SystemRole),
		Status:     string(u.Status),
	}
	if u.Username.Valid {
		dto.Username = &u.Username.String
	}
	if u.Phone.Valid {
		dto.Phone = &u.Phone.String
	}
	return dto
}

func newLoginResponse(r LoginResult) LoginResponse {
	return LoginResponse{
		AccessToken:          r.AccessToken,
		AccessTokenExpiresAt: timeutil.FormatTimestamp(r.AccessTokenExpiresAt),
		RefreshToken:         r.RefreshToken,
		User:                 newUserDTO(r.User),
	}
}
