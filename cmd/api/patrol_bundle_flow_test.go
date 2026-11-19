package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Phase 10.18 (docs/plan/10.3-patrol-mobile-feedback.md): the bundled field list with its ETag, and the grace
// period of a rotated QR.

// getWithHeaders is a GET that sends extra request headers and returns the response's headers too.
func (f *flow) getWithHeaders(path, token string, headers map[string]string) (int, http.Header, []byte) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, f.srv.URL+path, nil)
	require.NoError(f.t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(f.t, err)
	return res.StatusCode, res.Header, body
}

// dataOf decodes the data of a success envelope into maps, to compare answers as the app would see them.
func dataOf(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &env), string(raw))
	var list []map[string]any
	if err := json.Unmarshal(env.Data, &list); err != nil {
		var one map[string]any
		require.NoError(t, json.Unmarshal(env.Data, &one), string(raw))
		list = []map[string]any{one}
	}
	return list
}

func TestPatrolBundledFieldList(t *testing.T) {
	f := newFlow(t)

	// Two zones that share a checkpoint, one with a checklist, and a zone with nothing in it.
	a := f.fencedZone([]map[string]any{{"action": "Periksa gembok", "min_photos": 0, "max_photos": 1, "is_note_required": true}}, nil)
	b := f.fencedRound(2, nil)
	shared := a.all[0]
	code, _, raw := f.assign(b.ID, b.all[0].ID, shared.ID)
	require.Equal(t, http.StatusOK, code, string(raw))
	empty := f.newPlainZone("1K-IBMI-S", "Pos Kosong", nil)

	// The caller has walked a little: the first checkpoint of b, twice.
	for i := range 2 {
		cp := b.all[0]
		body := bodyAt(cp, cp.payload, time.Now().Add(-time.Duration(i)*31*time.Second))
		f.mustScan(f.personTok, b, body)
	}

	code, _, raw = f.getWithHeaders("/api/v1/zones?include=checkpoints", f.personTok, nil)
	require.Equal(t, http.StatusOK, code, string(raw))
	zones := dataOf(t, raw)

	t.Run("each zone reads exactly as its own detail does", func(t *testing.T) {
		seen := 0
		for _, z := range zones {
			id := int64(z["id"].(float64))
			_, detail := f.do(http.MethodGet, "/api/v1/zones/"+strconv.FormatInt(id, 10), f.personTok, nil, nil)
			want := dataOf(t, detail)[0]
			require.Equal(t, want["checkpoints"], z["checkpoints"], "zone %d", id)
			require.Equal(t, want["next_checkpoint_id"], z["next_checkpoint_id"], "zone %d", id)
			require.Equal(t, want["checkpoint_count"], z["checkpoint_count"], "zone %d", id)
			seen++
		}
		require.GreaterOrEqual(t, seen, 3, "a, b and the empty zone")
	})

	t.Run("it has the checklist, the walk and the next checkpoint", func(t *testing.T) {
		var got map[string]any
		for _, z := range zones {
			if int64(z["id"].(float64)) == b.ID {
				got = z
			}
		}
		require.NotNil(t, got)
		cps := got["checkpoints"].([]any)
		require.Len(t, cps, 2)
		first := cps[0].(map[string]any)
		require.Equal(t, b.all[0].ID, first["id"])
		require.EqualValues(t, 2, first["my_scans"])
		require.Equal(t, true, first["scannable"])
		require.Equal(t, shared.ID, cps[1].(map[string]any)["id"], "the shared checkpoint has its own place in b")
		require.Equal(t, shared.ID, got["next_checkpoint_id"])

		var inA map[string]any
		for _, z := range zones {
			if int64(z["id"].(float64)) == a.ID {
				inA = z
			}
		}
		sharedInA := inA["checkpoints"].([]any)[0].(map[string]any)
		require.Equal(t, shared.ID, sharedInA["id"])
		require.NotEmpty(t, sharedInA["actions"], "the checklist is in the bundle")
	})

	t.Run("a zone with no checkpoint has an empty list", func(t *testing.T) {
		for _, z := range zones {
			if int64(z["id"].(float64)) == empty.ID {
				require.Equal(t, []any{}, z["checkpoints"])
				return
			}
		}
		t.Fatal("the empty zone is missing")
	})

	t.Run("the plain list has no checkpoints, and no ETag", func(t *testing.T) {
		_, hdr, plain := f.getWithHeaders("/api/v1/zones", f.personTok, nil)
		require.Empty(t, hdr.Get("ETag"))
		for _, z := range dataOf(t, plain) {
			require.NotContains(t, z, "checkpoints")
		}
	})

	t.Run("include must be checkpoints", func(t *testing.T) {
		code, _, raw := f.getWithHeaders("/api/v1/zones?include=everything", f.personTok, nil)
		require.Equal(t, http.StatusUnprocessableEntity, code, string(raw))
		_, fields := problem(t, raw)
		require.Equal(t, []string{"include"}, fields)
	})

	t.Run("no secret is in the bundle", func(t *testing.T) {
		var secret string
		f.query([]any{&secret}, "SELECT qr_secret FROM patrol_checkpoints WHERE id = $1", shared.ID)
		require.NotContains(t, string(raw), secret)
		require.NotContains(t, string(raw), "qr_secret")
		require.NotContains(t, string(raw), "previous")
		require.NotContains(t, string(raw), "PERSIST-CP")
	})
}

