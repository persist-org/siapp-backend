package notifications_test

import (
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/notifications"
	"siapp/pkg/platform/testkit"
)

// seed writes n notifications for one recipient, a second apart, and returns their ids oldest
// first (the order of creation).
func (f *flow) seed(org, actor, recipient int64, token string, n int) []int64 {
	f.t.Helper()
	before := f.list(token, "limit=100").ids()
	for range n {
		f.clock.Advance(time.Second)
		f.notify(org, actor, notifications.Input{
			Type: notifications.TypeInstructionReceived, RecipientIDs: []int64{recipient},
			Vars: notifications.Vars{ActorName: "Budi", Subject: "Ganti shift"},
		})
	}
	all := f.list(token, "limit=100").ids() // newest first
	require.Len(f.t, all, len(before)+n)
	out := make([]int64, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, all[i])
	}
	return out
}

func TestListIsNewestFirstAndPaged(t *testing.T) {
	f := newFlow(t)
	token, me := f.person(f.orgA)
	_, actor := f.person(f.orgA)
	ids := f.seed(f.orgA, actor, me, token, 5) // oldest first

	t.Run("newest first, with the pagination block and the watermark", func(t *testing.T) {
		got := f.list(token, "")
		require.Equal(t, []int64{ids[4], ids[3], ids[2], ids[1], ids[0]}, got.ids())
		require.NotNil(t, got.Pagination)
		require.Equal(t, 1, got.Pagination.Page)
		require.Equal(t, 20, got.Pagination.Limit)
		require.Equal(t, 5, got.Pagination.Total)
		require.Equal(t, 1, got.Pagination.TotalPages)
		require.True(t, got.Meta.ServerTime.Equal(f.clock.Now()), "server_time is now: %s", got.Meta.ServerTime)
		require.Nil(t, got.Deleted, "the plain list has no tombstones")
		require.Nil(t, got.Meta.HasMore)
		require.Empty(t, got.Meta.NextCursor)
	})

	t.Run("pages", func(t *testing.T) {
		page2 := f.list(token, "limit=2&page=2")
		require.Equal(t, []int64{ids[2], ids[1]}, page2.ids())
		require.Equal(t, 5, page2.Pagination.Total)
		require.Equal(t, 3, page2.Pagination.TotalPages)

		require.Equal(t, []int64{ids[0]}, f.list(token, "limit=2&page=3").ids())

		beyond := f.list(token, "limit=2&page=4")
		require.NotNil(t, beyond.Data, "an empty page is [], not null")
		require.Empty(t, beyond.Data)
		require.Equal(t, 5, beyond.Pagination.Total)
	})

	t.Run("unread_only leaves out what was read, and the total follows the filter", func(t *testing.T) {
		status, _ := f.read(token, ids[3])
		require.Equal(t, http.StatusOK, status)

		unread := f.list(token, "unread_only=true")
		require.Equal(t, []int64{ids[4], ids[2], ids[1], ids[0]}, unread.ids())
		require.Equal(t, 4, unread.Pagination.Total)
		require.Len(t, f.list(token, "unread_only=false").Data, 5)
	})
}

