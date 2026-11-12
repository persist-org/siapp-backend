package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type photoOut struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}

type scanFull struct {
	ID            int64       `json:"id"`
	ZoneID        int64       `json:"zone_id"`
	PersonnelID   int64       `json:"personnel_id"`
	PersonnelName string      `json:"personnel_name"`
	TeamCode      string      `json:"team_code"`
	Status        string      `json:"status"`
	DecidedByID   *int64      `json:"decided_by_id"`
	DecisionNote  *string     `json:"decision_note"`
	Media         *[]photoOut `json:"media"`
	Answers       []struct {
		ItemID int64       `json:"item_id"`
		Media  *[]photoOut `json:"media"`
	} `json:"answers"`
}

type progressOut struct {
	Zone struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		MinVisit int    `json:"min_visit"`
	} `json:"zone"`
	Required  int `json:"required"`
	Submitted int `json:"submitted"`
	Approved  int `json:"approved"`
	Declined  int `json:"declined"`
}

type scannerOut struct {
	PersonnelID    int64  `json:"personnel_id"`
	FullName       string `json:"full_name"`
	SubmittedCount int    `json:"submitted_count"`
	ApprovedCount  int    `json:"approved_count"`
}

// addMember adds a person to a regu and returns their id and token.
func (f *flow) addMember(team, role string, userID int64, nik, name string) (int64, string) {
	f.t.Helper()
	id := f.addPerson(userID, nik, name)
	f.exec("INSERT INTO team_members (team_code, personnel_id, role, start_date) VALUES ($1, $2, $3, current_date - 30)", team, id, role)
	return id, f.token(id, f.addSession(id, "tok-"+nik))
}

