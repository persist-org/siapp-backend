package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/database"
	"siapp/pkg/platform/testkit"
)

// Phase 10.7: the admin routes of checkpoints, their checklist and their QR.

type actionOut struct {
	ID             string  `json:"id"`
	SortOrder      int     `json:"sort_order"`
	Action         string  `json:"action"`
	Instruction    *string `json:"instruction"`
	MinPhotos      int     `json:"min_photos"`
	MaxPhotos      int     `json:"max_photos"`
	PhotoRequired  bool    `json:"photo_required"`
	IsNoteRequired bool    `json:"is_note_required"`
}

type checkpointZoneOut struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	SortOrder int    `json:"sort_order"`
}

type checkpointOut struct {
	ID           string              `json:"id"`
	ProjectCode  string              `json:"project_code"`
	Name         string              `json:"name"`
	Description  *string             `json:"description"`
	Latitude     *float64            `json:"latitude"`
	Longitude    *float64            `json:"longitude"`
	RadiusMeters *int                `json:"radius_meters"`
	IsActive     bool                `json:"is_active"`
	QrRotatedAt  string              `json:"qr_rotated_at"`
	Actions      []actionOut         `json:"actions"`
	Zones        []checkpointZoneOut `json:"zones"`
}

const checkpointsPath = "/api/v1/admin/checkpoints"

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func checkpointPath(id string) string { return checkpointsPath + "/" + id }

// cpCall sends a request and decodes the checkpoint of a 2xx answer.
func (f *flow) cpCall(method, path, token string, body any) (int, checkpointOut, []byte) {
	f.t.Helper()
	var raw []byte
	if body != nil {
		raw = jsonBody(f.t, body)
	}
	code, resp := f.do(method, path, token, raw, nil)
	var env struct {
		Data checkpointOut `json:"data"`
	}
	if code < 300 {
		require.NoError(f.t, json.Unmarshal(resp, &env), string(resp))
	}
	return code, env.Data, resp
}

func (f *flow) cpList(path, token string) (int, []checkpointOut, []byte) {
	f.t.Helper()
	code, resp := f.do(http.MethodGet, path, token, nil, nil)
	var env struct {
		Data []checkpointOut `json:"data"`
	}
	if code == http.StatusOK {
		require.NoError(f.t, json.Unmarshal(resp, &env), string(resp))
	}
	return code, env.Data, resp
}

// newCheckpoint creates a checkpoint as the admin and returns it.
func (f *flow) newCheckpoint(project, name string, extra map[string]any) checkpointOut {
	f.t.Helper()
	body := map[string]any{"project_code": project, "name": name}
	for k, v := range extra {
		body[k] = v
	}
	code, c, raw := f.cpCall(http.MethodPost, checkpointsPath, f.adminTok, body)
	require.Equal(f.t, http.StatusCreated, code, string(raw))
	return c
}

func threeActions() []map[string]any {
	return []map[string]any{
		{"action": "Periksa gembok", "instruction": "Tarik gembok", "min_photos": 1, "max_photos": 2},
		{"action": "Periksa lampu", "max_photos": 0, "is_note_required": true},
		{"action": "Isi buku tamu"},
	}
}