func TestCounterAndMarkRead(t *testing.T) {
	f := newFlow(t)
	token, me := f.person(f.orgA)
	_, actor := f.person(f.orgA)
	ids := f.seed(f.orgA, actor, me, token, 3)

	require.Equal(t, count{Unread: 3, Total: 3}, f.counter(token))

	f.clock.Advance(time.Minute)
	readAt := f.clock.Now()
	status, first := f.read(token, ids[0])
	require.Equal(t, http.StatusOK, status)
	require.True(t, first.Data.IsRead)
	require.True(t, readAt.Equal(*first.Data.ReadAt), "read_at is the time of the call")
	require.True(t, readAt.Equal(first.Data.UpdatedAt), "reading is a change for the delta sync")
	require.Equal(t, count{Unread: 2, Total: 3}, f.counter(token))

	t.Run("marking read again changes nothing", func(t *testing.T) {
		f.clock.Advance(time.Hour)
		status, again := f.read(token, ids[0])
		require.Equal(t, http.StatusOK, status)
		require.True(t, again.Data.IsRead)
		require.True(t, readAt.Equal(*again.Data.ReadAt), "the first read_at is kept")
		require.True(t, readAt.Equal(again.Data.UpdatedAt), "a repeat is not a change: updated_at stays")
		require.Equal(t, count{Unread: 2, Total: 3}, f.counter(token))
	})

	t.Run("get shows the read state", func(t *testing.T) {
		status, got := f.get(token, ids[0])
		require.Equal(t, http.StatusOK, status)
		require.True(t, got.Data.IsRead)
		status, other := f.get(token, ids[1])
		require.Equal(t, http.StatusOK, status)
		require.False(t, other.Data.IsRead)
	})

	t.Run("read-all zeroes the counter and keeps earlier read times", func(t *testing.T) {
		f.clock.Advance(time.Hour)
		require.Equal(t, count{Unread: 0, Total: 3}, f.readAll(token))
		require.Equal(t, count{Unread: 0, Total: 3}, f.readAll(token), "and again")

		_, kept := f.get(token, ids[0])
		require.True(t, readAt.Equal(*kept.Data.ReadAt), "an already read notification is not read again")
		_, swept := f.get(token, ids[2])
		require.True(t, swept.Data.IsRead)
		require.True(t, f.clock.Now().Equal(*swept.Data.ReadAt))
	})
}

func TestOnlyTheRecipientSeesTheirNotifications(t *testing.T) {
	f := newFlow(t)
	tokenA, a := f.person(f.orgA)
	tokenB, _ := f.person(f.orgA)
	_, actor := f.person(f.orgA)
	foreignToken, _ := f.person(f.orgB)
	id := f.seed(f.orgA, actor, a, tokenA, 1)[0]

	missing := func(t *testing.T, status int, e envelope[view]) {
		t.Helper()
		require.Equal(t, http.StatusNotFound, status)
		require.Equal(t, "NOT_FOUND", e.Error.Code)
		require.Equal(t, notifications.ReasonNotFound, e.reason())
	}

	t.Run("another person of the same organization", func(t *testing.T) {
		status, e := f.get(tokenB, id)
		missing(t, status, e)
		status, e = f.read(tokenB, id)
		missing(t, status, e)

		require.Empty(t, f.list(tokenB, "").Data)
		require.Equal(t, count{}, f.counter(tokenB))
		require.Equal(t, count{}, f.readAll(tokenB), "read-all marks only the caller's own")

		_, mine := f.get(tokenA, id)
		require.False(t, mine.Data.IsRead, "nothing b did touched a's notification")
		require.Equal(t, count{Unread: 1, Total: 1}, f.counter(tokenA))
	})

	t.Run("a person of another organization", func(t *testing.T) {
		status, e := f.get(foreignToken, id)
		missing(t, status, e)
		status, e = f.read(foreignToken, id)
		missing(t, status, e)
		require.Empty(t, f.list(foreignToken, "").Data)

		// Even a token that claims a's personnel id, but of the other organization.
		impostor := testkit.Token(t, f.orgB, a)
		status, e = f.get(impostor, id)
		missing(t, status, e)
		require.Equal(t, count{}, f.readAll(impostor))

		_, mine := f.get(tokenA, id)
		require.False(t, mine.Data.IsRead)
	})

	t.Run("an id that does not exist answers like one that is not yours", func(t *testing.T) {
		_, theirs := f.get(tokenB, id)
		status, nothing := f.get(tokenB, id+1000)
		missing(t, status, nothing)
		require.Equal(t, theirs.Error.Message, nothing.Error.Message, "an id cannot be probed")
	})
}

