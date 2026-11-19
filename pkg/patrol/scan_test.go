package patrol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
)

func TestInsideZone(t *testing.T) {
	// radius 50 m: the tolerance is min(accuracy, 30), so the edge is 50 + tolerance.
	for name, tc := range map[string]struct {
		distance, radius, accuracy float64
		want                       bool
	}{
		"in the middle":                     {0, 50, 5, true},
		"exactly on the radius":             {50, 50, 0, true},
		"a hair outside, perfect fix":       {50.01, 50, 0, false},
		"inside by the accuracy":            {60, 50, 10, true},
		"exactly on radius plus accuracy":   {65, 50, 15, true},
		"just past radius plus accuracy":    {65.01, 50, 15, false},
		"the tolerance is capped at 30":     {80, 50, 500, true},
		"just past the cap":                 {80.01, 50, 500, false},
		"a huge accuracy does not unlock":   {300, 50, 5000, false},
		"accuracy exactly at the cap":       {80, 50, 30, true},
		"a negative accuracy gives nothing": {50.5, 50, -20, false},
		"far away":                          {10000, 50, 5, false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, insideZone(tc.distance, tc.radius, tc.accuracy))
		})
	}
}

func TestDistanceTo(t *testing.T) {
	require.InDelta(t, 0, distanceTo(-6.2, 106.8, -6.2, 106.8), 0.001)
	// 0.001 degree of latitude is about 111 m.
	require.InDelta(t, 111.2, distanceTo(-6.2, 106.8, -6.201, 106.8), 1)
}

func TestVerifyCheckpointPayload(t *testing.T) {
	const (
		secret = "abcdefghijklmnopqrstuv"
		id     = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
		other  = "9b2d5c0a-4f89-41d3-9a0c-0305e82c3302"
	)
	good := CheckpointPayload(900101, id, secret)
	for name, tc := range map[string]struct {
		payload string
		want    bool
	}{
		"the checkpoint's own code":    {good, true},
		"its id spelled in capitals":   {CheckpointPayload(900101, strings.ToUpper(id), secret), true},
		"another checkpoint":           {CheckpointPayload(900101, other, secret), false},
		"another organization":         {CheckpointPayload(900102, id, secret), false},
		"an old secret":                {CheckpointPayload(900101, id, "ZZZZZZZZZZZZZZZZZZZZZZ"), false},
		"a prefix of the secret":       {CheckpointPayload(900101, id, secret[:10]), false},
		"nothing":                      {"", false},
		"another format":               {"https://example.com/checkpoint/42", false},
		"a zone's old sticker":         {"PERSIST-ZONE:1:900101:42:" + secret, false},
		"another version":              {"PERSIST-CP:2:900101:" + id + ":" + secret, false},
		"too many parts":               {good + ":x", false},
		"an organization not a number": {"PERSIST-CP:1:a:" + id + ":" + secret, false},
		"a zero organization":          {"PERSIST-CP:1:0:" + id + ":" + secret, false},
		"an id that is not a uuid":     {"PERSIST-CP:1:900101:42:" + secret, false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, verifyCheckpointPayload(tc.payload, 900101, id, secret))
		})
	}
}