func TestPatrolCheckpointAdminCRUD(t *testing.T) {
	f := newFlow(t)
	f.addProject("1K-IBMI-C", "cleaning")

	var created checkpointOut
	t.Run("a checkpoint with a point and a checklist is saved and returned as sent", func(t *testing.T) {
		code, c, raw := f.cpCall(http.MethodPost, checkpointsPath, f.adminTok, map[string]any{
			"project_code": "1K-IBMI-S", "name": "  Gerbang Utama  ", "description": "Gerbang depan",
			"latitude": -6.2, "longitude": 106.8, "actions": threeActions(),
		})
		require.Equal(t, http.StatusCreated, code, string(raw))
		created = c
		require.Len(t, c.ID, 36, "a uuid")
		require.Equal(t, "Gerbang Utama", c.Name, "the name is trimmed")
		require.Equal(t, "1K-IBMI-S", c.ProjectCode)
		require.Equal(t, 30, *c.RadiusMeters, "a point sent without a radius gets the checkpoint default")
		require.True(t, c.IsActive)
		require.NotEmpty(t, c.QrRotatedAt)
		require.NotNil(t, c.Zones)
		require.Empty(t, c.Zones, "a new checkpoint is in no zone, and the field is an array, not null")

		require.Len(t, c.Actions, 3)
		gembok, lampu, buku := c.Actions[0], c.Actions[1], c.Actions[2]
		require.Equal(t, "Periksa gembok", gembok.Action)
		require.Equal(t, "Tarik gembok", *gembok.Instruction)
		require.Equal(t, 1, gembok.MinPhotos)
		require.Equal(t, 2, gembok.MaxPhotos)
		require.True(t, gembok.PhotoRequired)
		require.False(t, lampu.PhotoRequired)
		require.Equal(t, 0, lampu.MaxPhotos, "max_photos 0 is a checklist action with no photo slot")
		require.True(t, lampu.IsNoteRequired)
		require.Equal(t, 0, buku.MinPhotos, "defaults: the photo is optional, one at most, and no note is required")
		require.Equal(t, 1, buku.MaxPhotos)
		require.False(t, buku.IsNoteRequired)
		for i, a := range c.Actions {
			require.Equal(t, i, a.SortOrder)
		}

		code, again, raw := f.cpCall(http.MethodGet, checkpointPath(c.ID), f.adminTok, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, c, again, "reading it back gives the same checkpoint")
	})

	t.Run("a checkpoint without a point has no geofence, and no checklist is an empty array", func(t *testing.T) {
		c := f.newCheckpoint("1K-IBMI-S", "Pintu Belakang", nil)
		require.Nil(t, c.Latitude)
		require.Nil(t, c.RadiusMeters)
		require.NotNil(t, c.Actions)
		require.Empty(t, c.Actions)
	})

	t.Run("min_photos alone widens max_photos instead of failing", func(t *testing.T) {
		c := f.newCheckpoint("1K-IBMI-S", "Panel Listrik", map[string]any{"actions": []map[string]any{{"action": "Cek panel", "min_photos": 3}}})
		require.Equal(t, 3, c.Actions[0].MinPhotos)
		require.Equal(t, 3, c.Actions[0].MaxPhotos)
	})

	t.Run("a name is unique inside a project, ignoring case and outer spaces", func(t *testing.T) {
		for _, name := range []string{"Gerbang Utama", "  gerbang utama ", "GERBANG UTAMA"} {
			code, _, raw := f.cpCall(http.MethodPost, checkpointsPath, f.adminTok, map[string]any{"project_code": "1K-IBMI-S", "name": name})
			require.Equal(t, http.StatusConflict, code, name+": "+string(raw))
			reason, _ := problem(t, raw)
			require.Equal(t, "CHECKPOINT_NAME_TAKEN", reason)
		}
		f.newCheckpoint("1K-IBMI-C", "Gerbang Utama", nil) // another project may reuse it
	})

	t.Run("a project that does not exist, and a body that is wrong, are refused", func(t *testing.T) {
		code, _, _ := f.cpCall(http.MethodPost, checkpointsPath, f.adminTok, map[string]any{"project_code": "1K-NONE-S", "name": "Tersesat"})
		require.Equal(t, http.StatusNotFound, code)
		for name, tc := range map[string]struct {
			body  map[string]any
			field string
		}{
			"a missing name":          {map[string]any{"project_code": "1K-IBMI-S"}, "name"},
			"a blank name":            {map[string]any{"project_code": "1K-IBMI-S", "name": "   "}, "name"},
			"a latitude out of range": {map[string]any{"project_code": "1K-IBMI-S", "name": "X1", "latitude": 95.0, "longitude": 106.8}, "latitude"},
			"half a point":            {map[string]any{"project_code": "1K-IBMI-S", "name": "X2", "latitude": -6.2}, "latitude"},
			"a radius without point":  {map[string]any{"project_code": "1K-IBMI-S", "name": "X3", "radius_meters": 30}, "radius_meters"},
			"a radius that is huge":   {map[string]any{"project_code": "1K-IBMI-S", "name": "X4", "latitude": -6.2, "longitude": 106.8, "radius_meters": 1001}, "radius_meters"},
			"a radius of zero":        {map[string]any{"project_code": "1K-IBMI-S", "name": "X5", "latitude": -6.2, "longitude": 106.8, "radius_meters": 0}, "radius_meters"},
		} {
			code, _, raw := f.cpCall(http.MethodPost, checkpointsPath, f.adminTok, tc.body)
			require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
			_, fields := problem(t, raw)
			require.Contains(t, fields, tc.field, name)
		}
	})

	t.Run("an update changes only what was sent, and the geofence stays whole", func(t *testing.T) {
		path := checkpointPath(created.ID)
		code, c, raw := f.cpCall(http.MethodPatch, path, f.adminTok, map[string]any{"name": "Gerbang Depan", "radius_meters": 80})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, "Gerbang Depan", c.Name)
		require.Equal(t, 80, *c.RadiusMeters)
		require.Equal(t, -6.2, *c.Latitude, "the point was not sent, so it stayed")
		require.Equal(t, "Gerbang depan", *c.Description)
		require.Len(t, c.Actions, 3, "the checklist is not touched by a PATCH")

		code, c, raw = f.cpCall(http.MethodPatch, path, f.adminTok, map[string]any{"latitude": nil, "longitude": nil})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Nil(t, c.Latitude)
		require.Nil(t, c.RadiusMeters, "taking the point away takes the radius with it")

		code, c, raw = f.cpCall(http.MethodPatch, path, f.adminTok, map[string]any{"latitude": -6.3, "longitude": 106.9})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, 30, *c.RadiusMeters, "a point back without a radius gets the default")

		code, _, raw = f.cpCall(http.MethodPatch, path, f.adminTok, map[string]any{"radius_meters": nil})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		_, fields := problem(t, raw)
		require.Contains(t, fields, "radius_meters", "a point cannot lose its radius")

		code, c, raw = f.cpCall(http.MethodPatch, path, f.adminTok, map[string]any{"is_active": false, "description": nil})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.False(t, c.IsActive)
		require.Nil(t, c.Description)
		_, c, _ = f.cpCall(http.MethodPatch, path, f.adminTok, map[string]any{"is_active": true})
		require.True(t, c.IsActive)
	})

	t.Run("the project cannot be changed", func(t *testing.T) {
		path := checkpointPath(created.ID)
		code, _, raw := f.cpCall(http.MethodPatch, path, f.adminTok, map[string]any{"project_code": "1K-IBMI-C"})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		_, fields := problem(t, raw)
		require.Equal(t, []string{"project_code"}, fields)
		code, _, raw = f.cpCall(http.MethodPatch, path, f.adminTok, map[string]any{"project_code": "1K-IBMI-S"})
		require.Equal(t, http.StatusOK, code, "sending the current project is harmless: "+string(raw))
	})

	t.Run("renaming onto another checkpoint's name is refused", func(t *testing.T) {
		code, _, raw := f.cpCall(http.MethodPatch, checkpointPath(created.ID), f.adminTok, map[string]any{"name": "pintu belakang"})
		require.Equal(t, http.StatusConflict, code, string(raw))
		reason, _ := problem(t, raw)
		require.Equal(t, "CHECKPOINT_NAME_TAKEN", reason)
	})

	t.Run("an id that is not a uuid, and one that does not exist, are a checkpoint that is not there", func(t *testing.T) {
		for _, id := range []string{"abc", "123", "00000000-0000-0000-0000-000000000000", "3f2504e0-4f89-41d3-9a0c-0305e82c3301"} {
			for _, call := range []struct {
				method, suffix string
				body           any
			}{
				{http.MethodGet, "", nil},
				{http.MethodPatch, "", map[string]any{"name": "Hantu"}},
				{http.MethodDelete, "", nil},
				{http.MethodPut, "/actions", map[string]any{"actions": []any{}}},
				{http.MethodPost, "/rotate-qr", nil},
				{http.MethodGet, "/qr", nil},
			} {
				var raw []byte
				if call.body != nil {
					raw = jsonBody(t, call.body)
				}
				code, resp := f.do(call.method, checkpointPath(id)+call.suffix, f.adminTok, raw, nil)
				require.Equal(t, http.StatusNotFound, code, call.method+" "+id+call.suffix+": "+string(resp))
				reason, _ := problem(t, resp)
				require.Equal(t, "CHECKPOINT_NOT_FOUND", reason)
			}
		}
	})

	t.Run("a field user cannot use the admin routes", func(t *testing.T) {
		code, _, _ := f.cpList(checkpointsPath, f.personTok)
		require.Equal(t, http.StatusForbidden, code)
		code, _, _ = f.cpCall(http.MethodPost, checkpointsPath, f.personTok, map[string]any{"project_code": "1K-IBMI-S", "name": "Curang"})
		require.Equal(t, http.StatusForbidden, code)
		code, _, _ = f.cpCall(http.MethodGet, checkpointPath(created.ID), "", nil)
		require.Equal(t, http.StatusUnauthorized, code)
	})

	t.Run("every change is audited", func(t *testing.T) {
		var n int
		f.query([]any{&n}, "SELECT count(*) FROM audit_logs WHERE entity_type = 'patrol_checkpoint' AND entity_code = $1 AND action IN ('patrol.checkpoint.create', 'patrol.checkpoint.update')", created.ID)
		require.GreaterOrEqual(t, n, 5)
	})
}

