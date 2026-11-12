package patrol

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
)

func ptr[T any](v T) *T { return &v }

func twoOptions() []Option {
	return []Option{{Value: "a", Label: "A"}, {Value: "b", Label: "B"}}
}

func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	var app *apperr.Error
	require.ErrorAs(t, err, &app)
	var out []string
	for _, f := range app.Fields {
		out = append(out, f.Field)
	}
	return out
}

func TestParseChecklistDefaults(t *testing.T) {
	specs, err := parseChecklist([]ChecklistItemInput{{Label: "  Pagar  ", AnswerType: AnswerBoolean}})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	s := specs[0]
	require.Equal(t, "Pagar", s.label, "the label is trimmed")
	require.True(t, s.required, "a question is required unless the admin says otherwise")
	require.Equal(t, db.ChecklistAttachModeNONE, s.photoMode)
	require.Equal(t, db.ChecklistAttachModeNONE, s.noteMode)
	require.Equal(t, int16(1), s.photoMax)
	require.Equal(t, db.ChecklistAttachWhenALWAYS, s.attachWhen)
	require.Nil(t, s.options)
	require.Nil(t, s.helpText)
}

func TestParseChecklistKeepsWhatWasSent(t *testing.T) {
	specs, err := parseChecklist([]ChecklistItemInput{{
		ID: ptr(int64(7)), Label: "Lampu", HelpText: ptr(" cek "), AnswerType: AnswerChoice, Options: twoOptions(), Required: ptr(false),
		PhotoMode: ptr(AttachRequired), PhotoMax: ptr(4), NoteMode: ptr(AttachOptional),
	}})
	require.NoError(t, err)
	s := specs[0]
	require.Equal(t, int64(7), s.id)
	require.Equal(t, "cek", *s.helpText)
	require.False(t, s.required)
	require.Equal(t, db.ChecklistAttachModeREQUIRED, s.photoMode)
	require.Equal(t, int16(4), s.photoMax)
	require.JSONEq(t, `[{"value":"a","label":"A"},{"value":"b","label":"B"}]`, string(s.options))
}

func TestParseChecklistCombinations(t *testing.T) {
	for name, tc := range map[string]struct {
		items []ChecklistItemInput
		want  []string // the bad fields; nil means accepted
	}{
		"CHOICE with two options": {[]ChecklistItemInput{{Label: "Q", AnswerType: AnswerChoice, Options: twoOptions()}}, nil},
		"ON_NO on a BOOLEAN":      {[]ChecklistItemInput{{Label: "Q", AnswerType: AnswerBoolean, AttachWhen: ptr(WhenOnNo)}}, nil},
		"CHOICE without options":  {[]ChecklistItemInput{{Label: "Q", AnswerType: AnswerChoice}}, []string{"checklist[0].options"}},
		"CHOICE with one option":  {[]ChecklistItemInput{{Label: "Q", AnswerType: AnswerChoice, Options: twoOptions()[:1]}}, []string{"checklist[0].options"}},
		"CHOICE duplicate values": {[]ChecklistItemInput{{Label: "Q", AnswerType: AnswerChoice, Options: []Option{{Value: "a", Label: "A"}, {Value: "a", Label: "B"}}}}, []string{"checklist[0].options"}},
		"options on a TEXT":       {[]ChecklistItemInput{{Label: "Q", AnswerType: AnswerText, Options: twoOptions()}}, []string{"checklist[0].options"}},
		"options on a NUMBER":     {[]ChecklistItemInput{{Label: "Q", AnswerType: AnswerNumber, Options: twoOptions()}}, []string{"checklist[0].options"}},
		"ON_NO on a TEXT":         {[]ChecklistItemInput{{Label: "Q", AnswerType: AnswerText, AttachWhen: ptr(WhenOnNo)}}, []string{"checklist[0].attach_when"}},
		"ON_NO on a CHOICE":       {[]ChecklistItemInput{{Label: "Q", AnswerType: AnswerChoice, Options: twoOptions(), AttachWhen: ptr(WhenOnNo)}}, []string{"checklist[0].attach_when"}},
		"the same id twice":       {[]ChecklistItemInput{{ID: ptr(int64(1)), Label: "Q", AnswerType: AnswerText}, {ID: ptr(int64(1)), Label: "R", AnswerType: AnswerText}}, []string{"checklist[1].id"}},
		"every bad item is named": {[]ChecklistItemInput{
			{Label: "A", AnswerType: AnswerText},
			{Label: "B", AnswerType: AnswerChoice},
			{Label: "C", AnswerType: AnswerNumber, AttachWhen: ptr(WhenOnNo)},
		}, []string{"checklist[1].options", "checklist[2].attach_when"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseChecklist(tc.items)
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.Equal(t, tc.want, fieldsOf(t, err))
		})
	}
}