// The checkpoint actions the tests below judge, created long ago and active unless a test says otherwise.
var (
	scanAt   = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	longAgo  = pgtype.Timestamptz{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	actionID = func(n byte) pgtype.UUID { return pgtype.UUID{Bytes: uuid.UUID{15: n}, Valid: true} }
)

func action(n byte, edit func(*db.PatrolCheckpointAction)) db.PatrolCheckpointAction {
	a := db.PatrolCheckpointAction{
		ID: actionID(n), SortOrder: int32(n), Action: "Aksi " + string(rune('A'+n-1)), //nolint:gosec // a small test number
		MinPhotos: 0, MaxPhotos: 1, Active: true, CreatedAt: longAgo,
	}
	if edit != nil {
		edit(&a)
	}
	return a
}

func answer(n byte, edit func(*ActionAnswerInput)) ActionAnswerInput {
	a := ActionAnswerInput{ActionID: uuidString(actionID(n))}
	if edit != nil {
		edit(&a)
	}
	return a
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var app *apperr.Error
	require.ErrorAs(t, err, &app)
	return app.Reason
}

func TestCheckActionsRejections(t *testing.T) {
	photos := func(lowest, highest int16) func(*db.PatrolCheckpointAction) {
		return func(a *db.PatrolCheckpointAction) { a.MinPhotos, a.MaxPhotos = lowest, highest }
	}
	noteRequired := func(a *db.PatrolCheckpointAction) { a.IsNoteRequired = true }
	withMedia := func(ids ...int64) func(*ActionAnswerInput) {
		return func(a *ActionAnswerInput) { a.MediaIDs = ids }
	}
	withNote := func(s string) func(*ActionAnswerInput) { return func(a *ActionAnswerInput) { a.Note = &s } }
	deactivatedBefore := func(a *db.PatrolCheckpointAction) {
		a.Active, a.DeactivatedAt = false, pgtype.Timestamptz{Time: scanAt.Add(-time.Hour), Valid: true}
	}

	for name, tc := range map[string]struct {
		actions []db.PatrolCheckpointAction
		answers []ActionAnswerInput
		reason  string // "" means accepted
	}{
		"nothing to answer, nothing answered": {nil, nil, ""},
		"an action with no answer":            {[]db.PatrolCheckpointAction{action(1, nil)}, nil, ReasonAnswerRequired},
		"an action answered":                  {[]db.PatrolCheckpointAction{action(1, nil)}, []ActionAnswerInput{answer(1, nil)}, ""},
		"one of two with no answer":           {[]db.PatrolCheckpointAction{action(1, nil), action(2, nil)}, []ActionAnswerInput{answer(1, nil)}, ReasonAnswerRequired},
		"an action of another checkpoint":     {[]db.PatrolCheckpointAction{action(1, nil)}, []ActionAnswerInput{answer(1, nil), answer(9, nil)}, ReasonChecklistItemInvalid},
		"an id that is not a uuid":            {[]db.PatrolCheckpointAction{action(1, nil)}, []ActionAnswerInput{{ActionID: "abc"}}, ReasonChecklistItemInvalid},
		"an action answered twice":            {[]db.PatrolCheckpointAction{action(1, nil)}, []ActionAnswerInput{answer(1, nil), answer(1, nil)}, ReasonChecklistItemInvalid},
		"an action deactivated before":        {[]db.PatrolCheckpointAction{action(1, deactivatedBefore)}, []ActionAnswerInput{answer(1, nil)}, ReasonChecklistItemInvalid},

		"the photo limit is allowed":         {[]db.PatrolCheckpointAction{action(1, photos(0, 2))}, []ActionAnswerInput{answer(1, withMedia(5, 6))}, ""},
		"one photo too many":                 {[]db.PatrolCheckpointAction{action(1, photos(0, 2))}, []ActionAnswerInput{answer(1, withMedia(5, 6, 7))}, ReasonAnswerInvalid},
		"a photo for an action with no slot": {[]db.PatrolCheckpointAction{action(1, photos(0, 0))}, []ActionAnswerInput{answer(1, withMedia(5))}, ReasonAnswerInvalid},
		"no photo where none is allowed":     {[]db.PatrolCheckpointAction{action(1, photos(0, 0))}, []ActionAnswerInput{answer(1, nil)}, ""},
		"a required photo is missing":        {[]db.PatrolCheckpointAction{action(1, photos(1, 2))}, []ActionAnswerInput{answer(1, nil)}, ReasonAttachmentRequired},
		"two required photos, one sent":      {[]db.PatrolCheckpointAction{action(1, photos(2, 3))}, []ActionAnswerInput{answer(1, withMedia(5))}, ReasonAttachmentRequired},
		"a required photo is there":          {[]db.PatrolCheckpointAction{action(1, photos(1, 2))}, []ActionAnswerInput{answer(1, withMedia(5))}, ""},
		"an optional photo may be left out":  {[]db.PatrolCheckpointAction{action(1, photos(0, 3))}, []ActionAnswerInput{answer(1, nil)}, ""},

		"a required note is missing":  {[]db.PatrolCheckpointAction{action(1, noteRequired)}, []ActionAnswerInput{answer(1, nil)}, ReasonAttachmentRequired},
		"a required note is blank":    {[]db.PatrolCheckpointAction{action(1, noteRequired)}, []ActionAnswerInput{answer(1, withNote("   "))}, ReasonAttachmentRequired},
		"a required note is there":    {[]db.PatrolCheckpointAction{action(1, noteRequired)}, []ActionAnswerInput{answer(1, withNote("aman"))}, ""},
		"a note that is not required": {[]db.PatrolCheckpointAction{action(1, nil)}, []ActionAnswerInput{answer(1, withNote("catatan"))}, ""},
		"a note that is too long":     {[]db.PatrolCheckpointAction{action(1, nil)}, []ActionAnswerInput{answer(1, withNote(strings.Repeat("é", 1001)))}, ReasonAnswerInvalid},

		"the same photo twice in one action": {[]db.PatrolCheckpointAction{action(1, photos(0, 2))}, []ActionAnswerInput{answer(1, withMedia(5, 5))}, ReasonAnswerInvalid},
		"the same photo in two actions":      {[]db.PatrolCheckpointAction{action(1, nil), action(2, nil)}, []ActionAnswerInput{answer(1, withMedia(5)), answer(2, withMedia(5))}, ReasonAnswerInvalid},
		"different photos in two actions":    {[]db.PatrolCheckpointAction{action(1, nil), action(2, nil)}, []ActionAnswerInput{answer(1, withMedia(5)), answer(2, withMedia(6))}, ""},
		"an inactive action nobody has to do": {[]db.PatrolCheckpointAction{action(1, nil), action(2, func(a *db.PatrolCheckpointAction) {
			a.Active = false
			a.DeactivatedAt = pgtype.Timestamptz{Time: scanAt.Add(time.Hour), Valid: true}
		})}, []ActionAnswerInput{answer(1, nil)}, ""},
		"an answer is judged before a missing":   {[]db.PatrolCheckpointAction{action(1, nil), action(2, nil)}, []ActionAnswerInput{answer(9, nil)}, ReasonChecklistItemInvalid},
		"the first missing action is the answer": {[]db.PatrolCheckpointAction{action(1, nil), action(2, photos(1, 1))}, []ActionAnswerInput{answer(2, nil)}, ReasonAnswerRequired},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := checkActions(tc.actions, tc.answers, scanAt)
			if tc.reason == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, tc.reason, reasonOf(t, err))
		})
	}
}

