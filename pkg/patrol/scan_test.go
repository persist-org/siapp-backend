package patrol

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

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

func TestVerifyPayload(t *testing.T) {
	const secret = "abcdefghijklmnopqrstuv"
	good := Payload(900101, 42, secret)
	for name, tc := range map[string]struct {
		payload string
		want    bool
	}{
		"the zone's own code":    {good, true},
		"another zone":           {Payload(900101, 43, secret), false},
		"another organization":   {Payload(900102, 42, secret), false},
		"an old secret":          {Payload(900101, 42, "ZZZZZZZZZZZZZZZZZZZZZZ"), false},
		"a prefix of the secret": {Payload(900101, 42, secret[:10]), false},
		"nothing":                {"", false},
		"another format":         {"https://example.com/zone/42", false},
		"another version":        {"PERSIST-ZONE:2:900101:42:" + secret, false},
		"too many parts":         {good + ":x", false},
		"not numbers":            {"PERSIST-ZONE:1:a:b:" + secret, false},
		"a zero zone id":         {"PERSIST-ZONE:1:900101:0:" + secret, false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, verifyPayload(tc.payload, 900101, 42, secret))
		})
	}
}

// item builds a checklist item row, created long ago and active unless a test says otherwise.
func item(id int64, typ db.ChecklistAnswerType, edit func(*db.PatrolChecklistItem)) db.PatrolChecklistItem {
	it := db.PatrolChecklistItem{
		ID: id, ZoneID: 1, SortOrder: int32(id), //nolint:gosec // a small test id
		Label: "Q" + strconv.FormatInt(id, 10), AnswerType: typ, Required: true,
		PhotoMode: db.ChecklistAttachModeNONE, PhotoMax: 1, NoteMode: db.ChecklistAttachModeNONE,
		AttachWhen: db.ChecklistAttachWhenALWAYS, Active: true,
		CreatedAt: pgtype.Timestamptz{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
	}
	if typ == db.ChecklistAnswerTypeCHOICE {
		it.Options = json.RawMessage(`[{"value":"ok","label":"Ok"},{"value":"bad","label":"Bad"}]`)
	}
	if edit != nil {
		edit(&it)
	}
	return it
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var app *apperr.Error
	require.ErrorAs(t, err, &app)
	return app.Reason
}

var scanAt = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func TestCheckAnswersRejections(t *testing.T) {
	boolean := func(edit func(*db.PatrolChecklistItem)) db.PatrolChecklistItem {
		return item(1, db.ChecklistAnswerTypeBOOLEAN, edit)
	}
	photoRequired := func(it *db.PatrolChecklistItem) { it.PhotoMode = db.ChecklistAttachModeREQUIRED }
	for name, tc := range map[string]struct {
		items   []db.PatrolChecklistItem
		answers []AnswerInput
		reason  string // "" means accepted
	}{
		"a required answer is missing":       {[]db.PatrolChecklistItem{boolean(nil)}, nil, ReasonAnswerRequired},
		"a required answer is null":          {[]db.PatrolChecklistItem{boolean(nil)}, []AnswerInput{{ItemID: 1, Value: raw("null")}}, ReasonAnswerRequired},
		"a required text is blank":           {[]db.PatrolChecklistItem{item(1, db.ChecklistAnswerTypeTEXT, nil)}, []AnswerInput{{ItemID: 1, Value: raw(`"  "`)}}, ReasonAnswerRequired},
		"an optional answer may be left out": {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) { it.Required = false })}, nil, ""},
		"a boolean given a string":           {[]db.PatrolChecklistItem{boolean(nil)}, []AnswerInput{{ItemID: 1, Value: raw(`"yes"`)}}, ReasonAnswerInvalid},
		"a number given a string":            {[]db.PatrolChecklistItem{item(1, db.ChecklistAnswerTypeNUMBER, nil)}, []AnswerInput{{ItemID: 1, Value: raw(`"12"`)}}, ReasonAnswerInvalid},
		"a number accepted":                  {[]db.PatrolChecklistItem{item(1, db.ChecklistAnswerTypeNUMBER, nil)}, []AnswerInput{{ItemID: 1, Value: raw(`12.5`)}}, ""},
		"a text given a number":              {[]db.PatrolChecklistItem{item(1, db.ChecklistAnswerTypeTEXT, nil)}, []AnswerInput{{ItemID: 1, Value: raw(`7`)}}, ReasonAnswerInvalid},
		"a choice outside its options":       {[]db.PatrolChecklistItem{item(1, db.ChecklistAnswerTypeCHOICE, nil)}, []AnswerInput{{ItemID: 1, Value: raw(`"worse"`)}}, ReasonAnswerInvalid},
		"a choice inside its options":        {[]db.PatrolChecklistItem{item(1, db.ChecklistAnswerTypeCHOICE, nil)}, []AnswerInput{{ItemID: 1, Value: raw(`"bad"`)}}, ""},
		"an item that is not of this zone":   {[]db.PatrolChecklistItem{boolean(nil)}, []AnswerInput{{ItemID: 1, Value: raw(`true`)}, {ItemID: 99, Value: raw(`true`)}}, ReasonChecklistItemInvalid},
		"an item answered twice":             {[]db.PatrolChecklistItem{boolean(nil)}, []AnswerInput{{ItemID: 1, Value: raw(`true`)}, {ItemID: 1, Value: raw(`false`)}}, ReasonChecklistItemInvalid},
		"an item deactivated before the scan": {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) {
			it.Active, it.DeactivatedAt = false, pgtype.Timestamptz{Time: scanAt.Add(-time.Hour), Valid: true}
			it.Required = false
		})}, []AnswerInput{{ItemID: 1, Value: raw(`true`)}}, ReasonChecklistItemInvalid},
		"a photo on a NONE item":                 {[]db.PatrolChecklistItem{boolean(nil)}, []AnswerInput{{ItemID: 1, Value: raw(`true`), MediaIDs: []int64{5}}}, ReasonAnswerInvalid},
		"too many photos":                        {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) { it.PhotoMode = db.ChecklistAttachModeOPTIONAL; it.PhotoMax = 2 })}, []AnswerInput{{ItemID: 1, Value: raw(`true`), MediaIDs: []int64{5, 6, 7}}}, ReasonAnswerInvalid},
		"the photo limit is allowed":             {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) { it.PhotoMode = db.ChecklistAttachModeOPTIONAL; it.PhotoMax = 2 })}, []AnswerInput{{ItemID: 1, Value: raw(`true`), MediaIDs: []int64{5, 6}}}, ""},
		"a note on a NONE item":                  {[]db.PatrolChecklistItem{boolean(nil)}, []AnswerInput{{ItemID: 1, Value: raw(`true`), Note: ptr("catatan")}}, ReasonAnswerInvalid},
		"a blank note on a NONE item is nothing": {[]db.PatrolChecklistItem{boolean(nil)}, []AnswerInput{{ItemID: 1, Value: raw(`true`), Note: ptr("   ")}}, ""},
		"a required photo is missing":            {[]db.PatrolChecklistItem{boolean(photoRequired)}, []AnswerInput{{ItemID: 1, Value: raw(`true`)}}, ReasonAttachmentRequired},
		"a required photo is there":              {[]db.PatrolChecklistItem{boolean(photoRequired)}, []AnswerInput{{ItemID: 1, Value: raw(`true`), MediaIDs: []int64{5}}}, ""},
		"a required note is missing":             {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) { it.NoteMode = db.ChecklistAttachModeREQUIRED })}, []AnswerInput{{ItemID: 1, Value: raw(`true`)}}, ReasonAttachmentRequired},
		"a required note is there":               {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) { it.NoteMode = db.ChecklistAttachModeREQUIRED })}, []AnswerInput{{ItemID: 1, Value: raw(`true`), Note: ptr("ok")}}, ""},
		"an optional photo may be left out":      {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) { it.PhotoMode = db.ChecklistAttachModeOPTIONAL })}, []AnswerInput{{ItemID: 1, Value: raw(`true`)}}, ""},
		"ON_NO: yes needs no photo":              {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) { photoRequired(it); it.AttachWhen = db.ChecklistAttachWhenONNO })}, []AnswerInput{{ItemID: 1, Value: raw(`true`)}}, ""},
		"ON_NO: no needs a photo":                {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) { photoRequired(it); it.AttachWhen = db.ChecklistAttachWhenONNO })}, []AnswerInput{{ItemID: 1, Value: raw(`false`)}}, ReasonAttachmentRequired},
		"ON_NO: no with a photo is fine":         {[]db.PatrolChecklistItem{boolean(func(it *db.PatrolChecklistItem) { photoRequired(it); it.AttachWhen = db.ChecklistAttachWhenONNO })}, []AnswerInput{{ItemID: 1, Value: raw(`false`), MediaIDs: []int64{5}}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := checkAnswers(tc.items, tc.answers, scanAt)
			if tc.reason == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, tc.reason, reasonOf(t, err))
		})
	}
}

