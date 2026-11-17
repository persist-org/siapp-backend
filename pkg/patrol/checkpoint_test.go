package patrol

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/apperr"
)

func TestCheckpointPayload(t *testing.T) {
	require.Equal(t,
		"PERSIST-CP:1:900101:3f2504e0-4f89-41d3-9a0c-0305e82c3301:abcdefghijklmnopqrstuv",
		CheckpointPayload(900101, "3f2504e0-4f89-41d3-9a0c-0305e82c3301", "abcdefghijklmnopqrstuv"))
}

func TestQRFileNames(t *testing.T) {
	require.Equal(t, "qr-checkpoint-gerbang-utama.png", checkpointFileName("  Gerbang  Utama! ", "png"))
	require.Equal(t, "qr-checkpoint-checkpoint.svg", checkpointFileName("???", "svg"), "nothing usable falls back to a plain name")
	require.LessOrEqual(t, len(checkpointFileName(strings.Repeat("a", 200), "png")), len("qr-checkpoint-")+60+len(".png"))
}

func TestParseUUID(t *testing.T) {
	id, ok := parseUUID("3f2504e0-4f89-41d3-9a0c-0305e82c3301")
	require.True(t, ok)
	require.Equal(t, "3f2504e0-4f89-41d3-9a0c-0305e82c3301", uuidString(id))
	for _, bad := range []string{"", "abc", "123", "3f2504e0-4f89-41d3-9a0c", "not-a-uuid-at-all-not-a-uuid-at-all!"} {
		_, ok := parseUUID(bad)
		require.False(t, ok, bad)
	}
}

func TestCheckpointGeofenceRules(t *testing.T) {
	lat, lng := -6.2, 106.8
	t.Run("a point gets the checkpoint's own default, smaller than a zone's", func(t *testing.T) {
		g, err := geofenceWith(&lat, &lng, nil, false, checkpointFence)
		require.NoError(t, err)
		require.Equal(t, DefaultCheckpointRadiusMeters, *g.radius)
		require.Equal(t, 30, *g.radius)
	})
	t.Run("the largest radius is a kilometre", func(t *testing.T) {
		_, err := geofenceWith(&lat, &lng, ptr(MaxCheckpointRadiusMeters), false, checkpointFence)
		require.NoError(t, err)
		_, err = geofenceWith(&lat, &lng, ptr(MaxCheckpointRadiusMeters+1), false, checkpointFence)
		require.Equal(t, []string{"radius_meters"}, fieldsOf(t, err))
		g, err := geofence(&lat, &lng, ptr(MaxRadiusMeters), false)
		require.NoError(t, err, "a zone still allows its own, larger radius")
		require.Equal(t, MaxRadiusMeters, *g.radius)
	})
	t.Run("a message names the place", func(t *testing.T) {
		_, err := geofenceWith(&lat, &lng, nil, true, checkpointFence)
		var app *apperr.Error
		require.ErrorAs(t, err, &app)
		require.Contains(t, app.Fields[0].Message, "Checkpoint")
		_, err = geofence(&lat, &lng, nil, true)
		require.ErrorAs(t, err, &app)
		require.Contains(t, app.Fields[0].Message, "Zona")
	})
}