func TestCheckActionsNamesTheAction(t *testing.T) {
	_, _, err := checkActions([]db.PatrolCheckpointAction{action(3, nil)}, nil, scanAt)
	var app *apperr.Error
	require.ErrorAs(t, err, &app)
	require.Equal(t, uuidString(actionID(3)), app.Details["action_id"])
	require.Contains(t, app.Message, "Aksi C")
}

func TestCheckActionsKeepsWhatTheGuardGave(t *testing.T) {
	actions := []db.PatrolCheckpointAction{
		action(2, func(a *db.PatrolCheckpointAction) { a.MinPhotos, a.MaxPhotos = 1, 3 }),
		action(1, func(a *db.PatrolCheckpointAction) { a.IsNoteRequired = true }),
	}
	note := " rusak "
	entries, media, err := checkActions(actions, []ActionAnswerInput{
		{ActionID: uuidString(actionID(2)), MediaIDs: []int64{8, 9}},
		{ActionID: uuidString(actionID(1)), Note: &note, DeviceCondition: json.RawMessage(`{"battery_level":40}`)},
	}, scanAt)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, []int64{8, 9}, media, "the photos to attach, in the order of the checklist")

	// The entries follow the order the checklist was given in, not the order of the answers.
	require.Equal(t, actionID(2), entries[0].action.ID)
	require.Equal(t, []int64{8, 9}, entries[0].mediaIDs)
	require.Nil(t, entries[0].note)
	require.Equal(t, "rusak", *entries[1].note, "the note is trimmed")
	require.NotNil(t, entries[1].mediaIDs, "an action with no photo has an empty list, not null")
	require.Empty(t, entries[1].mediaIDs)
	require.JSONEq(t, `{"battery_level":40}`, string(entries[1].deviceCondition))
}