func TestPatrolCheckpointList(t *testing.T) {
	f := newFlow(t)
	f.addProject("1K-IBMI-C", "cleaning")
	gerbang := f.newCheckpoint("1K-IBMI-S", "Gerbang", map[string]any{"actions": threeActions()[:1]})
	pagar := f.newCheckpoint("1K-IBMI-S", "Pagar", nil)
	f.newCheckpoint("1K-IBMI-C", "Lorong", nil)
	off := f.newCheckpoint("1K-IBMI-S", "Dimatikan", map[string]any{"is_active": false})

	north, south := f.addZone("1K-IBMI-S", "Zona Utara"), f.addZone("1K-IBMI-S", "Zona Selatan")
	require.NoError(t, f.link("1K-IBMI-S", north, gerbang.ID, 1))
	require.NoError(t, f.link("1K-IBMI-S", north, pagar.ID, 2))
	require.NoError(t, f.link("1K-IBMI-S", south, gerbang.ID, 3))

	names := func(list []checkpointOut) (out []string) {
		for _, c := range list {
			out = append(out, c.Name)
		}
		return out
	}

	t.Run("the list holds the zones and the checklist of each checkpoint, in order", func(t *testing.T) {
		code, list, raw := f.cpList(checkpointsPath+"?project_code=1K-IBMI-S", f.adminTok)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, []string{"Dimatikan", "Gerbang", "Pagar"}, names(list), "by name")
		for _, c := range list {
			if c.ID != gerbang.ID {
				continue
			}
			require.Equal(t, []checkpointZoneOut{
				{ID: south, Name: "Zona Selatan", Type: "PATROL", SortOrder: 3},
				{ID: north, Name: "Zona Utara", Type: "PATROL", SortOrder: 1},
			}, c.Zones, "its own position in each zone")
			require.Len(t, c.Actions, 1)
		}
	})

	t.Run("the filters", func(t *testing.T) {
		_, list, _ := f.cpList(checkpointsPath, f.adminTok)
		require.Len(t, list, 4)
		_, list, _ = f.cpList(checkpointsPath+"?project_code=1K-IBMI-C", f.adminTok)
		require.Equal(t, []string{"Lorong"}, names(list))
		_, list, _ = f.cpList(checkpointsPath+"?q=gerb", f.adminTok)
		require.Equal(t, []string{"Gerbang"}, names(list))
		_, list, _ = f.cpList(checkpointsPath+"?is_active=false", f.adminTok)
		require.Equal(t, []string{off.Name}, names(list))
		_, list, _ = f.cpList(checkpointsPath+"?zone_id="+itoa(north), f.adminTok)
		require.ElementsMatch(t, []string{"Gerbang", "Pagar"}, names(list))
		_, list, _ = f.cpList(checkpointsPath+"?zone_id="+itoa(south), f.adminTok)
		require.Equal(t, []string{"Gerbang"}, names(list))
		for _, bad := range []string{"zone_id=abc", "zone_id=0", "is_active=maybe"} {
			code, _, raw := f.cpList(checkpointsPath+"?"+bad, f.adminTok)
			require.Equal(t, http.StatusUnprocessableEntity, code, bad+": "+string(raw))
		}
	})

	t.Run("paging", func(t *testing.T) {
		code, raw := f.do(http.MethodGet, checkpointsPath+"?limit=2&page=2", f.adminTok, nil, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		var env struct {
			Data       []checkpointOut `json:"data"`
			Pagination struct {
				Total int `json:"total"`
			} `json:"pagination"`
		}
		require.NoError(t, json.Unmarshal(raw, &env))
		require.Len(t, env.Data, 2)
		require.Equal(t, 4, env.Pagination.Total)
	})
}

