package testkit

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
)

// DefaultPassword is the password of factory users that do not set one.
const DefaultPassword = "Password123!"

// UserOpts are the fields of a factory user. Zero values get defaults.
type UserOpts struct {
	Email    string // default: <random>@example.com
	Username string // default: none
	Password string // default: DefaultPassword
	FullName string // default: "Test User"
	Role     db.UsersSystemRole
	Status   db.UsersStatus
}

// User is a factory user with the plain password, which the database does not keep.
type User struct {
	ID       string
	Email    string
	Username string
	Password string
	Role     db.UsersSystemRole
}

// CreateUser inserts a user with a hash made by the cheap test hasher.
func CreateUser(t *testing.T, pool *sql.DB, o UserOpts) User {
	t.Helper()
	id, err := uuid.NewV7()
	require.NoError(t, err)

	if o.Email == "" {
		o.Email = "user-" + randomHex(t) + "@example.com"
	}
	if o.Password == "" {
		o.Password = DefaultPassword
	}
	if o.FullName == "" {
		o.FullName = "Test User"
	}
	if o.Role == "" {
		o.Role = db.UsersSystemRoleMEMBER
	}
	if o.Status == "" {
		o.Status = db.UsersStatusACTIVE
	}

	hash, err := Passwords.Hash(o.Password)
	require.NoError(t, err)

	err = db.New(pool).CreateUser(t.Context(), db.CreateUserParams{
		ID:           id.String(),
		Email:        o.Email,
		Username:     sql.NullString{String: o.Username, Valid: o.Username != ""},
		FullName:     o.FullName,
		SystemRole:   o.Role,
		Status:       o.Status,
		PasswordHash: hash,
	})
	require.NoError(t, err)

	return User{ID: id.String(), Email: o.Email, Username: o.Username, Password: o.Password, Role: o.Role}
}