func TestActionSnapshotHasTheActionAndTheAnswer(t *testing.T) {
	instruction := "Tarik gembok"
	a := action(1, func(a *db.PatrolCheckpointAction) {
		a.Instruction = pgtype.Text{String: instruction, Valid: true}
		a.MinPhotos, a.MaxPhotos, a.IsNoteRequired = 1, 2, true
	})
	note := "aman"
	raw, err := actionSnapshot(actionEntry{action: a, note: &note, mediaIDs: []int64{11, 12}})
	require.NoError(t, err)
	require.JSONEq(t, `{"action":"Aksi A","instruction":"Tarik gembok","min_photos":1,"max_photos":2,"is_note_required":true,
		"sort_order":1,"note":"aman","media_ids":[11,12]}`, string(raw))

	bare, err := actionSnapshot(actionEntry{action: action(2, nil), mediaIDs: []int64{}})
	require.NoError(t, err)
	require.JSONEq(t, `{"action":"Aksi B","instruction":null,"min_photos":0,"max_photos":1,"is_note_required":false,
		"sort_order":2,"note":null,"media_ids":[]}`, string(bare), "no note is null and no photo is [], and the keys the table checks are always there")
}

func TestCheckActionsOfflineUsesTheChecklistAtTheScan(t *testing.T) {
	deactivatedAfter := action(1, func(a *db.PatrolCheckpointAction) {
		a.Active, a.DeactivatedAt = false, pgtype.Timestamptz{Time: scanAt.Add(time.Hour), Valid: true}
	})
	createdAfter := action(2, func(a *db.PatrolCheckpointAction) {
		a.CreatedAt = pgtype.Timestamptz{Time: scanAt.Add(time.Hour), Valid: true}
	})
	kept := action(3, nil)

	t.Run("an action deactivated after the scan is ignored, with its answer", func(t *testing.T) {
		entries, media, err := checkActions([]db.PatrolCheckpointAction{deactivatedAfter, kept}, []ActionAnswerInput{
			answer(1, func(a *ActionAnswerInput) { a.MediaIDs = []int64{50} }), answer(3, nil),
		}, scanAt)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, actionID(3), entries[0].action.ID)
		require.Empty(t, media, "the dropped answer's photo is not claimed")
	})

	t.Run("an action created after the scan is not required", func(t *testing.T) {
		entries, _, err := checkActions([]db.PatrolCheckpointAction{createdAfter, kept}, []ActionAnswerInput{answer(3, nil)}, scanAt)
		require.NoError(t, err)
		require.Len(t, entries, 1)
	})

	t.Run("answered anyway, it is judged like any other", func(t *testing.T) {
		entries, _, err := checkActions([]db.PatrolCheckpointAction{createdAfter, kept}, []ActionAnswerInput{answer(2, nil), answer(3, nil)}, scanAt)
		require.NoError(t, err)
		require.Len(t, entries, 2)
		tooMany := answer(2, func(a *ActionAnswerInput) { a.MediaIDs = []int64{1, 2} })
		_, _, err = checkActions([]db.PatrolCheckpointAction{createdAfter, kept}, []ActionAnswerInput{tooMany, answer(3, nil)}, scanAt)
		require.Equal(t, ReasonAnswerInvalid, reasonOf(t, err))
	})

	t.Run("a scan made after the edit judges by the new checklist", func(t *testing.T) {
		_, _, err := checkActions([]db.PatrolCheckpointAction{createdAfter, kept}, []ActionAnswerInput{answer(3, nil)}, scanAt.Add(2*time.Hour))
		require.Equal(t, ReasonAnswerRequired, reasonOf(t, err), "the new action is now required")
	})
}