func TestPatrolBundleETag(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(2, nil)
	const path = "/api/v1/zones?include=checkpoints"

	code, hdr, raw := f.getWithHeaders(path, f.personTok, nil)
	require.Equal(t, http.StatusOK, code, string(raw))
	tag := hdr.Get("ETag")
	require.NotEmpty(t, tag)
	require.Contains(t, hdr.Get("Cache-Control"), "no-cache")

	t.Run("the same read with its tag is 304 with no body", func(t *testing.T) {
		code, hdr, body := f.getWithHeaders(path, f.personTok, map[string]string{"If-None-Match": tag})
		require.Equal(t, http.StatusNotModified, code)
		require.Empty(t, body)
		require.Equal(t, tag, hdr.Get("ETag"))

		code, _, _ = f.getWithHeaders(path, f.personTok, map[string]string{"If-None-Match": `W/` + tag})
		require.Equal(t, http.StatusNotModified, code, "a weak tag compares the same on a GET")
		code, _, _ = f.getWithHeaders(path, f.personTok, map[string]string{"If-None-Match": `"other", ` + tag})
		require.Equal(t, http.StatusNotModified, code, "one of a list")
		code, _, _ = f.getWithHeaders(path, f.personTok, map[string]string{"If-None-Match": `"other"`})
		require.Equal(t, http.StatusOK, code)
	})

	t.Run("a scan changes what the caller sees, so the tag", func(t *testing.T) {
		f.mustScan(f.personTok, z, scanBodyOf(z.all[0], z.all[0].payload))
		code, hdr, raw := f.getWithHeaders(path, f.personTok, map[string]string{"If-None-Match": tag})
		require.Equal(t, http.StatusOK, code, string(raw))
		require.NotEqual(t, tag, hdr.Get("ETag"))
	})

	t.Run("another person's view has its own tag", func(t *testing.T) {
		_, danruTok := f.danru()
		_, other, _ := f.getWithHeaders(path, danruTok, nil)
		require.NotEmpty(t, other.Get("ETag"))
	})
}

func TestPatrolQRRotationGrace(t *testing.T) {
	f := newFlow(t)
	z := f.fencedRound(2, nil)
	cp := z.all[0]
	now := time.Now()

	oldPayload := cp.payload
	code, _, raw := f.cpCall(http.MethodPost, checkpointPath(cp.ID)+"/rotate-qr", f.adminTok, nil)
	require.Equal(t, http.StatusOK, code, string(raw))
	newPayload := f.checkpointPayload(cp.ID)
	require.NotEqual(t, oldPayload, newPayload)

	t.Run("a scan made before the rotation with the old sticker is accepted when it syncs", func(_ *testing.T) {
		body := bodyAt(cp, oldPayload, now.Add(-10*time.Minute))
		body["submitted_offline"] = true
		f.mustScan(f.personTok, z, body)
	})

	t.Run("a scan made after the rotation must carry the new one", func(t *testing.T) {
		code, _, raw := f.scan(f.personTok, z.ID, bodyAt(cp, oldPayload, now.Add(2*time.Minute)))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_QR_INVALID")
		f.mustScan(f.personTok, z, bodyAt(cp, newPayload, now.Add(2*time.Minute)))
	})

	t.Run("check-in and check-out do not get the grace", func(t *testing.T) {
		code, raw := f.do(http.MethodPost, "/api/v1/zones/"+strconv.FormatInt(z.ID, 10)+"/check-in", f.personTok,
			jsonBody(t, map[string]any{
				"checkpoint_id": cp.ID, "qr_payload": oldPayload, "latitude": zoneLat, "longitude": zoneLng, "accuracy_meters": 5,
			}), map[string]string{"Idempotency-Key": uuid.NewString()})
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_QR_INVALID")
	})

	t.Run("only one generation back: after a second rotation the first secret is dead", func(t *testing.T) {
		code, _, raw := f.cpCall(http.MethodPost, checkpointPath(cp.ID)+"/rotate-qr", f.adminTok, nil)
		require.Equal(t, http.StatusOK, code, string(raw))
		// The secret of the first rotation is the previous one now: still good for a scan before the second.
		f.mustScan(f.personTok, z, bodyAt(cp, newPayload, now.Add(-20*time.Minute)))
		// The original secret is two back.
		code, _, raw = f.scan(f.personTok, z.ID, bodyAt(cp, oldPayload, now.Add(-30*time.Minute)))
		reasonIs(t, code, raw, http.StatusUnprocessableEntity, "CHECKPOINT_QR_INVALID")
	})

	t.Run("the old secret is in no answer and not in the audit log", func(t *testing.T) {
		var previous string
		f.query([]any{&previous}, "SELECT previous_qr_secret FROM patrol_checkpoints WHERE id = $1", cp.ID)
		require.Len(t, previous, 22)
		_, list := f.do(http.MethodGet, checkpointsPath, f.adminTok, nil, nil)
		_, one := f.do(http.MethodGet, checkpointPath(cp.ID), f.adminTok, nil, nil)
		_, bundle := f.do(http.MethodGet, "/api/v1/zones?include=checkpoints", f.personTok, nil, nil)
		for _, body := range [][]byte{list, one, bundle} {
			require.NotContains(t, string(body), previous)
			require.NotContains(t, string(body), "previous_qr_secret")
		}
		require.Zero(t, countOf(t, f, "SELECT count(*) FROM audit_logs WHERE metadata::text LIKE '%' || $1 || '%'", previous))
	})

	t.Run("a checkpoint never rotated has no previous secret", func(t *testing.T) {
		fresh := f.fencedZone(nil, nil)
		require.Zero(t, countOf(t, f, "SELECT count(*) FROM patrol_checkpoints WHERE id = $1 AND previous_qr_secret IS NOT NULL", fresh.checkpoint.ID))
	})
}
