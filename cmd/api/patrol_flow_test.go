package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

type optionOut struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type checklistOut struct {
	ID         int64       `json:"id"`
	SortOrder  int         `json:"sort_order"`
	Label      string      `json:"label"`
	HelpText   *string     `json:"help_text"`
	AnswerType string      `json:"answer_type"`
	Options    []optionOut `json:"options"`
	Required   bool        `json:"required"`
	PhotoMode  string      `json:"photo_mode"`
	PhotoMax   int         `json:"photo_max"`
	NoteMode   string      `json:"note_mode"`
	AttachWhen string      `json:"attach_when"`
}

type zoneOut struct {
	ID            int64    `json:"id"`
	ProjectCode   string   `json:"project_code"`
	TeamCode      *string  `json:"team_code"`
	Type          string   `json:"type"`
	Name          string   `json:"name"`
	Alias         *string  `json:"alias"`
	Description   *string  `json:"description"`
	PopupText     *string  `json:"popup_text"`
	Latitude      *float64 `json:"latitude"`
	Longitude     *float64 `json:"longitude"`
	RadiusMeters  *int     `json:"radius_meters"`
	MinVisit      int      `json:"min_visit"`
	StartDate     *string  `json:"start_date"`
	PhotoRequired bool     `json:"photo_required"`
	IsActive      bool     `json:"is_active"`
	// The checklist is gone from the zone (Phase 10.8): it is the checkpoint's. A list has only the
	// count; a detail has the checkpoints.
	CheckpointCount  int                 `json:"checkpoint_count"`
	Checkpoints      []zoneCheckpointOut `json:"checkpoints"`
	NextCheckpointID *string             `json:"next_checkpoint_id"`
}

func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

// zoneCall sends a request and decodes the zone of a 2xx answer.
func (f *flow) zoneCall(method, path, token string, body any) (int, zoneOut, []byte) {
	f.t.Helper()
	var raw []byte
	if body != nil {
		raw = jsonBody(f.t, body)
	}
	code, resp := f.do(method, path, token, raw, nil)
	var env struct {
		Data zoneOut `json:"data"`
	}
	if code < 300 {
		require.NoError(f.t, json.Unmarshal(resp, &env), string(resp))
	}
	return code, env.Data, resp
}

// zoneList decodes a list answer.
func (f *flow) zoneList(path, token string) (int, []zoneOut, []byte) {
	f.t.Helper()
	code, resp := f.do(http.MethodGet, path, token, nil, nil)
	var env struct {
		Data []zoneOut `json:"data"`
	}
	if code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(resp, &env), string(resp))
	}
	return code, env.Data, resp
}

