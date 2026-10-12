package auth

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"siapp/internal/db"
	"siapp/internal/platform/database"
)

const (
	minSeedPasswordLen = 8
	seedAdminName      = "Administrator"
)

// SeedAdmin creates the first ADMIN when no admin exists, and reports whether it did. It is
// idempotent: with an admin already present it changes nothing, so it is safe to run on
// every deploy.
//
// The email is stored lowercase like every other email. A duplicate email that belongs to
// a non-admin user is an error, not a silent success, so the operator notices that no
// admin exists.
func SeedAdmin(ctx context.Context, q *db.Queries, passwords *Passwords, email, password string) (created bool, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return false, fmt.Errorf("seed admin: %q is not a valid email", email)
	}
	if len(password) < minSeedPasswordLen {
		return false, fmt.Errorf("seed admin: password must be at least %d characters", minSeedPasswordLen)
	}

	admins, err := q.CountAdmins(ctx)
	if err != nil {
		return false, fmt.Errorf("count admins: %w", err)
	}
	if admins > 0 {
		return false, nil
	}

	hash, err := passwords.Hash(password)
	if err != nil {
		return false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return false, fmt.Errorf("generate user id: %w", err)
	}
	err = q.CreateUser(ctx, db.CreateUserParams{
		ID:           id.String(),
		Email:        email,
		FullName:     seedAdminName,
		SystemRole:   db.UsersSystemRoleADMIN,
		Status:       db.UsersStatusACTIVE,
		PasswordHash: hash,
		Username:     sql.NullString{},
	})
	if database.IsDuplicateKey(err) {
		return false, fmt.Errorf("seed admin: email %s is already used by a user who is not an admin", email)
	}
	if err != nil {
		return false, fmt.Errorf("create admin: %w", err)
	}
	return true, nil
}
