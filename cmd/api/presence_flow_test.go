package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// member returns the entry of one person in the members of a team detail.
func (f *flow) member(viewerToken, team string, personnelID int64) (online bool, lastSeen *time.Time) {
	f.t.Helper()
	code, raw := f.do(http.MethodGet, "/api/v1/teams/"+team+"/members", viewerToken, nil, nil)
	require.Equal(f.t, http.StatusOK, code, string(raw))
	var env struct {
		Data struct {
			Members []struct {
				PersonnelID int64      `json:"personnel_id"`
				Online      bool       `json:"online"`
				LastSeenAt  *time.Time `json:"last_seen_at"`
			} `json:"members"`
		} `json:"data"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	for _, m := range env.Data.Members {
		if m.PersonnelID == personnelID {
			return m.Online, m.LastSeenAt
		}
	}
	f.t.Fatalf("person %d is not in the members of %s: %s", personnelID, team, raw)
	return false, nil
}

func TestPresenceInTheTeamDetail(t *testing.T) {
	f := newFlow(t)
	viewer := f.addPerson(900103, "VIEWER0001", "Anggota Dua")
	f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ('1K-IBMI-S1', $1, 'MEMBER', current_date - 30)", viewer)
	viewerTok := f.token(viewer, 5002)

	t.Run("a person who never did anything is offline and has no last seen", func(t *testing.T) {
		online, lastSeen := f.member(viewerTok, "1K-IBMI-S1", f.person)
		require.False(t, online)
		require.Nil(t, lastSeen)
	})

	t.Run("after a request the person is online, and last_seen_at is persisted", func(t *testing.T) {
		code, raw := f.do(http.MethodGet, "/api/v1/auth/me", f.personTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))

		online, lastSeen := f.member(viewerTok, "1K-IBMI-S1", f.person)
		require.True(t, online)
		require.NotNil(t, lastSeen)
		require.WithinDuration(t, time.Now(), *lastSeen, time.Minute)

		var stored *time.Time
		f.query([]any{&stored}, "SELECT last_seen_at FROM personnels WHERE id = $1", f.person)
		require.NotNil(t, stored, "persisted")
		require.WithinDuration(t, time.Now(), *stored, time.Minute)
	})

	t.Run("after logout the person is offline, keeping the last seen", func(t *testing.T) {
		code, raw := f.do(http.MethodPost, "/api/v1/auth/logout", f.personTok, nil, nil)
		require.Less(t, code, 300, string(raw))

		online, lastSeen := f.member(viewerTok, "1K-IBMI-S1", f.person)
		require.False(t, online)
		require.NotNil(t, lastSeen, "Terakhir online survives the logout")
	})
}
