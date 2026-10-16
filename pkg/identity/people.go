package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/personel"
)

// personelDirectory is the part of the Personel client People needs (seam S4).
type personelDirectory interface {
	EmployeeByNIK(ctx context.Context, orgID int64, nik string) (personel.Identity, error)
}

// People finds or creates the local copy of a Personalia person by NIK. Other modules use it
// when they need a person who has never signed in, for example module 02 adding a team member.
type People struct {
	pool      *pgxpool.Pool
	directory personelDirectory
	clock     clock.Clock
	repo      repo
}

// NewPeople returns People.
func NewPeople(pool *pgxpool.Pool, directory personelDirectory, c clock.Clock) *People {
	return &People{pool: pool, directory: directory, clock: c}
}

// Ensure returns the local personnels.id of the person with this NIK in the organization,
// fetching them from Personalia and inserting the row if they are not known yet.
//
// organizationID is the caller's organization (from auth.From, or the command line for the
// seed): never a request value. A NIK whose current company is not that organization is
// NOT_FOUND, so one tenant can never pull another tenant's people into its own cache.
//
// A person Personalia reports as inactive is cached as INACTIVE and still returned: the
// caller decides what to do with them (module 02 refuses to put them in a team).
//
// ⚠ Two parallel calls for the same NIK create ONE row: both miss the lookup, both upsert on
// (organization, Personalia user id), and the second turns into an update.
func (p *People) Ensure(ctx context.Context, organizationID int64, nik string) (int64, error) {
	nik = strings.TrimSpace(nik)
	if organizationID <= 0 || nik == "" {
		return 0, apperr.ErrNotFound
	}

	var id int64
	var found bool
	if err := database.WithTenant(ctx, p.pool, organizationID, func(q *db.Queries) error {
		row, ok, err := p.repo.personnelByNIK(ctx, q, nik)
		id, found = row.ID, ok
		return err
	}); err != nil {
		return 0, err
	}
	if found {
		return id, nil
	}

	identity, err := p.directory.EmployeeByNIK(ctx, organizationID, nik)
	switch {
	case errors.Is(err, personel.ErrNotFound):
		return 0, apperr.ErrNotFound
	case errors.Is(err, personel.ErrUnavailable):
		return 0, apperr.ErrPersonelUnavailable
	case errors.Is(err, personel.ErrRateLimited):
		return 0, apperr.ErrRateLimited
	case err != nil:
		return 0, fmt.Errorf("personel employee lookup: %w", err)
	}
	// The lookup is scoped to the organization, so this is a second lock on the same door.
	if identity.OrganizationID != organizationID {
		return 0, apperr.ErrNotFound
	}

	now := p.clock.Now()
	if err := database.WithoutTenant(ctx, p.pool, func(q *db.Queries) error {
		_, err := p.repo.upsertOrganization(ctx, q, identity.OrganizationID, identity.OrganizationName)
		return err
	}); err != nil {
		return 0, err
	}
	err = database.WithTenant(ctx, p.pool, organizationID, func(q *db.Queries) error {
		// Not the person's own activity: their billing clock stays where it is.
		row, err := p.repo.upsertPersonnel(ctx, q, identity, now, false)
		id = row.ID
		return err
	})
	return id, err
}
