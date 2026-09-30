package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"siapp/internal/db"
	"siapp/internal/platform/apperr"
	"siapp/internal/platform/audit"
	"siapp/internal/platform/database"
	"siapp/internal/platform/httpx"
	"siapp/internal/platform/timeutil"
)

// Names of the unique keys in the users table, as reported by MySQL on a duplicate.
const (
	keyActiveEmail    = "uq_users_active_email"
	keyActiveUsername = "uq_users_active_username"
)

// CreateUserInput is a new user, as an admin enters it.
type CreateUserInput struct {
	ActorID   string // the admin
	Email     string
	Username  string // optional
	FullName  string
	Phone     string // optional
	BirthDate timeutil.Date
	Role      db.UsersSystemRole
}

// CreateUser creates an ACTIVE user whose password is DefaultPassword(BirthDate). There is no
// invitation or activation step: the user can log in straight away.
func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (db.User, error) {
	now := s.clock.Now()
	if err := checkBirthDate(in.BirthDate, now); err != nil {
		return db.User{}, err
	}

	id, err := uuid.NewV7()
	if err != nil {
		return db.User{}, fmt.Errorf("generate user id: %w", err)
	}
	// Hashed before the transaction: argon2 takes around 100 ms and there is no reason to
	// hold a database transaction open for it.
	hash, err := s.passwords.Hash(DefaultPassword(in.BirthDate))
	if err != nil {
		return db.User{}, err
	}

	var user db.User
	err = database.WithTx(ctx, s.pool, func(q *db.Queries) error {
		err := q.CreateUser(ctx, db.CreateUserParams{
			ID:           id.String(),
			Email:        strings.ToLower(strings.TrimSpace(in.Email)),
			Username:     nullString(strings.TrimSpace(in.Username)),
			FullName:     strings.TrimSpace(in.FullName),
			Phone:        nullString(strings.TrimSpace(in.Phone)),
			BirthDate:    sql.NullTime{Time: in.BirthDate.Time(), Valid: true},
			SystemRole:   in.Role,
			Status:       db.UsersStatusACTIVE,
			PasswordHash: hash,
		})
		if err != nil {
			return mapUserWriteError("create user", err)
		}
		if user, err = q.GetUserByID(ctx, id.String()); err != nil {
			return fmt.Errorf("reload created user: %w", err)
		}
		// The audit row holds no birth date and no password: the birth date is the default
		// password.
		return audit.Record(ctx, q, audit.Entry{
			ActorID: in.ActorID, Action: "user.create", EntityType: "user", EntityID: user.ID,
			Metadata: map[string]any{"systemRole": string(in.Role)}, At: now,
		})
	})
	return user, err
}

// GetUser returns one user.
func (s *Service) GetUser(ctx context.Context, id string) (db.User, error) {
	user, err := s.q.GetUserByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return db.User{}, ErrUserNotFound
	}
	if err != nil {
		return db.User{}, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}

// ListUsersInput filters and pages the user list.
type ListUsersInput struct {
	Search string         // matches name, email or username; empty for all
	Status db.UsersStatus // empty for all
	Cursor *httpx.Cursor  // nil for the first page
	Limit  int            // page size, already capped by httpx
}

