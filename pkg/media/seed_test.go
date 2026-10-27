package media_test

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/media"
	"siapp/pkg/media/storagetest"
	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

// insert adds a photo row directly, with no file behind it. It is for the tests of Attach,
// which only look at rows. status is PENDING or UPLOADED.
func (f *flow) insert(org, owner int64, purpose, status string) int64 {
	f.t.Helper()
	var id int64
	require.NoError(f.t, database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), org, func(tx pgx.Tx) error {
		return tx.QueryRow(f.t.Context(), `INSERT INTO media (owner_personnel_id, purpose, content_type, size_bytes, storage_key, status, capture_source)
			VALUES ($1, $2::media_purpose, 'image/jpeg', 100, $3, $4::media_status, 'CAMERA') RETURNING id`,
			owner, purpose, storagetest.Key(f.t), status).Scan(&id)
	}))
	return id
}

// photo registers a real photo through the API, uploads its bytes to the driver and, when
// confirm is set, completes it. The file is really there afterwards.
func (f *flow) photo(token, purpose string, confirm bool) (id int64, key string) {
	f.t.Helper()
	const size = 900
	body := validBody(size)
	body["purpose"] = purpose
	status, reg := f.register(token, body)
	require.Equal(f.t, http.StatusCreated, status)
	uploaded, answer := storagetest.Upload(f.t, reg.Data.asUpload(), storagetest.JPEG(size), nil)
	require.Less(f.t, uploaded, 300, "upload refused: %s", answer)
	if confirm {
		status, done := f.complete(token, reg.Data.ID)
		require.Equal(f.t, http.StatusOK, status)
		require.Equal(f.t, "UPLOADED", done.Data.Status)
	}
	for _, r := range f.rowsAnyOrg() {
		if r.ID == reg.Data.ID {
			return r.ID, r.Key
		}
	}
	f.t.Fatalf("photo %d is in no organization", reg.Data.ID)
	return 0, ""
}

// rowsAnyOrg lists the photos of both test organizations.
func (f *flow) rowsAnyOrg() []row { return append(f.rows(f.orgA), f.rows(f.orgB)...) }

// age moves a photo's registration time into the past, so the cleanup job sees it as old.
func (f *flow) age(org, id int64, by time.Duration) {
	f.t.Helper()
	require.NoError(f.t, database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), org, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.t.Context(), "UPDATE media SET created_at = created_at - make_interval(secs => $2) WHERE id = $1", id, by.Seconds())
		return err
	}))
}

// attachedTo reads what a photo is attached to: its type and id, or "" and 0.
func (f *flow) attachedTo(org, id int64) (entityType string, entityID int64) {
	f.t.Helper()
	require.NoError(f.t, database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), org, func(tx pgx.Tx) error {
		var t *string
		var i *int64
		if err := tx.QueryRow(f.t.Context(), "SELECT attached_entity_type, attached_entity_id FROM media WHERE id = $1", id).Scan(&t, &i); err != nil {
			return err
		}
		if t != nil {
			entityType = *t
		}
		if i != nil {
			entityID = *i
		}
		return nil
	}))
	return entityType, entityID
}

// attach runs Attach in a tenant transaction of its own, as a module's create would, and
// returns the error the transaction ends with (so a failed Attach rolls back).
func (f *flow) attach(org int64, in media.AttachInput) error {
	f.t.Helper()
	return database.WithTenant(f.t.Context(), testkit.Pool(f.t), org, func(q *db.Queries) error {
		return f.service.Attach(f.t.Context(), q, in)
	})
}

// views runs Views in a tenant transaction.
func (f *flow) views(org int64, ids []int64) []media.View {
	f.t.Helper()
	var out []media.View
	require.NoError(f.t, database.WithTenant(f.t.Context(), testkit.Pool(f.t), org, func(q *db.Queries) (err error) {
		out, err = f.service.Views(f.t.Context(), q, ids)
		return err
	}))
	return out
}