func TestPatrolCheckpointActions(t *testing.T) {
	f := newFlow(t)
	c := f.newCheckpoint("1K-IBMI-S", "Gerbang", map[string]any{"actions": threeActions()})
	other := f.newCheckpoint("1K-IBMI-S", "Pagar", map[string]any{"actions": threeActions()[:1]})
	path := checkpointPath(c.ID) + "/actions"
	put := func(actions any) (int, checkpointOut, []byte) {
		return f.cpCall(http.MethodPut, path, f.adminTok, map[string]any{"actions": actions})
	}
	activeActions := func(id string) (n int) {
		f.query([]any{&n}, "SELECT count(*) FROM patrol_checkpoint_actions WHERE checkpoint_id = $1 AND active", id)
		return n
	}

	t.Run("actions with an id are updated, without one created, and missing ones deactivated, never deleted", func(t *testing.T) {
		gembok, lampu := c.Actions[0], c.Actions[1]
		code, got, raw := put([]map[string]any{
			{"action": "Cek CCTV", "min_photos": 1}, // new, first
			{"id": gembok.ID, "action": "Periksa gembok dan rantai", "min_photos": 0, "max_photos": 4},
		})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Len(t, got.Actions, 2)
		require.Equal(t, "Cek CCTV", got.Actions[0].Action)
		require.NotEqual(t, gembok.ID, got.Actions[0].ID)
		require.Equal(t, gembok.ID, got.Actions[1].ID, "an action with an id is the same one, edited")
		require.Equal(t, "Periksa gembok dan rantai", got.Actions[1].Action)
		require.Equal(t, 4, got.Actions[1].MaxPhotos)
		require.False(t, got.Actions[1].PhotoRequired)
		require.Nil(t, got.Actions[1].Instruction, "what the body leaves out is cleared: it replaces the action")
		require.Equal(t, 0, got.Actions[0].SortOrder)
		require.Equal(t, 1, got.Actions[1].SortOrder, "the order sent is the order shown")

		_, list, _ := f.cpList(checkpointsPath+"?q=gerbang", f.adminTok)
		require.Len(t, list[0].Actions, 2, "the list shows the same checklist")

		var total int
		f.query([]any{&total}, "SELECT count(*) FROM patrol_checkpoint_actions WHERE checkpoint_id = $1", c.ID)
		require.Equal(t, 4, total, "the two that were left out are still rows")
		require.Equal(t, 2, activeActions(c.ID))

		// An action that was deactivated cannot be named again: to bring one back, send it without an id.
		code, _, raw = put([]map[string]any{{"id": lampu.ID, "action": "Periksa lampu"}})
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		reason, fields := problem(t, raw)
		require.Equal(t, "CHECKPOINT_ACTION_INVALID", reason)
		require.Equal(t, []string{"actions[0].id"}, fields)
	})

	t.Run("an empty list clears the checklist", func(t *testing.T) {
		code, got, raw := put([]any{})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.NotNil(t, got.Actions)
		require.Empty(t, got.Actions)
		require.Zero(t, activeActions(c.ID))
	})

	t.Run("bad combinations are refused, with the reason and every bad field named", func(t *testing.T) {
		long := strings.Repeat("a", 301)
		for name, tc := range map[string]struct {
			actions []map[string]any
			fields  []string
		}{
			"a minimum above the maximum": {[]map[string]any{{"action": "A", "min_photos": 3, "max_photos": 2}}, []string{"actions[0].min_photos"}},
			"more than five photos":       {[]map[string]any{{"action": "A", "max_photos": 6}}, []string{"actions[0].max_photos"}},
			"a minimum above five":        {[]map[string]any{{"action": "A", "min_photos": 6, "max_photos": 6}}, []string{"actions[0].min_photos"}},
			"a negative minimum":          {[]map[string]any{{"action": "A", "min_photos": -1}}, []string{"actions[0].min_photos"}},
			"a minimum that needs a slot": {[]map[string]any{{"action": "A", "min_photos": 1, "max_photos": 0}}, []string{"actions[0].min_photos"}},
			"a blank name":                {[]map[string]any{{"action": "  "}}, []string{"actions[0].action"}},
			"a name that is too long":     {[]map[string]any{{"action": long}}, []string{"actions[0].action"}},
			"an instruction too long":     {[]map[string]any{{"action": "A", "instruction": strings.Repeat("a", 1001)}}, []string{"actions[0].instruction"}},
			"an id that is not a uuid":    {[]map[string]any{{"id": "abc", "action": "A"}}, []string{"actions[0].id"}},
			"an id of another checkpoint": {[]map[string]any{{"id": other.Actions[0].ID, "action": "A"}}, []string{"actions[0].id"}},
			"the same id twice":           {[]map[string]any{{"id": other.Actions[0].ID, "action": "A"}, {"id": other.Actions[0].ID, "action": "B"}}, []string{"actions[1].id"}},
			"every bad action is named":   {[]map[string]any{{"action": "A"}, {"action": "B", "max_photos": 9}, {"action": ""}}, []string{"actions[1].max_photos", "actions[2].action"}},
			"more than thirty actions":    {manyActions(31), []string{"actions"}},
		} {
			code, _, raw := put(tc.actions)
			require.Equal(t, http.StatusUnprocessableEntity, code, name+": "+string(raw))
			reason, fields := problem(t, raw)
			require.Equal(t, "CHECKPOINT_ACTION_INVALID", reason, name)
			require.Equal(t, tc.fields, fields, name)
		}
		code, got, raw := put(manyActions(30))
		require.Equal(t, http.StatusOK, code, "thirty is allowed: "+string(raw))
		require.Len(t, got.Actions, 30)
	})

	t.Run("a refused replace changes nothing", func(t *testing.T) {
		before := activeActions(c.ID)
		code, _, _ := put([]map[string]any{{"action": "Baru"}, {"action": "Salah", "max_photos": 9}})
		require.Equal(t, http.StatusUnprocessableEntity, code)
		require.Equal(t, before, activeActions(c.ID))
	})

	t.Run("another checkpoint's checklist is untouched", func(t *testing.T) {
		require.Equal(t, 1, activeActions(other.ID))
	})
}