// ListUsers returns one page of users, newest first, as limit+1 rows so that the caller can
// tell whether another page exists (see httpx.NewPage).
func (s *Service) ListUsers(ctx context.Context, in ListUsersInput) ([]db.User, error) {
	params := db.ListUsersParams{
		Status: db.NullUsersStatus{UsersStatus: in.Status, Valid: in.Status != ""},
		Limit:  httpx.FetchLimit(in.Limit),
	}
	if in.Search != "" {
		params.Search = sql.NullString{String: "%" + escapeLike(in.Search) + "%", Valid: true}
	}
	if in.Cursor != nil {
		params.CursorTime = sql.NullTime{Time: in.Cursor.T, Valid: true}
		params.CursorID = sql.NullString{String: in.Cursor.ID, Valid: true}
	}

	users, err := s.q.ListUsers(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

// escapeLike makes user input match literally in a LIKE pattern. Without it a search for
// "50%" or "_" would match far more than the admin typed.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// UpdateUserInput holds the fields an admin changes. A nil field is left alone.
type UpdateUserInput struct {
	ActorID  string
	ID       string
	FullName *string
	// Phone and Username: an empty string clears the value.
	Phone     *string
	Username  *string
	BirthDate *timeutil.Date
	Role      *db.UsersSystemRole
}

// UpdateUser changes a user's profile. It reads the row with a lock, applies the changes and
// writes, so two admins editing the same user cannot overwrite each other unnoticed.
//
// Changing BirthDate does not change the password: the default password was built from the
// old date once, at creation.
func (s *Service) UpdateUser(ctx context.Context, in UpdateUserInput) (db.User, error) {
	now := s.clock.Now()
	var result db.User

	err := database.WithTx(ctx, s.pool, func(q *db.Queries) error {
		user, err := q.GetUserByIDForUpdate(ctx, in.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}

		params := db.UpdateUserParams{
			ID: user.ID, FullName: user.FullName, Phone: user.Phone, BirthDate: user.BirthDate,
			Username: user.Username, SystemRole: user.SystemRole,
		}
		var changed []string
		meta := map[string]any{}

		if in.FullName != nil && strings.TrimSpace(*in.FullName) != user.FullName {
			params.FullName = strings.TrimSpace(*in.FullName)
			changed = append(changed, "fullName")
		}
		if in.Phone != nil {
			if v := nullString(strings.TrimSpace(*in.Phone)); v != user.Phone {
				params.Phone = v
				changed = append(changed, "phone")
			}
		}
		if in.Username != nil {
			if v := nullString(strings.TrimSpace(*in.Username)); v != user.Username {
				params.Username = v
				changed = append(changed, "username")
			}
		}
		if in.BirthDate != nil {
			if err := checkBirthDate(*in.BirthDate, now); err != nil {
				return err
			}
			// Compared with Equal: two time.Time values for the same day can differ in their
			// zone pointer, which == would treat as a difference.
			if !user.BirthDate.Valid || !user.BirthDate.Time.Equal(in.BirthDate.Time()) {
				params.BirthDate = sql.NullTime{Time: in.BirthDate.Time(), Valid: true}
				changed = append(changed, "birthDate") // the value itself is not audited
			}
		}
		if in.Role != nil && *in.Role != user.SystemRole {
			// An admin who could demote themselves could leave the system with no admin.
			if in.ActorID == user.ID {
				return apperr.ErrConflict.WithMessage("Anda tidak dapat mengubah peran akun Anda sendiri.")
			}
			params.SystemRole = *in.Role
			changed = append(changed, "systemRole")
			meta["systemRole"] = map[string]string{"from": string(user.SystemRole), "to": string(*in.Role)}
		}

		if len(changed) == 0 {
			result = user
			return nil
		}
		if err := q.UpdateUser(ctx, params); err != nil {
			return mapUserWriteError("update user", err)
		}
		meta["changed"] = changed
		if result, err = q.GetUserByID(ctx, user.ID); err != nil {
			return fmt.Errorf("reload user: %w", err)
		}
		return audit.Record(ctx, q, audit.Entry{
			ActorID: in.ActorID, Action: "user.update", EntityType: "user", EntityID: user.ID, Metadata: meta, At: now,
		})
	})
	return result, err
}

// SetUserStatus disables or enables a user. Doing what is already the case changes nothing
// and writes no audit row.
//
// Disabling also ends every session of the user in the same transaction and, once it has
// committed, cuts off their access tokens through the revocation list: a disabled user is
// locked out at once, not after their 15 minute token runs out.
func (s *Service) SetUserStatus(ctx context.Context, actorID, id string, status db.UsersStatus) (db.User, error) {
	now := s.clock.Now()
	var result db.User
	var recent []string

	err := database.WithTx(ctx, s.pool, func(q *db.Queries) error {
		user, err := q.GetUserByIDForUpdate(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("lock user: %w", err)
		}
		if status == db.UsersStatusDISABLED && actorID == user.ID {
			return apperr.ErrConflict.WithMessage("Anda tidak dapat menonaktifkan akun Anda sendiri.")
		}
		if user.Status == status {
			result = user
			return nil
		}

		if err := q.SetUserStatus(ctx, db.SetUserStatusParams{ID: user.ID, Status: status}); err != nil {
			return fmt.Errorf("set user status: %w", err)
		}
		action := "user.enable"
		if status == db.UsersStatusDISABLED {
			action = "user.disable"
			if recent, _, err = revokeAllSessions(ctx, q, user.ID, now); err != nil {
				return err
			}
		}
		if result, err = q.GetUserByID(ctx, user.ID); err != nil {
			return fmt.Errorf("reload user: %w", err)
		}
		return audit.Record(ctx, q, audit.Entry{
			ActorID: actorID, Action: action, EntityType: "user", EntityID: user.ID, At: now,
		})
	})
	if err != nil {
		return db.User{}, err
	}
	s.revocations.Revoke(ctx, recent...)
	return result, nil
}

// checkBirthDate rejects a date that cannot be a living person's birthday. "Today" is taken in
// UTC+9, the furthest-ahead Indonesian time zone, so a birthday that is already today there is
// not refused.
func checkBirthDate(d timeutil.Date, now time.Time) error {
	today := timeutil.BusinessDate(now, time.FixedZone("WIT", 9*60*60))
	if d.IsZero() || d.Before(timeutil.NewDate(minBirthYear, time.January, 1)) || d.After(today) {
		return apperr.ErrValidationFailed.WithDetails([]httpx.FieldError{
			{Field: "birthDate", Messages: []string{"tanggal lahir tidak valid"}},
		})
	}
	return nil
}

// mapUserWriteError turns a duplicate-key error on an email or username into the error the
// admin should see. The key name is how MySQL says which one collided.
func mapUserWriteError(what string, err error) error {
	if database.IsDuplicateKey(err) {
		switch database.DuplicateKeyName(err) {
		case keyActiveEmail:
			return ErrEmailTaken
		case keyActiveUsername:
			return ErrUsernameTaken
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