func TestParseDeviceCondition(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string // "" means accepted as sent; otherwise the message must contain it
		none bool   // nothing to store
	}{
		"nothing sent":                   {in: "", none: true},
		"null":                           {in: "null", none: true},
		"an empty object":                {in: `{}`},
		"every known key":                {in: `{"battery_level":55.5,"is_charging":true,"network_type":"wifi","gps_enabled":true,"mock_location":false,"os":"Android","os_version":"14","device_model":"Pixel 8","app_version":"1.2.3"}`},
		"an unknown key is kept":         {in: `{"thermal_state":"serious","extra":{"a":1}}`},
		"mock_location is just stored":   {in: `{"mock_location":true}`},
		"battery at the edges":           {in: `{"battery_level":0}`},
		"battery over 100":               {in: `{"battery_level":101}`, want: "battery_level"},
		"battery under 0":                {in: `{"battery_level":-1}`, want: "battery_level"},
		"battery as text":                {in: `{"battery_level":"full"}`, want: "battery_level"},
		"charging as text":               {in: `{"is_charging":"yes"}`, want: "is_charging"},
		"gps as a number":                {in: `{"gps_enabled":1}`, want: "gps_enabled"},
		"mock location as text":          {in: `{"mock_location":"no"}`, want: "mock_location"},
		"a network that is not known":    {in: `{"network_type":"5g"}`, want: "network_type"},
		"a network as a number":          {in: `{"network_type":4}`, want: "network_type"},
		"an os version that is too long": {in: `{"os_version":"` + strings.Repeat("9", 65) + `"}`, want: "os_version"},
		"an os version at the limit":     {in: `{"os_version":"` + strings.Repeat("9", 64) + `"}`},
		"a device model as a number":     {in: `{"device_model":7}`, want: "device_model"},
		"an array":                       {in: `[]`, want: "objek"},
		"a string":                       {in: `"baik"`, want: "objek"},
		"a number":                       {in: `12`, want: "objek"},
		"broken json":                    {in: `{"battery_level":`, want: "objek"},
		"too large":                      {in: `{"note":"` + strings.Repeat("a", 5000) + `"}`, want: "byte"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseDeviceCondition(json.RawMessage(tc.in), "device_condition")
			if tc.want != "" {
				require.Error(t, err)
				var app *apperr.Error
				require.ErrorAs(t, err, &app)
				require.Equal(t, ReasonDeviceConditionInvalid, app.Reason)
				require.Equal(t, "device_condition", app.Fields[0].Field)
				require.Contains(t, app.Fields[0].Message, tc.want)
				return
			}
			require.NoError(t, err)
			if tc.none {
				require.Nil(t, got)
				return
			}
			require.JSONEq(t, tc.in, string(got), "what is stored is what was sent")
		})
	}
}

func TestWithDeviceConditions(t *testing.T) {
	scan := json.RawMessage(`{"battery_level":80}`)
	own := json.RawMessage(`{"battery_level":10}`)

	t.Run("the scan's condition is the default and an action's own wins", func(t *testing.T) {
		got, err := withDeviceConditions(scan, []ActionAnswerInput{{ActionID: "a"}, {ActionID: "b", DeviceCondition: own}})
		require.NoError(t, err)
		require.JSONEq(t, `{"battery_level":80}`, string(got[0].DeviceCondition))
		require.JSONEq(t, `{"battery_level":10}`, string(got[1].DeviceCondition))
	})
	t.Run("none sent anywhere is nothing", func(t *testing.T) {
		got, err := withDeviceConditions(nil, []ActionAnswerInput{{ActionID: "a"}})
		require.NoError(t, err)
		require.Nil(t, got[0].DeviceCondition)
	})
	t.Run("a bad one names where it was", func(t *testing.T) {
		_, err := withDeviceConditions(json.RawMessage(`[]`), nil)
		var app *apperr.Error
		require.ErrorAs(t, err, &app)
		require.Equal(t, "device_condition", app.Fields[0].Field)
		_, err = withDeviceConditions(nil, []ActionAnswerInput{{ActionID: "a"}, {ActionID: "b", DeviceCondition: json.RawMessage(`"x"`)}})
		require.ErrorAs(t, err, &app)
		require.Equal(t, "actions[1].device_condition", app.Fields[0].Field)
	})
	t.Run("the input is not changed", func(t *testing.T) {
		in := []ActionAnswerInput{{ActionID: "a"}}
		_, err := withDeviceConditions(scan, in)
		require.NoError(t, err)
		require.Nil(t, in[0].DeviceCondition)
	})
}

func TestSubjectOf(t *testing.T) {
	require.Equal(t, "Gerbang", subjectOf(nil, "Gerbang"))
	require.Equal(t, "Gerbang", subjectOf(ptr("   "), "Gerbang"))
	require.Equal(t, "Patroli malam", subjectOf(ptr("  Patroli malam "), "Gerbang"))
}