func TestIdsThatCannotExistAreNotFound(t *testing.T) {
	f := newFlow(t)
	token, _ := f.person(f.orgA)

	for _, id := range []string{"abc", "0", "-3", "99999999999999999999", "1.5"} {
		for _, call := range []struct{ method, path string }{
			{http.MethodGet, "/notifications/" + id},
			{http.MethodPost, "/notifications/" + id + "/read"},
		} {
			status, raw := f.raw(call.method, call.path, token)
			require.Equal(t, http.StatusNotFound, status, "%s %s", call.method, call.path)
			e := decode[any](t, status, raw)
			require.Equal(t, notifications.ReasonNotFound, e.reason())
		}
	}
}

func TestEveryInboxRouteNeedsAToken(t *testing.T) {
	f := newFlow(t)
	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/notifications"},
		{http.MethodGet, "/notifications/count"},
		{http.MethodPost, "/notifications/read-all"},
		{http.MethodGet, "/notifications/1"},
		{http.MethodPost, "/notifications/1/read"},
	} {
		status, raw := f.raw(call.method, call.path, "")
		require.Equal(t, http.StatusUnauthorized, status, "%s %s", call.method, call.path)
		require.Equal(t, "UNAUTHENTICATED", decode[any](t, status, raw).Error.Code)
	}
}

func TestListQueryIsValidated(t *testing.T) {
	f := newFlow(t)
	token, _ := f.person(f.orgA)
	goodCursor := notifications.Cursor{UpdatedAt: time.Unix(1_700_000_000, 0), ID: 7}.Encode()
	notAPosition := base64.RawURLEncoding.EncodeToString([]byte("not json"))

	rejected := []struct {
		name, query string
		fields      []string
	}{
		{"since that is not a time", "since=yesterday", []string{"since"}},
		{"since that is only a date", "since=2026-06-01", []string{"since"}},
		{"cursor that is not base64", "cursor=%25%25%25", []string{"cursor"}},
		{"cursor that does not decode to a position", "cursor=" + notAPosition, []string{"cursor"}},
		{"cursor that is too long", "cursor=" + repeat("A", 200), []string{"cursor"}},
		{"unread_only that is not a boolean", "unread_only=maybe", []string{"unread_only"}},
		{"unread_only with since", "unread_only=true&since=2026-06-01T00:00:00Z", []string{"unread_only"}},
		{"unread_only with cursor", "unread_only=true&cursor=" + goodCursor, []string{"unread_only"}},
		{"limit zero", "limit=0", []string{"limit"}},
		{"limit above 100", "limit=101", []string{"limit"}},
		{"page zero", "page=0", []string{"page"}},
		{"every problem at once", "limit=500&since=x&unread_only=y", []string{"limit", "since", "unread_only"}},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := f.raw(http.MethodGet, "/notifications?"+tc.query, token)
			require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
			e := decode[any](t, status, raw)
			require.Equal(t, "VALIDATION_ERROR", e.Error.Code)
			require.ElementsMatch(t, tc.fields, e.fields())
		})
	}

	t.Run("a good cursor, a good since and the largest limit are accepted", func(t *testing.T) {
		require.Equal(t, http.StatusOK, mustStatus(f.raw(http.MethodGet, "/notifications?cursor="+goodCursor, token)))
		require.Equal(t, http.StatusOK, mustStatus(f.raw(http.MethodGet, "/notifications?since=2026-06-01T03:00:00Z&limit=100", token)))
		require.Equal(t, http.StatusOK, mustStatus(f.raw(http.MethodGet, "/notifications?since=0001-01-01T00:00:00Z", token)),
			"a year-one since is a pull from the start, not a 500")
		require.Equal(t, http.StatusOK, mustStatus(f.raw(http.MethodGet, "/notifications?since=2026-06-01T10:00:00%2B07:00", token)),
			"an offset other than Z is a time too")
	})
}

func mustStatus(status int, _ []byte) int { return status }

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
