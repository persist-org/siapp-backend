package notifications_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/notifications"
)

// since is the query of a delta pull from t.
func since(t time.Time) string { return "since=" + t.Format(time.RFC3339) }

// pageThrough follows next_cursor from the first answer to the last, and returns every page. The
// follow-up requests carry the cursor only, as a client does.
func (f *flow) pageThrough(token string, first listBody, limit string) []listBody {
	f.t.Helper()
	pages := []listBody{first}
	for pages[len(pages)-1].Meta.HasMore != nil && *pages[len(pages)-1].Meta.HasMore {
		cursor := pages[len(pages)-1].Meta.NextCursor
		require.NotEmpty(f.t, cursor, "has_more without a next_cursor")
		pages = append(pages, f.list(token, "limit="+limit+"&cursor="+cursor))
		require.Less(f.t, len(pages), 50, "paging does not end")
	}
	return pages
}

func TestDeltaSyncReturnsWhatChanged(t *testing.T) {
	f := newFlow(t)
	start := f.clock.Now()
	token, me := f.person(f.orgA)
	_, actor := f.person(f.orgA)
	ids := f.seed(f.orgA, actor, me, token, 3)

	t.Run("a first pull gives the rows oldest change first, with no pagination and the watermark", func(t *testing.T) {
		got := f.list(token, since(start.Add(-time.Minute)))
		require.Equal(t, ids, got.ids())
		require.Nil(t, got.Pagination, "a delta response carries no pagination (DL-57)")
		require.NotNil(t, got.Deleted, "tombstones are [] when there are none, not null")
		require.Empty(t, got.Deleted)
		require.NotNil(t, got.Meta.HasMore)
		require.False(t, *got.Meta.HasMore)
		require.Empty(t, got.Meta.NextCursor, "next_cursor is left out on the last page")
		require.True(t, got.Meta.ServerTime.Equal(f.clock.Now()))
	})

	t.Run("the next pull starts at that watermark and finds only what changed after it", func(t *testing.T) {
		// Far enough on that the three rows are older than the overlap.
		f.clock.Advance(time.Minute)
		watermark := f.list(token, since(start.Add(-time.Minute))).Meta.ServerTime

		f.clock.Advance(time.Minute)
		status, _ := f.read(token, ids[1])
		require.Equal(t, 200, status)

		got := f.list(token, since(*watermark))
		require.Equal(t, []int64{ids[1]}, got.ids(), "only the row that was read since")
		require.True(t, got.Data[0].IsRead)
		require.True(t, f.clock.Now().Equal(got.Data[0].UpdatedAt))

		// The pull right after, from the new watermark, still sends that row: a change from
		// the last few seconds is sent again (see deltaOverlap), and the app upserts by id.
		next := f.list(token, since(*got.Meta.ServerTime))
		require.Equal(t, []int64{ids[1]}, next.ids())

		// Once the change is older than the overlap, nothing is sent.
		f.clock.Advance(time.Minute)
		require.Empty(t, f.list(token, since(f.clock.Now())).Data)
	})

	t.Run("a new notification shows up in the next pull", func(t *testing.T) {
		watermark := f.list(token, "").Meta.ServerTime
		f.clock.Advance(time.Minute)
		f.notify(f.orgA, actor, notifications.Input{Type: notifications.TypePanicTriggered, RecipientIDs: []int64{me}, Priority: notifications.PriorityHigh})

		got := f.list(token, since(*watermark))
		require.Len(t, got.Data, 1)
		require.Equal(t, "PANIC_TRIGGERED", got.Data[0].Type)
		require.Equal(t, "HIGH", got.Data[0].Priority)
	})
}

func TestDeltaSyncPagesWithACursor(t *testing.T) {
	f := newFlow(t)
	start := f.clock.Now()
	token, me := f.person(f.orgA)
	_, actor := f.person(f.orgA)
	ids := f.seed(f.orgA, actor, me, token, 5)

	pages := f.pageThrough(token, f.list(token, since(start.Add(-time.Minute))+"&limit=2"), "2")

	require.Len(t, pages, 3)
	require.Equal(t, []int64{ids[0], ids[1]}, pages[0].ids())
	require.Equal(t, []int64{ids[2], ids[3]}, pages[1].ids())
	require.Equal(t, []int64{ids[4]}, pages[2].ids())
	require.False(t, *pages[2].Meta.HasMore)
	require.Empty(t, pages[2].Meta.NextCursor)
	for _, page := range pages {
		require.Nil(t, page.Pagination)
	}

	t.Run("the cursor wins over since", func(t *testing.T) {
		// A since far in the future would find nothing; the cursor says where to continue.
		got := f.list(token, since(start.Add(24*time.Hour))+"&limit=2&cursor="+pages[0].Meta.NextCursor)
		require.Equal(t, []int64{ids[2], ids[3]}, got.ids())
	})
}