// problem reads the reason and the bad fields of an error answer.
func problem(t *testing.T, raw []byte) (reason string, fields []string) {
	t.Helper()
	var env struct {
		Error struct {
			Details map[string]any `json:"details"`
			Errors  []struct {
				Field string `json:"field"`
			} `json:"errors"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), string(raw))
	reason, _ = env.Error.Details["reason"].(string)
	for _, e := range env.Error.Errors {
		fields = append(fields, e.Field)
	}
	return reason, fields
}

func zonePath(id int64) string { return "/api/v1/admin/zones/" + strconv.FormatInt(id, 10) }

func TestPatrolZoneAdminCRUD(t *testing.T) {
	f := newFlow(t)

	var id int64
	t.Run("a zone is saved and returned as sent", func(t *testing.T) {
		code, z, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
			"project_code": "1K-IBMI-S", "type": "PATROL", "name": "  Pos Gerbang  ", "alias": "Gerbang", "description": "Gerbang depan",
			"popup_text": "Hati-hati anjing", "latitude": -6.2, "longitude": 106.8, "radius_meters": 75, "min_visit": 3,
			"start_date": "2026-01-15",
		})
		require.Equal(t, http.StatusCreated, code, string(raw))
		id = z.ID
		require.Equal(t, "Pos Gerbang", z.Name, "the name is trimmed")
		require.Equal(t, "PATROL", z.Type)
		require.Equal(t, "1K-IBMI-S", z.ProjectCode)
		require.Nil(t, z.TeamCode)
		require.Equal(t, 75, *z.RadiusMeters)
		require.Equal(t, 3, z.MinVisit)
		require.True(t, z.PhotoRequired, "photo_required defaults to true")
		require.True(t, z.IsActive)
		require.Equal(t, "2026-01-15", *z.StartDate)

		require.Zero(t, z.CheckpointCount)
		require.NotNil(t, z.Checkpoints, "a zone's detail has its checkpoints, here none, as an array")
		require.Empty(t, z.Checkpoints)
		require.Nil(t, z.NextCheckpointID)

		code, again, raw := f.zoneCall(http.MethodGet, zonePath(id), f.adminTok, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, z, again, "reading it back gives the same zone")
	})

	t.Run("a point sent without a radius gets 50, and no point means no geofence", func(t *testing.T) {
		code, z, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
			"project_code": "1K-IBMI-S", "type": "GUARD", "name": "Pos Jaga", "latitude": -6.21, "longitude": 106.81,
		})
		require.Equal(t, http.StatusCreated, code, string(raw))
		require.Equal(t, 50, *z.RadiusMeters)

		code, bare, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
			"project_code": "1K-IBMI-S", "type": "GUARD", "name": "Pos tanpa geofence", "photo_required": false,
		})
		require.Equal(t, http.StatusCreated, code, string(raw))
		require.Nil(t, bare.Latitude)
		require.Nil(t, bare.RadiusMeters)
		require.False(t, bare.PhotoRequired)
		require.Zero(t, bare.CheckpointCount)
		require.Equal(t, 1, bare.MinVisit)
	})

	t.Run("the list filters by project and type and searches name and alias", func(t *testing.T) {
		code, all, raw := f.zoneList("/api/v1/admin/zones?project_code=1K-IBMI-S", f.adminTok)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, all, 3)
		_, guards, _ := f.zoneList("/api/v1/admin/zones?type=GUARD", f.adminTok)
		require.Len(t, guards, 2)
		_, found, _ := f.zoneList("/api/v1/admin/zones?q=gerbang", f.adminTok)
		require.Len(t, found, 1, "matches the alias and the name, ignoring case")
		require.Equal(t, id, found[0].ID)
		require.Nil(t, found[0].Checkpoints, "the list carries only the count of checkpoints")
		code, _, raw = f.zoneList("/api/v1/admin/zones?type=BOGUS", f.adminTok)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("a patch changes what was sent, and null empties", func(t *testing.T) {
		code, z, raw := f.zoneCall(http.MethodPatch, zonePath(id), f.adminTok, map[string]any{
			"name": "Pos Utama", "min_visit": 2, "alias": nil, "photo_required": false, "team_code": "1K-IBMI-S1",
		})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, "Pos Utama", z.Name)
		require.Equal(t, 2, z.MinVisit)
		require.Nil(t, z.Alias)
		require.False(t, z.PhotoRequired)
		require.Equal(t, "1K-IBMI-S1", *z.TeamCode)
		require.Equal(t, "Gerbang depan", *z.Description, "what was not sent is kept")
		require.Equal(t, 75, *z.RadiusMeters)

		code, z, raw = f.zoneCall(http.MethodPatch, zonePath(id), f.adminTok, map[string]any{"team_code": nil, "latitude": nil, "longitude": nil})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Nil(t, z.TeamCode)
		require.Nil(t, z.Latitude)
		require.Nil(t, z.RadiusMeters, "taking the point away takes the radius with it")

		code, z, raw = f.zoneCall(http.MethodPatch, zonePath(id), f.adminTok, map[string]any{"latitude": -6.3, "longitude": 106.9})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, 50, *z.RadiusMeters, "a new point on a zone without a geofence gets the default radius")
	})

	t.Run("geofence mistakes name the field", func(t *testing.T) {
		for name, tc := range map[string]struct {
			body  map[string]any
			field string
		}{
			"a latitude alone":         {map[string]any{"latitude": -6.2}, "latitude"},
			"a radius without a point": {map[string]any{"radius_meters": 40}, "radius_meters"},
			"a latitude out of range":  {map[string]any{"latitude": 95.0, "longitude": 106.8}, "latitude"},
			"a radius of zero":         {map[string]any{"latitude": -6.2, "longitude": 106.8, "radius_meters": 0}, "radius_meters"},
		} {
			body := map[string]any{"project_code": "1K-IBMI-S", "type": "PATROL", "name": "X"}
			for k, v := range tc.body {
				body[k] = v
			}
			code, _, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, body)
			require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
			_, fields := problem(t, raw)
			require.Contains(t, fields, tc.field, name)
		}
		code, _, raw := f.zoneCall(http.MethodPatch, zonePath(id), f.adminTok, map[string]any{"latitude": nil})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("a regu of another project is refused", func(t *testing.T) {
		f.exec(`INSERT INTO projects (code, client_code, description, business_line, timezone) VALUES ('1K-IBMI-C', '1K-IBMI', 'IBMI kebersihan', 'cleaning', 'Asia/Jakarta')`)
		f.exec(`INSERT INTO teams (code, project_code, description, active) VALUES ('1K-IBMI-C1', '1K-IBMI-C', 'IBMI C1', true)`)
		code, _, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
			"project_code": "1K-IBMI-S", "team_code": "1K-IBMI-C1", "type": "PATROL", "name": "Salah regu",
		})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		_, fields := problem(t, raw)
		require.Equal(t, []string{"team_code"}, fields)

		code, _, raw = f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
			"project_code": "9Z-NOPE-S", "type": "PATROL", "name": "Tanpa proyek",
		})
		require.Equal(t, http.StatusNotFound, code, string(raw))
		reason, _ := problem(t, raw)
		require.Equal(t, "PROJECT_NOT_FOUND", reason)
	})

	t.Run("a bad body is a validation error naming the field", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"no name":      {"project_code": "1K-IBMI-S", "type": "PATROL"},
			"a bad type":   {"project_code": "1K-IBMI-S", "type": "ROUTE", "name": "X"},
			"a blank name": {"project_code": "1K-IBMI-S", "type": "PATROL", "name": "   "},
			"min_visit 0":  {"project_code": "1K-IBMI-S", "type": "PATROL", "name": "X", "min_visit": 0},
			"a bad date":   {"project_code": "1K-IBMI-S", "type": "PATROL", "name": "X", "start_date": "15-01-2026"},
		} {
			code, _, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, body)
			require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
		}
	})

	t.Run("a delete keeps the row, and the zone is gone", func(t *testing.T) {
		code, z, _ := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
			"project_code": "1K-IBMI-S", "type": "PATROL", "name": "Sementara",
		})
		require.Equal(t, http.StatusCreated, code)
		code, raw := f.do(http.MethodDelete, zonePath(z.ID), f.adminTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		code, _, raw = f.zoneCall(http.MethodGet, zonePath(z.ID), f.adminTok, nil)
		require.Equal(t, http.StatusNotFound, code, string(raw))
		reason, _ := problem(t, raw)
		require.Equal(t, "ZONE_NOT_FOUND", reason)
		var active bool
		var deleted bool
		f.query([]any{&active, &deleted}, "SELECT active, deleted_at IS NOT NULL FROM patrol_zones WHERE id = $1", z.ID)
		require.False(t, active)
		require.True(t, deleted)
		code, raw = f.do(http.MethodDelete, zonePath(z.ID), f.adminTok, nil, nil)
		require.Equal(t, http.StatusNotFound, code, "deleting twice: "+string(raw))
		_, all, _ := f.zoneList("/api/v1/admin/zones", f.adminTok)
		require.Len(t, all, 3, "it is not listed")
	})

	t.Run("an id that is not a number is a zone that does not exist", func(t *testing.T) {
		code, _ := f.do(http.MethodGet, "/api/v1/admin/zones/abc", f.adminTok, nil, nil)
		require.Equal(t, http.StatusNotFound, code)
	})

	t.Run("every change is audited, with no secret", func(t *testing.T) {
		var n int
		f.query([]any{&n}, "SELECT count(*) FROM audit_logs WHERE entity_type = 'patrol_zone' AND action IN ('patrol.zone.create', 'patrol.zone.update', 'patrol.zone.delete')")
		require.GreaterOrEqual(t, n, 6)
		var secret string
		f.query([]any{&secret}, "SELECT qr_secret FROM patrol_zones WHERE id = $1", id)
		var leaked int
		f.query([]any{&leaked}, "SELECT count(*) FROM audit_logs WHERE metadata::text LIKE '%' || $1 || '%'", secret)
		require.Zero(t, leaked)
	})
}

// tryExec runs a statement and returns its error instead of failing the test.
func (f *flow) tryExec(sql string, args ...any) error {
	f.t.Helper()
	return database.WithTenantTx(f.t.Context(), testkit.Pool(f.t), f.org, func(tx pgx.Tx) error {
		_, err := tx.Exec(f.t.Context(), sql, args...)
		return err
	})
}

// doHeader is a GET that also returns the response headers, for the QR image.
func (f *flow) doHeader(method, path, token string) (int, http.Header, []byte) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.srv.URL+path, nil)
	require.NoError(f.t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(f.t, err)
	return res.StatusCode, res.Header, body
}

func TestPatrolQR(t *testing.T) {
	f := newFlow(t)
	_, z, _ := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
		"project_code": "1K-IBMI-S", "type": "PATROL", "name": "Pos Gerbang Utama!",
	})
	qr := func(query string) (int, http.Header, []byte) {
		return f.doHeader(http.MethodGet, zonePath(z.ID)+"/qr"+query, f.adminTok)
	}

	t.Run("the default is a PNG named after the zone", func(t *testing.T) {
		code, header, body := qr("")
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "image/png", header.Get("Content-Type"))
		require.Contains(t, header.Get("Content-Disposition"), `filename=qr-zona-pos-gerbang-utama.png`)
		require.Contains(t, header.Get("Cache-Control"), "no-store")
		img, err := png.Decode(bytes.NewReader(body))
		require.NoError(t, err, "it is a real PNG")
		require.Positive(t, img.Bounds().Dx())
	})

	t.Run("an SVG is offered too", func(t *testing.T) {
		code, header, body := qr("?format=svg")
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "image/svg+xml", header.Get("Content-Type"))
		require.Contains(t, header.Get("Content-Disposition"), ".svg")
		require.True(t, strings.HasPrefix(string(body), `<?xml`) && strings.Contains(string(body), "<svg"))
	})

	t.Run("an unknown format is refused", func(t *testing.T) {
		code, _, body := qr("?format=pdf")
		require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
		_, fields := problem(t, body)
		require.Equal(t, []string{"format"}, fields)
	})

	t.Run("rotating the secret changes the QR, and the payload holds the organization, the zone and the secret", func(t *testing.T) {
		var secret string
		f.query([]any{&secret}, "SELECT qr_secret FROM patrol_zones WHERE id = $1", z.ID)
		require.Len(t, secret, 22)
		_, _, before := qr("?format=svg")

		code, rotated, raw := f.zoneCall(http.MethodPost, zonePath(z.ID)+"/rotate-qr", f.adminTok, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, z.ID, rotated.ID)
		require.NotContains(t, string(raw), "qr_secret")

		var after string
		f.query([]any{&after}, "SELECT qr_secret FROM patrol_zones WHERE id = $1", z.ID)
		require.Len(t, after, 22)
		require.NotEqual(t, secret, after)
		_, _, changed := qr("?format=svg")
		require.NotEqual(t, before, changed, "the printed code is different")

		var logged int
		f.query([]any{&logged}, "SELECT count(*) FROM audit_logs WHERE action = 'patrol.zone.rotate_qr' AND entity_id = $1", z.ID)
		require.Equal(t, 1, logged)
	})
}

func TestPatrolZoneFieldReads(t *testing.T) {
	f := newFlow(t)
	create := func(body map[string]any) zoneOut {
		body["project_code"] = "1K-IBMI-S"
		code, z, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, body)
		require.Equal(t, http.StatusCreated, code, string(raw))
		return z
	}
	whole := create(map[string]any{"type": "PATROL", "name": "Seluruh proyek"})
	own := create(map[string]any{"type": "GUARD", "name": "Khusus S1", "team_code": "1K-IBMI-S1"})
	other := create(map[string]any{"type": "PATROL", "name": "Khusus S2", "team_code": "1K-IBMI-S2"})
	off := create(map[string]any{"type": "PATROL", "name": "Dimatikan", "is_active": false})

	t.Run("a member lists the zones of their project and regu, with the count of checkpoints", func(t *testing.T) {
		code, zones, raw := f.zoneList("/api/v1/zones", f.personTok)
		require.Equal(t, http.StatusOK, code, string(raw))
		var names []string
		for _, z := range zones {
			names = append(names, z.Name)
		}
		require.ElementsMatch(t, []string{"Seluruh proyek", "Khusus S1"}, names, "not S2's zone, not the switched-off one")
		for _, z := range zones {
			require.Nil(t, z.Checkpoints, "the list carries the count only: the checkpoints are in the zone's own read")
		}
		_ = other
		_ = off
	})

	t.Run("the list filters by type and regu", func(t *testing.T) {
		_, zones, _ := f.zoneList("/api/v1/zones?type=GUARD", f.personTok)
		require.Len(t, zones, 1)
		require.Equal(t, own.ID, zones[0].ID)
		_, zones, _ = f.zoneList("/api/v1/zones?project_code=1K-IBMI-S&team_code=1K-IBMI-S1", f.personTok)
		require.Len(t, zones, 2)
		code, _, raw := f.zoneList("/api/v1/zones?type=BOGUS", f.personTok)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
	})

	t.Run("nothing a field user reads holds the QR secret", func(t *testing.T) {
		var secret string
		f.query([]any{&secret}, "SELECT qr_secret FROM patrol_zones WHERE id = $1", whole.ID)
		_, list := f.do(http.MethodGet, "/api/v1/zones", f.personTok, nil, nil)
		_, one := f.do(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(whole.ID, 10), f.personTok, nil, nil)
		for _, body := range [][]byte{list, one} {
			require.NotContains(t, string(body), secret)
			require.NotContains(t, string(body), "qr_secret")
			require.NotContains(t, string(body), "PERSIST-ZONE")
		}
		// The admin's JSON does not carry it either: the secret lives in the image only.
		_, admin := f.do(http.MethodGet, zonePath(whole.ID), f.adminTok, nil, nil)
		require.NotContains(t, string(admin), secret)
	})

	t.Run("one zone is readable by a member of its project", func(t *testing.T) {
		code, z, raw := f.zoneCall(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(whole.ID, 10), f.personTok, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, whole, z)
		code, _, _ = f.zoneCall(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(own.ID, 10), f.personTok, nil)
		require.Equal(t, http.StatusOK, code)
	})

	t.Run("a non-member gets 403, a zone of another regu too", func(t *testing.T) {
		stranger := f.addPerson(900301, "ORANGLAIN1", "Orang Lain")
		strangerTok := f.token(stranger, f.addSession(stranger, "tok-stranger"))
		code, _, raw := f.zoneCall(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(whole.ID, 10), strangerTok, nil)
		require.Equal(t, http.StatusForbidden, code, string(raw))
		reason, _ := problem(t, raw)
		require.Equal(t, "PERMISSION_DENIED", reason)
		code, _, raw = f.zoneList("/api/v1/zones?project_code=1K-IBMI-S", strangerTok)
		require.Equal(t, http.StatusForbidden, code, string(raw))
		code, zones, _ := f.zoneList("/api/v1/zones", strangerTok)
		require.Equal(t, http.StatusOK, code)
		require.Empty(t, zones, "without a project asked for, a stranger just sees nothing")

		// f.person serves in S1; S2's membership starts in ten days.
		code, _, raw = f.zoneCall(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(other.ID, 10), f.personTok, nil)
		require.Equal(t, http.StatusForbidden, code, string(raw))
	})

	t.Run("another organization's zone is 404, and so is one that is gone", func(t *testing.T) {
		tok, _, err := f.tokens.NewAccessToken(900999, 2, 5999)
		require.NoError(t, err)
		code, _, raw := f.zoneCall(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(whole.ID, 10), tok, nil)
		require.Equal(t, http.StatusNotFound, code, string(raw))
		reason, _ := problem(t, raw)
		require.Equal(t, "ZONE_NOT_FOUND", reason)
		code, _, _ = f.zoneCall(http.MethodGet, "/api/v1/zones/99999999", f.personTok, nil)
		require.Equal(t, http.StatusNotFound, code)
	})
}

func TestPatrolZoneRegionScope(t *testing.T) {
	f := newFlow(t)
	f.exec(`INSERT INTO regions (code, description) VALUES ('OS2', 'Outsourcing 2')`)
	regional := f.addPerson(900302, "REGADMIN02", "Admin Wilayah Dua")
	f.exec("INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, 'REGION_ADMIN', 'OS2')", regional)
	regionalTok := f.token(regional, f.addSession(regional, "tok-regional"))
	home := f.addPerson(900303, "REGADMIN01", "Admin Wilayah Satu")
	f.exec("INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, 'REGION_ADMIN', 'OS1')", home)
	homeTok := f.token(home, f.addSession(home, "tok-home"))

	code, z, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
		"project_code": "1K-IBMI-S", "type": "PATROL", "name": "Pos A",
	})
	require.Equal(t, http.StatusCreated, code, string(raw))

	t.Run("an admin of another region sees nothing and is refused", func(t *testing.T) {
		_, zones, _ := f.zoneList("/api/v1/admin/zones", regionalTok)
		require.Empty(t, zones)
		for _, call := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, zonePath(z.ID), nil},
			{http.MethodPatch, zonePath(z.ID), map[string]any{"name": "Dicuri"}},
			{http.MethodDelete, zonePath(z.ID), nil},
			{http.MethodPost, zonePath(z.ID) + "/rotate-qr", nil},
			{http.MethodGet, zonePath(z.ID) + "/qr", nil},
			{http.MethodPut, zonePath(z.ID) + "/checkpoints", map[string]any{"checkpoint_ids": []any{}}},
			{http.MethodPost, "/api/v1/admin/zones", map[string]any{"project_code": "1K-IBMI-S", "type": "PATROL", "name": "Zona asing"}},
		} {
			var raw []byte
			if call.body != nil {
				raw = jsonBody(t, call.body)
			}
			code, resp := f.do(call.method, call.path, regionalTok, raw, nil)
			require.Equal(t, http.StatusForbidden, code, call.method+" "+call.path+": "+string(resp))
			reason, _ := problem(t, resp)
			require.Equal(t, "ACCESS_OUTSIDE_ADMIN_SCOPE", reason)
		}
		code, _, _ := f.zoneList("/api/v1/admin/zones?project_code=1K-IBMI-S", regionalTok)
		require.Equal(t, http.StatusForbidden, code)
		code, got, _ := f.zoneCall(http.MethodGet, zonePath(z.ID), f.adminTok, nil)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "Pos A", got.Name, "and nothing was changed")
	})

	t.Run("an admin of the zone's region has full access", func(t *testing.T) {
		_, zones, _ := f.zoneList("/api/v1/admin/zones", homeTok)
		require.Len(t, zones, 1)
		code, _, raw := f.zoneCall(http.MethodPatch, zonePath(z.ID), homeTok, map[string]any{"name": "Pos A2"})
		require.Equal(t, http.StatusOK, code, string(raw))
	})
}

// A zone is told apart by its name inside a project and type, ignoring case and outer spaces.
func TestPatrolZoneNameIsUnique(t *testing.T) {
	f := newFlow(t)
	create := func(typ, name string) (int, zoneOut, []byte) {
		return f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok,
			map[string]any{"project_code": "1K-IBMI-S", "type": typ, "name": name})
	}

	code, z, raw := create("PATROL", "Pos Depan")
	require.Equal(t, http.StatusCreated, code, string(raw))

	t.Run("the same name, in any case, in the same project and type is refused", func(t *testing.T) {
		for _, name := range []string{"Pos Depan", "  pos depan ", "POS DEPAN"} {
			code, _, raw := create("PATROL", name)
			reasonIs(t, code, raw, http.StatusConflict, "ZONE_NAME_TAKEN")
		}
	})

	t.Run("another type may use the name", func(t *testing.T) {
		code, _, raw := create("GUARD", "Pos Depan")
		require.Equal(t, http.StatusCreated, code, string(raw))
	})

	t.Run("renaming a zone to a taken name is refused", func(t *testing.T) {
		code, other, raw := create("PATROL", "Pos Belakang")
		require.Equal(t, http.StatusCreated, code, string(raw))
		code, _, raw = f.zoneCall(http.MethodPatch, zonePath(other.ID), f.adminTok, map[string]any{"name": "pos depan"})
		reasonIs(t, code, raw, http.StatusConflict, "ZONE_NAME_TAKEN")
	})

	t.Run("a deleted zone frees its name", func(t *testing.T) {
		code, _, raw := f.zoneCall(http.MethodDelete, zonePath(z.ID), f.adminTok, nil)
		require.Less(t, code, 300, string(raw))
		code, _, raw = create("PATROL", "Pos Depan")
		require.Equal(t, http.StatusCreated, code, string(raw))
	})
}

// A PATCH has no validate tags (a field may be left out or null), so its limits are checked on the
// result: a latitude off the globe or a radius of 0 was a 500 from the database's CHECK.
func TestPatrolZoneUpdateIsBounded(t *testing.T) {
	f := newFlow(t)
	code, z, raw := f.zoneCall(http.MethodPost, "/api/v1/admin/zones", f.adminTok, map[string]any{
		"project_code": "1K-IBMI-S", "type": "PATROL", "name": "Pos Batas", "latitude": -6.2, "longitude": 106.8, "radius_meters": 50,
	})
	require.Equal(t, http.StatusCreated, code, string(raw))

	for name, body := range map[string]map[string]any{
		"latitude 95":       {"latitude": 95},
		"latitude 1000":     {"latitude": 1000},
		"longitude -181":    {"longitude": -181},
		"radius 0":          {"radius_meters": 0},
		"radius 5001":       {"radius_meters": 5001},
		"radius past int32": {"radius_meters": 5_000_000_000},
		"radius negative":   {"radius_meters": -3},
	} {
		code, _, raw := f.zoneCall(http.MethodPatch, zonePath(z.ID), f.adminTok, body)
		require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
	}

	code, got, raw := f.zoneCall(http.MethodPatch, zonePath(z.ID), f.adminTok, map[string]any{"latitude": -6.3, "radius_meters": 5000})
	require.Equal(t, http.StatusOK, code, "the limits themselves are fine: "+string(raw))
	require.Equal(t, 5000, *got.RadiusMeters)
}