func manyActions(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := range n {
		out = append(out, map[string]any{"action": "Aksi " + itoa(int64(i))})
	}
	return out
}

func TestPatrolCheckpointQR(t *testing.T) {
	f := newFlow(t)
	c := f.newCheckpoint("1K-IBMI-S", "Gerbang Utama", nil)
	qr := func(query string) (int, http.Header, []byte) {
		return f.doHeader(http.MethodGet, checkpointPath(c.ID)+"/qr"+query, f.adminTok)
	}

	t.Run("the default is a PNG named after the checkpoint, and nothing may keep a copy of it", func(t *testing.T) {
		code, header, body := qr("")
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "image/png", header.Get("Content-Type"))
		require.Contains(t, header.Get("Content-Disposition"), "qr-checkpoint-gerbang-utama.png")
		require.Equal(t, "private, no-store", header.Get("Cache-Control"))
		require.True(t, strings.HasPrefix(string(body), "\x89PNG"))
	})

	t.Run("an SVG is offered too, and an unknown format is refused", func(t *testing.T) {
		code, header, body := qr("?format=svg")
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "image/svg+xml", header.Get("Content-Type"))
		require.Contains(t, header.Get("Content-Disposition"), ".svg")
		require.True(t, strings.HasPrefix(string(body), `<?xml`) && strings.Contains(string(body), "<svg"))

		code, _, body = qr("?format=pdf")
		require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
		_, fields := problem(t, body)
		require.Equal(t, []string{"format"}, fields)
	})

	t.Run("rotating the secret changes the QR", func(t *testing.T) {
		var secret string
		f.query([]any{&secret}, "SELECT qr_secret FROM patrol_checkpoints WHERE id = $1", c.ID)
		require.Len(t, secret, 22)
		_, _, before := qr("?format=svg")

		code, rotated, raw := f.cpCall(http.MethodPost, checkpointPath(c.ID)+"/rotate-qr", f.adminTok, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		require.Equal(t, c.ID, rotated.ID)
		require.NotEqual(t, c.QrRotatedAt, rotated.QrRotatedAt, "it says when the sticker last changed")

		var after string
		f.query([]any{&after}, "SELECT qr_secret FROM patrol_checkpoints WHERE id = $1", c.ID)
		require.Len(t, after, 22)
		require.NotEqual(t, secret, after)
		_, _, changed := qr("?format=svg")
		require.NotEqual(t, before, changed, "the printed code is different")

		var logged int
		f.query([]any{&logged}, "SELECT count(*) FROM audit_logs WHERE action = 'patrol.checkpoint.rotate_qr' AND entity_code = $1", c.ID)
		require.Equal(t, 1, logged)
	})

	t.Run("the secret is in the image only: not in any answer, not in the audit log", func(t *testing.T) {
		var secret string
		f.query([]any{&secret}, "SELECT qr_secret FROM patrol_checkpoints WHERE id = $1", c.ID)
		_, list := f.do(http.MethodGet, checkpointsPath, f.adminTok, nil, nil)
		_, one := f.do(http.MethodGet, checkpointPath(c.ID), f.adminTok, nil, nil)
		_, rotated := f.do(http.MethodPost, checkpointPath(c.ID)+"/rotate-qr", f.adminTok, nil, nil)
		for _, body := range [][]byte{list, one, rotated} {
			require.NotContains(t, string(body), "qr_secret")
		}
		f.query([]any{&secret}, "SELECT qr_secret FROM patrol_checkpoints WHERE id = $1", c.ID)
		for _, body := range [][]byte{list, one, rotated} {
			require.NotContains(t, string(body), secret)
			require.NotContains(t, string(body), "PERSIST-CP")
		}
		var leaked int
		f.query([]any{&leaked}, "SELECT count(*) FROM audit_logs WHERE metadata::text LIKE '%' || $1 || '%'", secret)
		require.Zero(t, leaked)
	})
}