// ⚠ One Notify gives all its rows the same timestamp, and a person can get several in the same
// instant. The page boundary then falls between rows of one updated_at: the id has to break the
// tie, or a row would be sent twice or never.
func TestDeltaPagingIsExactWhenRowsShareATimestamp(t *testing.T) {
	f := newFlow(t)
	start := f.clock.Now()
	token, me := f.person(f.orgA)
	_, actor := f.person(f.orgA)
	for range 4 { // the clock does not move: four rows, one updated_at
		f.notify(f.orgA, actor, notifications.Input{Type: notifications.TypeSwapAssigned, RecipientIDs: []int64{me}})
	}
	want := f.list(token, "").ids() // newest first

	pages := f.pageThrough(token, f.list(token, since(start.Add(-time.Minute))+"&limit=1"), "1")

	var got []int64
	for _, page := range pages {
		got = append(got, page.ids()...)
	}
	require.Len(t, pages, 4)
	require.Equal(t, []int64{want[3], want[2], want[1], want[0]}, got, "every row once, in id order")
}

func TestDeltaSyncSendsTombstones(t *testing.T) {
	f := newFlow(t)
	start := f.clock.Now()
	token, me := f.person(f.orgA)
	_, actor := f.person(f.orgA)
	ids := f.seed(f.orgA, actor, me, token, 3)

	// Nothing in this phase deletes a notification; retention will. It must set deleted_at and
	// updated_at together, which is what this does.
	f.clock.Advance(time.Minute)
	deletedAt := f.clock.Now()
	f.exec(f.orgA, "UPDATE notifications SET deleted_at = $1, updated_at = $1 WHERE id = $2", deletedAt, ids[1])

	t.Run("the delta stream turns it into a tombstone, in its place in the order", func(t *testing.T) {
		got := f.list(token, since(start.Add(-time.Minute)))
		require.Equal(t, []int64{ids[0], ids[2]}, got.ids(), "a deleted row is not sent as a row")
		require.Len(t, got.Deleted, 1)
		require.Equal(t, ids[1], got.Deleted[0].ID)
		require.True(t, deletedAt.Equal(got.Deleted[0].DeletedAt))
	})

	t.Run("rows and tombstones share one cursor", func(t *testing.T) {
		pages := f.pageThrough(token, f.list(token, since(start.Add(-time.Minute))+"&limit=1"), "1")
		require.Len(t, pages, 3)
		require.Equal(t, []int64{ids[0]}, pages[0].ids())
		require.Equal(t, []int64{ids[2]}, pages[1].ids())
		require.Empty(t, pages[2].Data)
		require.Equal(t, ids[1], pages[2].Deleted[0].ID)
	})

	t.Run("an old tombstone is not sent again", func(t *testing.T) {
		f.clock.Advance(time.Hour)
		got := f.list(token, since(f.clock.Now().Add(-time.Minute)))
		require.Empty(t, got.Data)
		require.Empty(t, got.Deleted)
	})

	t.Run("everywhere else the row is gone", func(t *testing.T) {
		require.Equal(t, []int64{ids[2], ids[0]}, f.list(token, "").ids())
		require.Equal(t, count{Unread: 2, Total: 2}, f.counter(token))
		status, e := f.get(token, ids[1])
		require.Equal(t, 404, status)
		require.Equal(t, notifications.ReasonNotFound, e.reason())
		status, _ = f.read(token, ids[1])
		require.Equal(t, 404, status)
		require.Equal(t, count{Unread: 0, Total: 2}, f.readAll(token))
	})
}

// ⚠ A row's updated_at is when its statement ran; it becomes visible when its transaction
// commits, a little later. A pull in between gets a watermark newer than the row, and without
// the overlap would never ask for it again.
func TestDeltaSyncLooksALittleBeforeSince(t *testing.T) {
	f := newFlow(t)
	token, me := f.person(f.orgA)
	_, actor := f.person(f.orgA)
	ids := f.seed(f.orgA, actor, me, token, 2)

	watermark := f.clock.Advance(time.Hour)
	// Two rows that "committed after the pull", one a moment before the watermark, one long
	// before it.
	f.exec(f.orgA, "UPDATE notifications SET updated_at = $1 WHERE id = $2", watermark.Add(-3*time.Second), ids[0])
	f.exec(f.orgA, "UPDATE notifications SET updated_at = $1 WHERE id = $2", watermark.Add(-10*time.Second), ids[1])

	got := f.list(token, since(watermark))

	require.Equal(t, []int64{ids[0]}, got.ids(), "3 s before the watermark is inside the overlap, 10 s is outside")
}

func TestDeltaSyncShowsOnlyTheCallersRows(t *testing.T) {
	f := newFlow(t)
	start := f.clock.Now()
	token, me := f.person(f.orgA)
	otherToken, other := f.person(f.orgA)
	foreignToken, foreign := f.person(f.orgB)
	_, actor := f.person(f.orgA)
	_, foreignActor := f.person(f.orgB)
	mine := f.seed(f.orgA, actor, me, token, 2)
	theirs := f.seed(f.orgA, actor, other, otherToken, 2)
	foreignRows := f.seed(f.orgB, foreignActor, foreign, foreignToken, 2)

	query := since(start.Add(-time.Minute))
	require.Equal(t, mine, f.list(token, query).ids())
	require.Equal(t, theirs, f.list(otherToken, query).ids())
	require.Equal(t, foreignRows, f.list(foreignToken, query).ids())
}