func (f *flow) getScans(token, path string) (int, []scanFull, []byte) {
	f.t.Helper()
	code, raw := f.do(http.MethodGet, path, token, nil, nil)
	var env struct {
		Data []scanFull `json:"data"`
	}
	if code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func (f *flow) getScan(token string, id int64) (int, scanFull, []byte) {
	f.t.Helper()
	code, raw := f.do(http.MethodGet, "/api/v1/scans/"+strconv.FormatInt(id, 10), token, nil, nil)
	var env struct {
		Data scanFull `json:"data"`
	}
	if code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func scanURL(id int64, action string) string {
	return "/api/v1/scans/" + strconv.FormatInt(id, 10) + "/" + action
}

func (f *flow) approve(token string, id int64) (int, []byte) {
	f.t.Helper()
	return f.do(http.MethodPost, scanURL(id, "approve"), token, nil, nil)
}

func (f *flow) decline(token string, id int64, note string) (int, []byte) {
	f.t.Helper()
	return f.do(http.MethodPost, scanURL(id, "decline"), token, jsonBody(f.t, map[string]any{"note": note}), nil)
}

type bulkOut struct {
	Approved []scanFull `json:"approved"`
	Skipped  []int64    `json:"skipped"`
}

func (f *flow) approveMany(token string, ids ...int64) (int, bulkOut, []byte) {
	f.t.Helper()
	code, raw := f.do(http.MethodPost, "/api/v1/scans/approve", token, jsonBody(f.t, map[string]any{"ids": ids}), nil)
	var env struct {
		Data bulkOut `json:"data"`
	}
	if code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func (f *flow) progress(token, query string) (int, []progressOut, []byte) {
	f.t.Helper()
	code, raw := f.do(http.MethodGet, "/api/v1/zones/progress"+query, token, nil, nil)
	var env struct {
		Data []progressOut `json:"data"`
	}
	if code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(raw, &env), string(raw))
	}
	return code, env.Data, raw
}

func progressOf(t *testing.T, list []progressOut, zoneID int64) progressOut {
	t.Helper()
	for _, p := range list {
		if p.Zone.ID == zoneID {
			return p
		}
	}
	t.Fatalf("zone %d is not in the progress list", zoneID)
	return progressOut{}
}

func TestPatrolProgressCounters(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedZone(nil, map[string]any{"min_visit": 3})
	quiet := f.fencedZone(nil, map[string]any{"type": "GUARD"})

	t.Run("a zone nobody scanned still shows, with its red number", func(t *testing.T) {
		code, list, raw := f.progress(f.personTok, "")
		require.Equal(t, http.StatusOK, code, string(raw))
		p := progressOf(t, list, z.ID)
		require.Equal(t, 3, p.Required)
		require.Zero(t, p.Submitted+p.Approved+p.Declined)
		require.Equal(t, "PATROL", p.Zone.Type)
		require.Equal(t, 1, progressOf(t, list, quiet.ID).Required)
	})

	a := f.mustScan(f.personTok, z, scanBody(z))
	b := f.mustScan(f.personTok, z, scanBody(z))
	c := f.mustScan(f.personTok, z, scanBody(z))

	t.Run("the blue circle counts what was submitted", func(t *testing.T) {
		_, list, _ := f.progress(f.personTok, "")
		p := progressOf(t, list, z.ID)
		require.Equal(t, 3, p.Submitted)
		require.Zero(t, p.Approved)
	})

	t.Run("an approve turns one green, a decline is counted apart", func(t *testing.T) {
		code, raw := f.approve(danruTok, a.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		code, raw = f.decline(danruTok, b.ID, "foto kurang jelas")
		require.Equal(t, http.StatusOK, code, string(raw))
		for _, tok := range []string{f.personTok, danruTok} {
			_, list, _ := f.progress(tok, "")
			p := progressOf(t, list, z.ID)
			require.Equal(t, 1, p.Submitted)
			require.Equal(t, 1, p.Approved)
			require.Equal(t, 1, p.Declined)
		}
		_ = c
	})

	t.Run("the filters pick the type, the project and the day", func(t *testing.T) {
		_, list, _ := f.progress(f.personTok, "?type=GUARD")
		require.Len(t, list, 1)
		require.Equal(t, quiet.ID, list[0].Zone.ID)

		_, list, _ = f.progress(f.personTok, "?project_code=1K-IBMI-S&type=PATROL")
		require.Len(t, list, 1)

		_, list, _ = f.progress(f.personTok, "?work_date=2020-01-01&type=PATROL")
		require.Zero(t, progressOf(t, list, z.ID).Submitted+progressOf(t, list, z.ID).Approved, "another day has no scans")

		today := time.Now().In(jakarta(t)).Format(time.DateOnly)
		_, list, _ = f.progress(f.personTok, "?work_date="+today+"&type=PATROL")
		require.Equal(t, 1, progressOf(t, list, z.ID).Approved)

		code, _, raw := f.progress(f.personTok, "?work_date=today")
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		code, _, raw = f.progress(f.personTok, "?type=BOGUS")
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("a project the caller does not serve in is refused", func(t *testing.T) {
		code, _, raw := f.progress(f.personTok, "?project_code=1K-IBMI-C")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
	})
}

func jakarta(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Jakarta")
	require.NoError(t, err)
	return loc
}

func TestPatrolProgressIsPerRegu(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	_, s2Tok := f.addMember("1K-IBMI-S2", "MEMBER", 900510, "REGUDUA001", "Anggota S2")
	z := f.fencedZone(nil, nil) // for the whole project

	f.mustScan(f.personTok, z, scanBody(z))
	f.mustScan(s2Tok, z, scanBody(z))
	f.mustScan(s2Tok, z, scanBody(z))

	t.Run("a member counts their own regu, not the other", func(t *testing.T) {
		_, list, _ := f.progress(f.personTok, "")
		require.Equal(t, 1, progressOf(t, list, z.ID).Submitted)
		_, list, _ = f.progress(s2Tok, "")
		require.Equal(t, 2, progressOf(t, list, z.ID).Submitted)
	})

	t.Run("a DANRU counts their regu, and cannot pick one outside their reach", func(t *testing.T) {
		_, list, _ := f.progress(danruTok, "")
		require.Equal(t, 1, progressOf(t, list, z.ID).Submitted)
		code, list, raw := f.progress(danruTok, "?team_code=1K-IBMI-S1")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, 1, progressOf(t, list, z.ID).Submitted)
		code, _, raw = f.progress(danruTok, "?team_code=1K-IBMI-S2")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, _, raw = f.progress(f.personTok, "?team_code=1K-IBMI-S2")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
	})

	t.Run("a DANRU whose permission is wider than their own regu can pick the others", func(t *testing.T) {
		f.exec("UPDATE role_permissions SET scope = 'PROJECT' WHERE permission = 'patrol.approve' AND team_role = 'DANRU'")
		_, list, _ := f.progress(danruTok, "?team_code=1K-IBMI-S2")
		require.Equal(t, 2, progressOf(t, list, z.ID).Submitted)
		_, list, _ = f.progress(danruTok, "")
		require.Equal(t, 3, progressOf(t, list, z.ID).Submitted, "without a pick, every regu in reach")
	})
}

func TestPatrolScannersList(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	other, otherTok := f.addMember("1K-IBMI-S1", "MEMBER", 900520, "KOLEGA0005", "Kolega Lima")
	z := f.fencedZone(nil, nil)
	path := zoneURL(z.ID, "/scanners")

	mine1 := f.mustScan(f.personTok, z, scanBody(z))
	f.mustScan(f.personTok, z, scanBody(z))
	f.mustScan(f.personTok, z, scanBody(z))
	f.mustScan(otherTok, z, scanBody(z))
	code, raw := f.approve(danruTok, mine1.ID)
	require.Equal(t, http.StatusOK, code, string(raw))

	get := func(token, query string) (int, []scannerOut, []byte) {
		code, raw := f.do(http.MethodGet, path+query, token, nil, nil)
		var env struct {
			Data []scannerOut `json:"data"`
		}
		if code == http.StatusOK {
			require.NoError(t, json.Unmarshal(raw, &env), string(raw))
		}
		return code, env.Data, raw
	}

	t.Run("the DANRU sees everyone of the regu with the number next to each name", func(t *testing.T) {
		code, list, raw := get(danruTok, "")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, list, 2)
		byID := map[int64]scannerOut{}
		for _, s := range list {
			byID[s.PersonnelID] = s
		}
		require.Equal(t, scannerOut{f.person, "Anggota Satu A", 2, 1}, byID[f.person])
		require.Equal(t, scannerOut{other, "Kolega Lima", 1, 0}, byID[other])
	})

	t.Run("a member sees only themselves", func(t *testing.T) {
		_, list, _ := get(otherTok, "")
		require.Len(t, list, 1)
		require.Equal(t, other, list[0].PersonnelID)
	})

	t.Run("another day is empty, the regu can be picked, and a regu outside the reach is refused", func(t *testing.T) {
		_, list, _ := get(danruTok, "?work_date=2020-01-01")
		require.Empty(t, list)
		_, list, _ = get(danruTok, "?team_code=1K-IBMI-S1")
		require.Len(t, list, 2)
		code, _, raw := get(danruTok, "?team_code=1K-IBMI-S2")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, _, raw = get(danruTok, "?work_date=nope")
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("a stranger and an unknown zone are refused", func(t *testing.T) {
		stranger := f.addPerson(900521, "ORANGLAIN4", "Orang Lain Empat")
		tok := f.token(stranger, f.addSession(stranger, "tok-stranger4"))
		code, _, raw := get(tok, "")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, raw = f.do(http.MethodGet, zoneURL(99999999, "/scanners"), danruTok, nil, nil)
		reasonIs(t, code, raw, http.StatusNotFound, "ZONE_NOT_FOUND")
	})
}

func TestPatrolDecisions(t *testing.T) {
	f := newFlow(t)
	danruID, danruTok := f.danru()
	z := f.fencedZone(nil, nil)
	scan := f.mustScan(f.personTok, z, scanBody(z))

	t.Run("a member cannot decide, not even their own scan", func(t *testing.T) {
		code, raw := f.approve(f.personTok, scan.ID)
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, raw = f.decline(f.personTok, scan.ID, "x")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, raw = f.do(http.MethodPost, "/api/v1/scans/approve", f.personTok, jsonBody(t, map[string]any{"ids": []int64{scan.ID}}), nil)
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
	})

	t.Run("a DANRU of another regu cannot decide, and cannot even read it", func(t *testing.T) {
		_, s2Danru := f.addMember("1K-IBMI-S2", "DANRU", 900530, "DANRUDUA01", "Danru Dua")
		code, raw := f.approve(s2Danru, scan.ID)
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, raw = f.decline(s2Danru, scan.ID, "x")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, _, raw2 := f.getScan(s2Danru, scan.ID)
		reasonIs(t, code, raw2, http.StatusNotFound, "SCAN_NOT_FOUND")
		_, status := "", ""
		f.query([]any{&status}, "SELECT status::text FROM patrol_scans WHERE id = $1", scan.ID)
		require.Equal(t, "SUBMITTED", status, "nothing changed")
	})

	t.Run("the permission of the zone's type is what counts", func(t *testing.T) {
		f.exec("UPDATE role_permissions SET scope = 'NONE' WHERE permission = 'patrol.approve'")
		code, raw := f.approve(danruTok, scan.ID)
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		f.exec("UPDATE role_permissions SET scope = 'TEAM' WHERE permission = 'patrol.approve' AND team_role = 'DANRU'")
	})

	t.Run("a decline needs a note", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"note":"   "}`, `{"note":""}`} {
			code, raw := f.do(http.MethodPost, scanURL(scan.ID, "decline"), danruTok, []byte(body), nil)
			require.Equal(t, http.StatusUnprocessableEntity, code, body+": "+string(raw))
		}
		code, raw := f.do(http.MethodPost, scanURL(scan.ID, "decline"), danruTok, jsonBody(t, map[string]any{"note": string(make([]byte, 1001))}), nil)
		require.NotEqual(t, http.StatusOK, code, string(raw))
	})

	t.Run("an approve records who, when, and the author is told", func(t *testing.T) {
		code, raw := f.approve(danruTok, scan.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		_, got, _ := f.getScan(danruTok, scan.ID)
		require.Equal(t, "APPROVED", got.Status)
		require.Equal(t, danruID, *got.DecidedByID)
		require.Equal(t, 1, f.countInbox(f.personTok, "PATROL_APPROVED"))
		var titled string
		for _, it := range f.inbox(f.personTok) {
			if it.Type == "PATROL_APPROVED" {
				titled = it.Title
			}
		}
		require.Contains(t, titled, "disetujui")
	})

	t.Run("a scan that was decided cannot be decided again, either way", func(t *testing.T) {
		code, raw := f.approve(danruTok, scan.ID)
		reasonIs(t, code, raw, http.StatusConflict, "SCAN_ALREADY_DECIDED")
		code, raw = f.decline(danruTok, scan.ID, "terlambat")
		reasonIs(t, code, raw, http.StatusConflict, "SCAN_ALREADY_DECIDED")
		_, got, _ := f.getScan(danruTok, scan.ID)
		require.Equal(t, "APPROVED", got.Status)
		require.Equal(t, 1, f.countInbox(f.personTok, "PATROL_APPROVED"), "no second notification")
	})

	t.Run("a decline keeps the note, and the author is told with it", func(t *testing.T) {
		second := f.mustScan(f.personTok, z, scanBody(z))
		code, raw := f.decline(danruTok, second.ID, "  foto buram, ulangi  ")
		require.Equal(t, http.StatusOK, code, string(raw))
		_, got, _ := f.getScan(f.personTok, second.ID)
		require.Equal(t, "DECLINED", got.Status)
		require.Equal(t, "foto buram, ulangi", *got.DecisionNote, "the author reads the feedback on their own scan")

		var title, body string
		for _, it := range f.inbox(f.personTok) {
			if it.Type == "PATROL_APPROVED" && it.Title != "" && it.Body != "" && contains(it.Body, "ditolak") {
				title, body = it.Title, it.Body
			}
		}
		require.Contains(t, title, "ditolak")
		require.Contains(t, body, "foto buram, ulangi")
	})

	t.Run("a guard zone notifies with the guard type", func(t *testing.T) {
		g := f.fencedZone(nil, map[string]any{"type": "GUARD"})
		gs := f.mustScan(f.personTok, g, scanBody(g))
		code, raw := f.approve(danruTok, gs.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, 1, f.countInbox(f.personTok, "GUARD_APPROVED"))
	})

	t.Run("a scan that does not exist, or has a bad id, is 404", func(t *testing.T) {
		code, raw := f.approve(danruTok, 99999999)
		reasonIs(t, code, raw, http.StatusNotFound, "SCAN_NOT_FOUND")
		code, raw = f.do(http.MethodPost, "/api/v1/scans/abc/approve", danruTok, nil, nil)
		reasonIs(t, code, raw, http.StatusNotFound, "SCAN_NOT_FOUND")
	})

	t.Run("every decision is audited", func(t *testing.T) {
		require.Equal(t, 2, countOf(t, f, "SELECT count(*) FROM audit_logs WHERE action = 'patrol.scan.approve'"))
		require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM audit_logs WHERE action = 'patrol.scan.decline'"))
	})
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestPatrolBulkApprove(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	other, otherTok := f.addMember("1K-IBMI-S1", "MEMBER", 900540, "KOLEGA0006", "Kolega Enam")
	z := f.fencedZone(nil, nil)

	a1 := f.mustScan(f.personTok, z, scanBody(z))
	a2 := f.mustScan(f.personTok, z, scanBody(z))
	b1 := f.mustScan(otherTok, z, scanBody(z))

	t.Run("one request approves them all and each author gets one notification", func(t *testing.T) {
		code, res, raw := f.approveMany(danruTok, a1.ID, a2.ID, b1.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, res.Approved, 3)
		require.Empty(t, res.Skipped)
		for _, s := range res.Approved {
			require.Equal(t, "APPROVED", s.Status)
		}
		require.Equal(t, 1, f.countInbox(f.personTok, "PATROL_APPROVED"), "two scans of one author, one notification")
		require.Equal(t, 1, f.countInbox(otherTok, "PATROL_APPROVED"))
		_ = other
	})

	t.Run("scans that were decided are skipped and listed, the rest approved", func(t *testing.T) {
		c1 := f.mustScan(f.personTok, z, scanBody(z))
		c2 := f.mustScan(f.personTok, z, scanBody(z))
		code, raw := f.decline(danruTok, c1.ID, "salah zona")
		require.Equal(t, http.StatusOK, code, string(raw))
		before := f.countInbox(f.personTok, "PATROL_APPROVED")

		code, res, raw := f.approveMany(danruTok, a1.ID, c1.ID, c2.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, []int64{a1.ID, c1.ID}, res.Skipped)
		require.Len(t, res.Approved, 1)
		require.Equal(t, c2.ID, res.Approved[0].ID)
		require.Equal(t, before+1, f.countInbox(f.personTok, "PATROL_APPROVED"), "one notice, for the one that changed")
		_, got, _ := f.getScan(danruTok, c1.ID)
		require.Equal(t, "DECLINED", got.Status, "a declined scan is not turned into an approval")
	})

	t.Run("a request that is all decided is a 200 with everything skipped", func(t *testing.T) {
		code, res, raw := f.approveMany(danruTok, a1.ID, a2.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Empty(t, res.Approved)
		require.Equal(t, []int64{a1.ID, a2.ID}, res.Skipped)
	})

	t.Run("one scan outside the caller's reach, or missing, refuses the whole request", func(t *testing.T) {
		mine := f.mustScan(f.personTok, z, scanBody(z))
		_, s2Tok := f.addMember("1K-IBMI-S2", "MEMBER", 900541, "REGUDUA002", "Anggota S2 Dua")
		foreign := f.mustScan(s2Tok, z, scanBody(z))

		code, _, raw := f.approveMany(danruTok, mine.ID, foreign.ID)
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, _, raw = f.approveMany(danruTok, mine.ID, 99999999)
		reasonIs(t, code, raw, http.StatusNotFound, "SCAN_NOT_FOUND")
		_, got, _ := f.getScan(danruTok, mine.ID)
		require.Equal(t, "SUBMITTED", got.Status, "nothing was applied")
	})

	t.Run("the ids are checked", func(t *testing.T) {
		for name, body := range map[string]string{
			"empty":       `{"ids":[]}`,
			"missing":     `{}`,
			"not numbers": `{"ids":["a"]}`,
			"zero":        `{"ids":[0]}`,
		} {
			code, raw := f.do(http.MethodPost, "/api/v1/scans/approve", danruTok, []byte(body), nil)
			require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
		}
		ids := make([]int64, 51)
		for i := range ids {
			ids[i] = int64(i + 1)
		}
		code, raw := f.do(http.MethodPost, "/api/v1/scans/approve", danruTok, jsonBody(t, map[string]any{"ids": ids}), nil)
		require.Equal(t, http.StatusUnprocessableEntity, code, "more than 50: "+string(raw))
	})

	t.Run("a repeated id counts once", func(t *testing.T) {
		d := f.mustScan(f.personTok, z, scanBody(z))
		code, res, raw := f.approveMany(danruTok, d.ID, d.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, res.Approved, 1)
		require.Empty(t, res.Skipped)
	})
}

func TestPatrolDecisionRace(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	z := f.fencedZone(nil, nil)
	scan := f.mustScan(f.personTok, z, scanBody(z))

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i == 0 {
				codes[i], _ = f.approve(danruTok, scan.ID)
			} else {
				codes[i], _ = f.decline(danruTok, scan.ID, "bersamaan")
			}
		}()
	}
	wg.Wait()
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, codes, "exactly one of two simultaneous decisions wins")
	require.Equal(t, 1, countOf(t, f, "SELECT count(*) FROM audit_logs WHERE action IN ('patrol.scan.approve', 'patrol.scan.decline')"))
}

func TestPatrolScanViews(t *testing.T) {
	f := newFlow(t)
	danruID, danruTok := f.danru()
	_ = danruID
	z := f.fencedZone([]map[string]any{
		{"label": "Pagar utuh?", "answer_type": "BOOLEAN", "photo_mode": "OPTIONAL", "photo_max": 2},
		{"label": "Catatan", "answer_type": "TEXT", "required": false},
	}, nil)
	pagar := z.Checklist[0].ID

	general, answerA, answerB := f.photo(f.person), f.photo(f.person), f.photo(f.person)
	body := scanBody(z)
	body["media_ids"] = []int64{general}
	body["answers"] = []map[string]any{{"item_id": pagar, "value": false, "media_ids": []int64{answerA, answerB}}}
	scan := f.mustScan(f.personTok, z, body)
	plain := f.mustScan(f.personTok, z, func() map[string]any {
		b := scanBody(z)
		b["answers"] = []map[string]any{{"item_id": pagar, "value": true}}
		return b
	}())

	t.Run("a DANRU reads each answer with its own proof, and the general photos apart", func(t *testing.T) {
		code, got, raw := f.getScan(danruTok, scan.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, "Anggota Satu A", got.PersonnelName)
		require.NotNil(t, got.Media)
		require.Len(t, *got.Media, 1)
		require.Equal(t, general, (*got.Media)[0].ID)
		require.NotEmpty(t, (*got.Media)[0].URL)
		require.Equal(t, "image/jpeg", (*got.Media)[0].ContentType)

		require.Len(t, got.Answers, 2)
		require.Equal(t, pagar, got.Answers[0].ItemID)
		require.NotNil(t, got.Answers[0].Media)
		require.Len(t, *got.Answers[0].Media, 2)
		require.Equal(t, []int64{answerA, answerB}, []int64{(*got.Answers[0].Media)[0].ID, (*got.Answers[0].Media)[1].ID})
		require.Empty(t, *got.Answers[1].Media, "an unanswered question has no photos, and an empty list")
	})

	t.Run("the author reads their own scan, a colleague cannot", func(t *testing.T) {
		code, got, raw := f.getScan(f.personTok, scan.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, *got.Media, 1)

		_, colleagueTok := f.addMember("1K-IBMI-S1", "MEMBER", 900550, "KOLEGA0007", "Kolega Tujuh")
		code, _, raw = f.getScan(colleagueTok, scan.ID)
		reasonIs(t, code, raw, http.StatusNotFound, "SCAN_NOT_FOUND")
	})

	t.Run("another organization's scan is 404", func(t *testing.T) {
		foreign, _, err := f.tokens.NewAccessToken(900999, 2, 5999)
		require.NoError(t, err)
		code, _, raw := f.getScan(foreign, scan.ID)
		reasonIs(t, code, raw, http.StatusNotFound, "SCAN_NOT_FOUND")
	})

	t.Run("the zone's list: the DANRU sees the regu's scans, a member their own, newest first", func(t *testing.T) {
		_, colleagueTok := f.addMember("1K-IBMI-S1", "MEMBER", 900551, "KOLEGA0008", "Kolega Delapan")
		theirBody := scanBody(z)
		theirBody["answers"] = []map[string]any{{"item_id": pagar, "value": true}}
		theirs := f.mustScan(colleagueTok, z, theirBody)

		code, list, raw := f.getScans(danruTok, zoneURL(z.ID, "/scans"))
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, list, 3)
		require.Equal(t, theirs.ID, list[0].ID, "newest first")
		require.NotNil(t, list[0].Media, "the list carries the media views too")

		_, list, _ = f.getScans(f.personTok, zoneURL(z.ID, "/scans"))
		require.Len(t, list, 2)
		for _, s := range list {
			require.Equal(t, f.person, s.PersonnelID)
		}

		_, list, _ = f.getScans(danruTok, zoneURL(z.ID, "/scans")+"?personnel_id="+strconv.FormatInt(f.person, 10))
		require.Len(t, list, 2)
	})

	t.Run("approved scans stay in the list, and the filters pick them", func(t *testing.T) {
		code, raw := f.approve(danruTok, plain.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		_, list, _ := f.getScans(danruTok, zoneURL(z.ID, "/scans")+"?status=APPROVED")
		require.Len(t, list, 1)
		require.Equal(t, plain.ID, list[0].ID)
		_, list, _ = f.getScans(danruTok, zoneURL(z.ID, "/scans")+"?status=SUBMITTED")
		require.Len(t, list, 2)
		today := time.Now().In(jakarta(t)).Format(time.DateOnly)
		_, list, _ = f.getScans(danruTok, zoneURL(z.ID, "/scans")+"?work_date="+today)
		require.Len(t, list, 3)
		_, list, _ = f.getScans(danruTok, zoneURL(z.ID, "/scans")+"?work_date=2020-01-01")
		require.Empty(t, list)

		_, one, _ := f.getScan(f.personTok, plain.ID)
		require.Equal(t, "APPROVED", one.Status, "and the author still reads it")
	})

	t.Run("a page of the list", func(t *testing.T) {
		code, raw := f.do(http.MethodGet, zoneURL(z.ID, "/scans")+"?limit=2&page=2", danruTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		var env struct {
			Data       []scanFull `json:"data"`
			Pagination struct {
				Total int `json:"total"`
				Limit int `json:"limit"`
				Page  int `json:"page"`
			} `json:"pagination"`
		}
		require.NoError(t, json.Unmarshal(raw, &env))
		require.Len(t, env.Data, 1, "three scans, two per page: the second page holds the last one")
		require.Equal(t, 3, env.Pagination.Total)
		require.Equal(t, 2, env.Pagination.Page)
	})

	t.Run("a bad filter is a validation error, a stranger is refused, an unknown zone is 404", func(t *testing.T) {
		for _, q := range []string{"?status=DONE", "?personnel_id=x", "?work_date=x"} {
			code, _, raw := f.getScans(danruTok, zoneURL(z.ID, "/scans")+q)
			require.Equal(t, http.StatusUnprocessableEntity, code, q+": "+string(raw))
		}
		stranger := f.addPerson(900552, "ORANGLAIN5", "Orang Lain Lima")
		tok := f.token(stranger, f.addSession(stranger, "tok-stranger5"))
		code, _, raw := f.getScans(tok, zoneURL(z.ID, "/scans"))
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, _, raw = f.getScans(danruTok, zoneURL(99999999, "/scans"))
		reasonIs(t, code, raw, http.StatusNotFound, "ZONE_NOT_FOUND")
	})
}

func TestPatrolPendingApprovals(t *testing.T) {
	f := newFlow(t)
	_, danruTok := f.danru()
	_, s2Tok := f.addMember("1K-IBMI-S2", "MEMBER", 900560, "REGUDUA003", "Anggota S2 Tiga")
	z := f.fencedZone(nil, nil)
	g := f.fencedZone(nil, map[string]any{"type": "GUARD"})

	first := scanBody(z)
	first["occurred_at"] = time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339Nano)
	oldest := f.mustScan(f.personTok, z, first)
	newer := f.mustScan(f.personTok, z, scanBody(z))
	guardScan := f.mustScan(f.personTok, g, scanBody(g))
	f.mustScan(s2Tok, z, scanBody(z)) // another regu's: not in this DANRU's queue

	ids := func(list []scanFull) []int64 {
		var out []int64
		for _, s := range list {
			out = append(out, s.ID)
		}
		return out
	}

	t.Run("the DANRU's queue is their regu's submitted scans, oldest first", func(t *testing.T) {
		code, list, raw := f.getScans(danruTok, "/api/v1/patrol/pending-approvals")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, []int64{oldest.ID, newer.ID, guardScan.ID}, ids(list))
		require.Equal(t, "Anggota Satu A", list[0].PersonnelName)
	})

	t.Run("a decided scan leaves the queue", func(t *testing.T) {
		code, raw := f.approve(danruTok, oldest.ID)
		require.Equal(t, http.StatusOK, code, string(raw))
		code, raw = f.decline(danruTok, newer.ID, "ulangi")
		require.Equal(t, http.StatusOK, code, string(raw))
		_, list, _ := f.getScans(danruTok, "/api/v1/patrol/pending-approvals")
		require.Equal(t, []int64{guardScan.ID}, ids(list))
	})

	t.Run("a guard zone is judged by guard.approve", func(t *testing.T) {
		f.exec("UPDATE role_permissions SET scope = 'NONE' WHERE permission = 'guard.approve'")
		_, list, _ := f.getScans(danruTok, "/api/v1/patrol/pending-approvals")
		require.Empty(t, list, "patrol.approve alone does not reach a guard zone's scan")
		f.exec("UPDATE role_permissions SET scope = 'TEAM' WHERE permission = 'guard.approve' AND team_role = 'DANRU'")
		_, list, _ = f.getScans(danruTok, "/api/v1/patrol/pending-approvals")
		require.Len(t, list, 1)
	})

	t.Run("a member has no queue, and the regu filter stays inside the reach", func(t *testing.T) {
		code, _, raw := f.getScans(f.personTok, "/api/v1/patrol/pending-approvals")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, _, raw = f.getScans(danruTok, "/api/v1/patrol/pending-approvals?team_code=1K-IBMI-S2")
		reasonIs(t, code, raw, http.StatusForbidden, "PERMISSION_DENIED")
		code, list, raw := f.getScans(danruTok, "/api/v1/patrol/pending-approvals?team_code=1K-IBMI-S1")
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, list, 1)
	})
}