func TestPatrolCheckpointDeleteClosesTheGap(t *testing.T) {
	f := newFlow(t)
	a, b, c := f.newCheckpoint("1K-IBMI-S", "A", nil), f.newCheckpoint("1K-IBMI-S", "B", nil), f.newCheckpoint("1K-IBMI-S", "C", nil)
	d := f.newCheckpoint("1K-IBMI-S", "D", map[string]any{"is_active": false}) // inactive, but it keeps its place
	x, y := f.addZone("1K-IBMI-S", "Zona X"), f.addZone("1K-IBMI-S", "Zona Y")
	for i, id := range []string{a.ID, b.ID, c.ID, d.ID} {
		require.NoError(t, f.link("1K-IBMI-S", x, id, i+1))
	}
	require.NoError(t, f.link("1K-IBMI-S", y, c.ID, 1))
	require.NoError(t, f.link("1K-IBMI-S", y, a.ID, 2))

	// positions lists "<name><position>" for the checkpoints a zone holds, in position order.
	positions := func(zone int64) (out []string) {
		for _, p := range []struct{ name, id string }{{"A", a.ID}, {"B", b.ID}, {"C", c.ID}, {"D", d.ID}} {
			var n int
			f.query([]any{&n}, "SELECT coalesce((SELECT sort_order FROM patrol_zone_checkpoints WHERE zone_id = $1 AND checkpoint_id = $2), 0)", zone, p.id)
			if n > 0 {
				out = append(out, p.name+itoa(int64(n)))
			}
		}
		slices.SortFunc(out, func(x, y string) int { return strings.Compare(x[1:], y[1:]) })
		return out
	}

	code, raw := f.do(http.MethodDelete, checkpointPath(a.ID), f.adminTok, nil, nil)
	require.Equal(t, http.StatusOK, code, string(raw))

	t.Run("the checkpoint is gone, and the others in its zones are renumbered without a gap", func(t *testing.T) {
		require.Equal(t, []string{"B1", "C2", "D3"}, positions(x), "the inactive one kept its place")
		require.Equal(t, []string{"C1"}, positions(y))
		code, _, raw := f.cpCall(http.MethodGet, checkpointPath(a.ID), f.adminTok, nil)
		require.Equal(t, http.StatusNotFound, code, string(raw))
		_, list, _ := f.cpList(checkpointsPath+"?zone_id="+itoa(x), f.adminTok)
		require.Len(t, list, 3)
		require.Equal(t, []checkpointZoneOut{{ID: x, Name: "Zona X", Type: "PATROL", SortOrder: 2}}, onlyZone(list, c.ID, x))
	})

	t.Run("the row stays, inactive, and its name can be used again", func(t *testing.T) {
		var active bool
		var deleted bool
		f.query([]any{&active, &deleted}, "SELECT active, deleted_at IS NOT NULL FROM patrol_checkpoints WHERE id = $1", a.ID)
		require.False(t, active)
		require.True(t, deleted)
		f.newCheckpoint("1K-IBMI-S", "A", nil)
		code, _ := f.do(http.MethodDelete, checkpointPath(a.ID), f.adminTok, nil, nil)
		require.Equal(t, http.StatusNotFound, code, "it cannot be deleted twice")
	})

	t.Run("the deletion is audited", func(t *testing.T) {
		var n int
		f.query([]any{&n}, "SELECT count(*) FROM audit_logs WHERE action = 'patrol.checkpoint.delete' AND entity_code = $1", a.ID)
		require.Equal(t, 1, n)
	})
}