func TestGeofence(t *testing.T) {
	lat, lng := -6.2, 106.8
	t.Run("no geofence at all is allowed", func(t *testing.T) {
		g, err := geofence(nil, nil, nil, false)
		require.NoError(t, err)
		require.Nil(t, g.lat)
		require.Nil(t, g.radius)
	})
	t.Run("a point without a radius gets the default", func(t *testing.T) {
		g, err := geofence(&lat, &lng, nil, false)
		require.NoError(t, err)
		require.Equal(t, DefaultRadiusMeters, *g.radius)
	})
	t.Run("a radius that was sent is kept", func(t *testing.T) {
		g, err := geofence(&lat, &lng, ptr(120), false)
		require.NoError(t, err)
		require.Equal(t, 120, *g.radius)
	})
	t.Run("the radius cleared on purpose conflicts with the point", func(t *testing.T) {
		_, err := geofence(&lat, &lng, nil, true)
		require.Equal(t, []string{"radius_meters"}, fieldsOf(t, err))
	})
	t.Run("a radius without a point is refused", func(t *testing.T) {
		_, err := geofence(nil, nil, ptr(50), false)
		require.Equal(t, []string{"radius_meters"}, fieldsOf(t, err))
	})
	t.Run("half a point is refused", func(t *testing.T) {
		_, err := geofence(&lat, nil, nil, false)
		require.Equal(t, []string{"latitude"}, fieldsOf(t, err))
		_, err = geofence(nil, &lng, ptr(50), false)
		require.Equal(t, []string{"latitude"}, fieldsOf(t, err))
	})
}

func TestPayloadAndSecret(t *testing.T) {
	require.Equal(t, "PERSIST-ZONE:1:900101:42:abcdefghijklmnopqrstuv", Payload(900101, 42, "abcdefghijklmnopqrstuv"))

	seen := map[string]bool{}
	for range 50 {
		s, err := newSecret()
		require.NoError(t, err)
		require.Len(t, s, 22, "16 bytes in unpadded base64url")
		raw, err := base64.RawURLEncoding.DecodeString(s)
		require.NoError(t, err)
		require.Len(t, raw, 16)
		require.False(t, seen[s], "secrets do not repeat")
		seen[s] = true
	}
}

func TestFileName(t *testing.T) {
	for name, tc := range map[string]struct{ zone, format, want string }{
		"plain":            {"Pos Gerbang", "png", "qr-zona-pos-gerbang.png"},
		"punctuation":      {"  Pos Gerbang Utama!! (A/B) ", "svg", "qr-zona-pos-gerbang-utama-a-b.svg"},
		"unicode":          {"Pós Jaga ☃", "png", "qr-zona-p-s-jaga.png"},
		"only symbols":     {"!!!", "png", "qr-zona-zona.png"},
		"header injection": {"a\r\nSet-Cookie: x=1", "png", "qr-zona-a-set-cookie-x-1.png"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, fileName(tc.zone, tc.format))
		})
	}
	long := fileName(strings.Repeat("a", 200), "png")
	require.LessOrEqual(t, len(long), len("qr-zona-")+60+len(".png"))
}

func TestRenderQR(t *testing.T) {
	payload := Payload(1, 2, "abcdefghijklmnopqrstuv")

	body, contentType, err := renderQR(payload, FormatPNG)
	require.NoError(t, err)
	require.Equal(t, "image/png", contentType)
	img, err := png.Decode(bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, pngSize, img.Bounds().Dx())

	body, contentType, err = renderQR(payload, FormatSVG)
	require.NoError(t, err)
	require.Equal(t, "image/svg+xml", contentType)
	svg := string(body)
	require.True(t, strings.HasPrefix(svg, `<?xml`))
	require.Contains(t, svg, `viewBox="0 0 `)
	require.Contains(t, svg, `<path fill="#000" d="M`)
	require.True(t, strings.HasSuffix(svg, `</svg>`))
	require.NotContains(t, svg, "abcdefghijklmnopqrstuv", "the picture holds modules, not the text")

	_, _, err = renderQR(payload, "pdf")
	require.Error(t, err)

	t.Run("a different secret is a different picture", func(t *testing.T) {
		other, _, err := renderQR(Payload(1, 2, "ZZZZZZZZZZZZZZZZZZZZZZ"), FormatSVG)
		require.NoError(t, err)
		require.NotEqual(t, svg, string(other))
	})
}

func TestSVGRunsOfModules(t *testing.T) {
	// ■■□
	// □■■
	got := string(svgOf([][]bool{{true, true, false}, {false, true, true}}))
	require.Contains(t, got, `viewBox="0 0 2 2"`) // n is the row count; the QR bitmap is square
	require.Contains(t, got, "M0 0h2v1h-2z", "two modules in a row are one rectangle")
	require.Contains(t, got, "M1 1h2v1h-2z")
}
