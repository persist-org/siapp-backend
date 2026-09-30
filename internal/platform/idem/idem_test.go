package idem_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"siapp/internal/platform/apperr"
	"siapp/internal/platform/httpx"
	"siapp/internal/platform/idem"
)

func TestCheckOccurredAt(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		at   time.Time
		want *apperr.Error
	}{
		{"now", now, nil},
		{"exactly 5 min ahead", now.Add(5 * time.Minute), nil},
		{"just over 5 min ahead", now.Add(5*time.Minute + time.Millisecond), apperr.ErrOccurredAtInFuture},
		{"a day ahead", now.Add(24 * time.Hour), apperr.ErrOccurredAtInFuture},
		{"exactly 7 days old", now.Add(-7 * 24 * time.Hour), nil},
		{"just over 7 days old", now.Add(-7*24*time.Hour - time.Millisecond), apperr.ErrOccurredAtTooOld},
		{"a month old", now.Add(-30 * 24 * time.Hour), apperr.ErrOccurredAtTooOld},
		{"another zone, same instant", now.In(time.FixedZone("WIT", 9*3600)), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := idem.CheckOccurredAt(tc.at, now)
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestOfflineCreateValidation(t *testing.T) {
	const v7 = "0193a5d8-7c1e-7000-8000-000000000001"
	at := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		in        idem.OfflineCreate
		wantField string // "" when valid
	}{
		{"valid", idem.OfflineCreate{ID: v7, OccurredAt: at}, ""},
		{"missing id", idem.OfflineCreate{OccurredAt: at}, "id"},
		{"not a uuid", idem.OfflineCreate{ID: "abc", OccurredAt: at}, "id"},
		{"uuid v4", idem.OfflineCreate{ID: "3f2b8c1e-5d4a-4b6c-9e7f-1a2b3c4d5e6f", OccurredAt: at}, "id"},
		{"uppercase v7", idem.OfflineCreate{ID: "0193A5D8-7C1E-7000-8000-000000000001", OccurredAt: at}, "id"},
		{"braced v7", idem.OfflineCreate{ID: "{" + v7 + "}", OccurredAt: at}, "id"},
		{"missing occurredAt", idem.OfflineCreate{ID: v7}, "occurredAt"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := httpx.Validate(&tc.in)
			if tc.wantField == "" {
				require.NoError(t, err)
				return
			}
			var appErr *apperr.Error
			require.ErrorAs(t, err, &appErr)
			fields, ok := appErr.Details.([]httpx.FieldError)
			require.True(t, ok)
			require.Equal(t, tc.wantField, fields[0].Field)
		})
	}
}

type record struct{ ID, Note string }

const (
	alice = "user-alice"
	bob   = "user-bob"
)

func duplicateKey(key string) error {
	return &mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key '" + key + "'"}
}

// fakeStore drives Create through find/insert closures and counts the calls.
type fakeStore struct {
	finds   []findResult // consumed in order, the last one repeats
	insert  func() (record, error)
	nFinds  int
	nInsert int
}

type findResult struct {
	rec   record
	owner string
	found bool
	err   error
}

func (s *fakeStore) run(t *testing.T, userID string) (record, bool, error) {
	t.Helper()
	return idem.Create(t.Context(),
		func() (record, string, bool, error) {
			i := min(s.nFinds, len(s.finds)-1)
			s.nFinds++
			f := s.finds[i]
			return f.rec, f.owner, f.found, f.err
		},
		func() (record, error) {
			s.nInsert++
			return s.insert()
		},
		userID,
	)
}