func TestParseActions(t *testing.T) {
	id := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	other := "9b2d5c0a-4f89-41d3-9a0c-0305e82c3302"
	t.Run("defaults", func(t *testing.T) {
		specs, err := parseActions([]ActionInput{{Action: "  Periksa gembok  "}})
		require.NoError(t, err)
		s := specs[0]
		require.Equal(t, "Periksa gembok", s.action, "trimmed")
		require.Equal(t, int16(0), s.minPhotos, "the photo is optional")
		require.Equal(t, int16(1), s.maxPhotos)
		require.False(t, s.isNoteRequired)
		require.Nil(t, s.instruction)
		require.False(t, s.id.Valid, "no id means a new action")
	})
	t.Run("what was sent is kept", func(t *testing.T) {
		specs, err := parseActions([]ActionInput{{
			ID: ptr(id), Action: "Cek panel", Instruction: ptr(" buka pintu "), MinPhotos: ptr(2), MaxPhotos: ptr(4), IsNoteRequired: ptr(true),
		}})
		require.NoError(t, err)
		s := specs[0]
		require.Equal(t, id, uuidString(s.id))
		require.Equal(t, "buka pintu", *s.instruction)
		require.Equal(t, int16(2), s.minPhotos)
		require.Equal(t, int16(4), s.maxPhotos)
		require.True(t, s.isNoteRequired)
	})
	t.Run("max_photos follows min_photos when it is left out", func(t *testing.T) {
		for min, want := range map[int]int16{0: 1, 1: 1, 3: 3, 5: 5} {
			specs, err := parseActions([]ActionInput{{Action: "A", MinPhotos: ptr(min)}})
			require.NoError(t, err)
			require.Equal(t, want, specs[0].maxPhotos, "min_photos %d", min)
		}
		specs, err := parseActions([]ActionInput{{Action: "A", MaxPhotos: ptr(0)}})
		require.NoError(t, err)
		require.Equal(t, int16(0), specs[0].maxPhotos, "an explicit 0 is a checklist action with no photo slot, not a default")
	})
	t.Run("combinations", func(t *testing.T) {
		for name, tc := range map[string]struct {
			in   []ActionInput
			want []string // the bad fields; nil means accepted
		}{
			"photos 0 to 0":            {[]ActionInput{{Action: "A", MinPhotos: ptr(0), MaxPhotos: ptr(0)}}, nil},
			"photos 5 to 5":            {[]ActionInput{{Action: "A", MinPhotos: ptr(5), MaxPhotos: ptr(5)}}, nil},
			"min above max":            {[]ActionInput{{Action: "A", MinPhotos: ptr(3), MaxPhotos: ptr(2)}}, []string{"actions[0].min_photos"}},
			"min needs a slot":         {[]ActionInput{{Action: "A", MinPhotos: ptr(1), MaxPhotos: ptr(0)}}, []string{"actions[0].min_photos"}},
			"max above five":           {[]ActionInput{{Action: "A", MaxPhotos: ptr(6)}}, []string{"actions[0].max_photos"}},
			"a negative max":           {[]ActionInput{{Action: "A", MaxPhotos: ptr(-1)}}, []string{"actions[0].max_photos"}},
			"min above five":           {[]ActionInput{{Action: "A", MinPhotos: ptr(6)}}, []string{"actions[0].min_photos"}},
			"a blank action":           {[]ActionInput{{Action: "   "}}, []string{"actions[0].action"}},
			"a long action":            {[]ActionInput{{Action: strings.Repeat("é", 301)}}, []string{"actions[0].action"}},
			"a long action in letters": {[]ActionInput{{Action: strings.Repeat("é", 300)}}, nil},
			"a long instruction":       {[]ActionInput{{Action: "A", Instruction: ptr(strings.Repeat("a", 1001))}}, []string{"actions[0].instruction"}},
			"a bad id":                 {[]ActionInput{{ID: ptr("nope"), Action: "A"}}, []string{"actions[0].id"}},
			"the same id twice":        {[]ActionInput{{ID: ptr(id), Action: "A"}, {ID: ptr(id), Action: "B"}}, []string{"actions[1].id"}},
			"two different ids":        {[]ActionInput{{ID: ptr(id), Action: "A"}, {ID: ptr(other), Action: "B"}}, nil},
			"every bad one is named": {[]ActionInput{
				{Action: "A"}, {Action: "B", MaxPhotos: ptr(9)}, {Action: ""}, {ID: ptr("x"), Action: "D"},
			}, []string{"actions[1].max_photos", "actions[2].action", "actions[3].id"}},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := parseActions(tc.in)
				if tc.want == nil {
					require.NoError(t, err)
					return
				}
				require.Equal(t, tc.want, fieldsOf(t, err))
				var app *apperr.Error
				require.ErrorAs(t, err, &app)
				require.Equal(t, ReasonCheckpointActionInvalid, app.Reason)
			})
		}
	})
	t.Run("the most actions", func(t *testing.T) {
		many := make([]ActionInput, MaxCheckpointActions)
		for i := range many {
			many[i] = ActionInput{Action: "A"}
		}
		_, err := parseActions(many)
		require.NoError(t, err)
		_, err = parseActions(append(many, ActionInput{Action: "A"}))
		require.Equal(t, []string{"actions"}, fieldsOf(t, err))
	})
}