func TestCheckAnswersNamesTheItem(t *testing.T) {
	_, _, err := checkAnswers([]db.PatrolChecklistItem{item(3, db.ChecklistAnswerTypeBOOLEAN, nil)}, nil, scanAt)
	var app *apperr.Error
	require.ErrorAs(t, err, &app)
	require.EqualValues(t, 3, app.Details["item_id"])
	require.Contains(t, app.Message, "Q3")
}

func TestCheckAnswersSnapshot(t *testing.T) {
	items := []db.PatrolChecklistItem{
		item(1, db.ChecklistAnswerTypeBOOLEAN, func(it *db.PatrolChecklistItem) {
			it.PhotoMode, it.PhotoMax, it.NoteMode = db.ChecklistAttachModeOPTIONAL, 2, db.ChecklistAttachModeOPTIONAL
		}),
		item(2, db.ChecklistAnswerTypeCHOICE, func(it *db.PatrolChecklistItem) { it.Required = false }),
		item(3, db.ChecklistAnswerTypeTEXT, func(it *db.PatrolChecklistItem) { it.Required = false }),
	}
	entries, media, err := checkAnswers(items, []AnswerInput{
		{ItemID: 1, Value: raw(`false`), Note: ptr(" rusak "), MediaIDs: []int64{8, 9}},
		{ItemID: 2, Value: raw(`"ok"`)},
	}, scanAt)
	require.NoError(t, err)
	require.Len(t, entries, 3, "one entry per question, the unanswered optional one too")
	require.Equal(t, []int64{8, 9}, media)

	first := entries[0]
	require.Equal(t, false, first.Value)
	require.Equal(t, "rusak", *first.Note)
	require.Equal(t, []int64{8, 9}, first.MediaIDs)
	require.Equal(t, "Q1", first.Label, "the question is copied into the entry")
	require.Equal(t, "OPTIONAL", first.PhotoMode)

	require.Equal(t, "ok", entries[1].Value)
	require.Len(t, entries[1].Options, 2, "a CHOICE keeps its options")

	require.Nil(t, entries[2].Value, "an unanswered optional question has no value")
	require.Empty(t, entries[2].MediaIDs)
	require.NotNil(t, entries[2].MediaIDs, "and an empty list, not null")
}