func TestCreate(t *testing.T) {
	stored := record{ID: "r1", Note: "stored"}
	fresh := record{ID: "r1", Note: "fresh"}
	newRec := func() (record, error) { return fresh, nil }

	t.Run("new id inserts and is not a replay", func(t *testing.T) {
		s := &fakeStore{finds: []findResult{{}}, insert: newRec}
		got, replayed, err := s.run(t, alice)
		require.NoError(t, err)
		require.False(t, replayed)
		require.Equal(t, fresh, got)
		require.Equal(t, 1, s.nInsert)
	})

	t.Run("replay by the same owner returns the stored record without inserting", func(t *testing.T) {
		s := &fakeStore{finds: []findResult{{rec: stored, owner: alice, found: true}}, insert: newRec}
		got, replayed, err := s.run(t, alice)
		require.NoError(t, err)
		require.True(t, replayed)
		require.Equal(t, stored, got)
		require.Zero(t, s.nInsert)
	})

	t.Run("same id from another user is ID_CONFLICT and leaks nothing", func(t *testing.T) {
		s := &fakeStore{finds: []findResult{{rec: stored, owner: alice, found: true}}, insert: newRec}
		got, replayed, err := s.run(t, bob)
		require.ErrorIs(t, err, apperr.ErrIDConflict)
		require.False(t, replayed)
		require.Zero(t, got)
		require.Zero(t, s.nInsert)
	})

	t.Run("race: duplicate primary key, then the winner is the same owner", func(t *testing.T) {
		s := &fakeStore{
			finds:  []findResult{{}, {rec: stored, owner: alice, found: true}},
			insert: func() (record, error) { return record{}, duplicateKey("reports.PRIMARY") },
		}
		got, replayed, err := s.run(t, alice)
		require.NoError(t, err)
		require.True(t, replayed)
		require.Equal(t, stored, got)
		require.Equal(t, 2, s.nFinds)
	})

	t.Run("race: duplicate primary key, then the winner is another user", func(t *testing.T) {
		s := &fakeStore{
			finds:  []findResult{{}, {rec: stored, owner: alice, found: true}},
			insert: func() (record, error) { return record{}, duplicateKey("reports.PRIMARY") },
		}
		_, replayed, err := s.run(t, bob)
		require.ErrorIs(t, err, apperr.ErrIDConflict)
		require.False(t, replayed)
	})

	t.Run("duplicate on another unique key is returned unchanged", func(t *testing.T) {
		dup := duplicateKey("users.uq_users_email")
		s := &fakeStore{finds: []findResult{{}}, insert: func() (record, error) { return record{}, dup }}
		_, replayed, err := s.run(t, alice)
		require.ErrorIs(t, err, dup)
		require.False(t, replayed)
		require.Equal(t, 1, s.nFinds, "no second find for a non-primary duplicate")
	})

	t.Run("duplicate primary key but the row stays invisible is an error", func(t *testing.T) {
		s := &fakeStore{
			finds:  []findResult{{}},
			insert: func() (record, error) { return record{}, duplicateKey("reports.PRIMARY") },
		}
		_, _, err := s.run(t, alice)
		require.Error(t, err)
		require.NotErrorIs(t, err, apperr.ErrIDConflict)
	})

	t.Run("other insert errors pass through", func(t *testing.T) {
		errBoom := errors.New("boom")
		s := &fakeStore{finds: []findResult{{}}, insert: func() (record, error) { return record{}, errBoom }}
		_, _, err := s.run(t, alice)
		require.ErrorIs(t, err, errBoom)
	})

	t.Run("find errors pass through, before and after a duplicate", func(t *testing.T) {
		errBoom := errors.New("boom")
		s := &fakeStore{finds: []findResult{{err: errBoom}}, insert: newRec}
		_, _, err := s.run(t, alice)
		require.ErrorIs(t, err, errBoom)
		require.Zero(t, s.nInsert)

		s = &fakeStore{
			finds:  []findResult{{}, {err: errBoom}},
			insert: func() (record, error) { return record{}, duplicateKey("reports.PRIMARY") },
		}
		_, _, err = s.run(t, alice)
		require.ErrorIs(t, err, errBoom)
	})

	t.Run("a cancelled context does nothing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		calls := 0
		_, _, err := idem.Create(ctx,
			func() (record, string, bool, error) { calls++; return record{}, "", false, nil },
			func() (record, error) { calls++; return fresh, nil },
			alice)
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, calls)
	})
}