func onlyZone(list []checkpointOut, checkpoint string, zone int64) []checkpointZoneOut {
	for _, c := range list {
		if c.ID != checkpoint {
			continue
		}
		for _, z := range c.Zones {
			if z.ID == zone {
				return []checkpointZoneOut{z}
			}
		}
	}
	return nil
}

func TestPatrolCheckpointScope(t *testing.T) {
	f := newFlow(t)
	f.exec(`INSERT INTO regions (code, description) VALUES ('OS2', 'Outsourcing 2')`)
	regional := f.addPerson(900302, "REGADMIN02", "Admin Wilayah Dua")
	f.exec("INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, 'REGION_ADMIN', 'OS2')", regional)
	regionalTok := f.token(regional, f.addSession(regional, "tok-regional"))
	home := f.addPerson(900303, "REGADMIN01", "Admin Wilayah Satu")
	f.exec("INSERT INTO web_role_assignments (personnel_id, role, region_code) VALUES ($1, 'REGION_ADMIN', 'OS1')", home)
	homeTok := f.token(home, f.addSession(home, "tok-home"))

	c := f.newCheckpoint("1K-IBMI-S", "Pos A", map[string]any{"actions": threeActions()[:1]})

	t.Run("an admin of another region sees nothing and is refused", func(t *testing.T) {
		_, list, _ := f.cpList(checkpointsPath, regionalTok)
		require.Empty(t, list)
		for _, call := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, checkpointPath(c.ID), nil},
			{http.MethodPatch, checkpointPath(c.ID), map[string]any{"name": "Dicuri"}},
			{http.MethodDelete, checkpointPath(c.ID), nil},
			{http.MethodPost, checkpointPath(c.ID) + "/rotate-qr", nil},
			{http.MethodGet, checkpointPath(c.ID) + "/qr", nil},
			{http.MethodPut, checkpointPath(c.ID) + "/actions", map[string]any{"actions": []any{}}},
			{http.MethodPost, checkpointsPath, map[string]any{"project_code": "1K-IBMI-S", "name": "Asing"}},
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
		code, _, _ := f.cpList(checkpointsPath+"?project_code=1K-IBMI-S", regionalTok)
		require.Equal(t, http.StatusForbidden, code)
		_, got, _ := f.cpCall(http.MethodGet, checkpointPath(c.ID), f.adminTok, nil)
		require.Equal(t, "Pos A", got.Name, "and nothing was changed")
		require.Len(t, got.Actions, 1)
	})

	t.Run("an admin of the checkpoint's region has full access", func(t *testing.T) {
		_, list, _ := f.cpList(checkpointsPath, homeTok)
		require.Len(t, list, 1)
		code, _, raw := f.cpCall(http.MethodPatch, checkpointPath(c.ID), homeTok, map[string]any{"name": "Pos A2"})
		require.Equal(t, http.StatusOK, code, string(raw))
	})

	t.Run("another organization's checkpoint is not found", func(t *testing.T) {
		other := testkit.NewOrg(t)
		var foreign string
		require.NoError(t, database.WithTenantTx(t.Context(), testkit.Pool(t), other, func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), flowWorldSQL); err != nil {
				return err
			}
			return tx.QueryRow(t.Context(),
				`INSERT INTO patrol_checkpoints (project_code, name, qr_secret) VALUES ('1K-IBMI-S', 'Milik Asing', $1) RETURNING id::text`,
				checkpointSecret).Scan(&foreign)
		}))
		code, _, raw := f.cpCall(http.MethodGet, checkpointPath(foreign), f.adminTok, nil)
		require.Equal(t, http.StatusNotFound, code, string(raw))
		code, _, _ = f.cpCall(http.MethodPatch, checkpointPath(foreign), f.adminTok, map[string]any{"name": "Dicuri"})
		require.Equal(t, http.StatusNotFound, code)
		code, _, _ = f.cpCall(http.MethodGet, checkpointPath(foreign)+"/qr", f.adminTok, nil)
		require.Equal(t, http.StatusNotFound, code)
		_, list, _ := f.cpList(checkpointsPath, f.adminTok)
		for _, got := range list {
			require.NotEqual(t, "Milik Asing", got.Name)
		}
	})
}