func TestCheckAnswersOfflineUsesTheChecklistAtTheScan(t *testing.T) {
	deactivatedAfter := item(1, db.ChecklistAnswerTypeBOOLEAN, func(it *db.PatrolChecklistItem) {
		it.Active, it.DeactivatedAt = false, pgtype.Timestamptz{Time: scanAt.Add(time.Hour), Valid: true}
	})
	createdAfter := item(2, db.ChecklistAnswerTypeBOOLEAN, func(it *db.PatrolChecklistItem) {
		it.CreatedAt = pgtype.Timestamptz{Time: scanAt.Add(time.Hour), Valid: true}
	})
	kept := item(3, db.ChecklistAnswerTypeBOOLEAN, nil)

	t.Run("an item deactivated after the scan is ignored, with its answer", func(t *testing.T) {
		entries, media, err := checkAnswers([]db.PatrolChecklistItem{deactivatedAfter, kept}, []AnswerInput{
			{ItemID: 1, Value: raw(`true`), MediaIDs: []int64{50}},
			{ItemID: 3, Value: raw(`true`)},
		}, scanAt)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.EqualValues(t, 3, entries[0].ItemID)
		require.Empty(t, media, "the dropped answer's photo is not claimed")
	})

	t.Run("an item created after the scan is not required", func(t *testing.T) {
		entries, _, err := checkAnswers([]db.PatrolChecklistItem{createdAfter, kept}, []AnswerInput{{ItemID: 3, Value: raw(`true`)}}, scanAt)
		require.NoError(t, err)
		require.Len(t, entries, 1)
	})

	t.Run("answered anyway, it is judged like any other", func(t *testing.T) {
		entries, _, err := checkAnswers([]db.PatrolChecklistItem{createdAfter, kept}, []AnswerInput{
			{ItemID: 2, Value: raw(`false`)}, {ItemID: 3, Value: raw(`true`)},
		}, scanAt)
		require.NoError(t, err)
		require.Len(t, entries, 2)
		require.False(t, entries[0].Required, "and it is not marked required, the guard could not have known")
		_, _, err = checkAnswers([]db.PatrolChecklistItem{createdAfter, kept}, []AnswerInput{
			{ItemID: 2, Value: raw(`"x"`)}, {ItemID: 3, Value: raw(`true`)},
		}, scanAt)
		require.Equal(t, ReasonAnswerInvalid, reasonOf(t, err))
	})

	t.Run("a scan made after the edit judges by the new checklist", func(t *testing.T) {
		_, _, err := checkAnswers([]db.PatrolChecklistItem{createdAfter, kept}, []AnswerInput{{ItemID: 3, Value: raw(`true`)}}, scanAt.Add(2*time.Hour))
		require.Equal(t, ReasonAnswerRequired, reasonOf(t, err), "the new question is now required")
	})
}
