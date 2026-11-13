package patrol

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
)

// Answer types, attachment modes and the ON_NO rule, as stored.
const (
	AnswerBoolean = "BOOLEAN"
	AnswerText    = "TEXT"
	AnswerNumber  = "NUMBER"
	AnswerChoice  = "CHOICE"

	AttachNone     = "NONE"
	AttachOptional = "OPTIONAL"
	AttachRequired = "REQUIRED"

	WhenAlways = "ALWAYS"
	WhenOnNo   = "ON_NO"
)

// MaxChecklistItems is how many questions one zone may ask. A guard answers them on a phone while
// standing at the post; past this the form is not a checklist any more.
const MaxChecklistItems = 100

// ChecklistItemInput is one question as the admin sends it. Pointers tell "not sent" (the
// default applies) from a value, so photo_max 0 is refused instead of read as "default".
type ChecklistItemInput struct {
	// ID names an existing item to update; nil creates one.
	ID         *int64   `json:"id" validate:"omitempty,min=1"`
	Label      string   `json:"label" validate:"required,notblank,max=500"`
	HelpText   *string  `json:"help_text" validate:"omitempty,max=1000"`
	AnswerType string   `json:"answer_type" validate:"required,oneof=BOOLEAN TEXT NUMBER CHOICE"`
	Options    []Option `json:"options" validate:"omitempty,max=50,dive"`
	Required   *bool    `json:"required"`
	PhotoMode  *string  `json:"photo_mode" validate:"omitempty,oneof=NONE OPTIONAL REQUIRED"`
	PhotoMax   *int     `json:"photo_max" validate:"omitempty,min=1,max=5"`
	NoteMode   *string  `json:"note_mode" validate:"omitempty,oneof=NONE OPTIONAL REQUIRED"`
	AttachWhen *string  `json:"attach_when" validate:"omitempty,oneof=ALWAYS ON_NO"`
}

// ChecklistReplace is the body of PUT /admin/zones/{id}/checklist.
type ChecklistReplace struct {
	Checklist []ChecklistItemInput `json:"checklist" validate:"required,max=100,dive"`
}

// itemSpec is a checked, defaulted question, ready to be stored.
type itemSpec struct {
	id         int64 // 0 for a new item
	label      string
	helpText   *string
	answerType db.ChecklistAnswerType
	options    json.RawMessage // nil unless CHOICE
	required   bool
	photoMode  db.ChecklistAttachMode
	photoMax   int16
	noteMode   db.ChecklistAttachMode
	attachWhen db.ChecklistAttachWhen
}

// parseChecklist checks the combinations the struct tags cannot see and applies the defaults. It
// reports every bad field at once, named by its place in the body (checklist[2].options).
//
// The rules (the table has the same ones as CHECKs, so a mistake here cannot reach the data):
//   - a CHOICE has at least two options with distinct values, and only a CHOICE has options;
//   - attach_when ON_NO belongs to a BOOLEAN: "no" is the only answer that can ask for proof;
//   - an id appears once.
func parseChecklist(in []ChecklistItemInput) ([]itemSpec, error) {
	var problems []apperr.FieldError
	fail := func(i int, field, msg string) {
		problems = append(problems, apperr.FieldError{
			Field: fmt.Sprintf("checklist[%d].%s", i, field), Code: "INVALID", Message: msg,
		})
	}
	out := make([]itemSpec, 0, len(in))
	seenID := map[int64]bool{}
	for i, it := range in {
		spec := itemSpec{
			label: strings.TrimSpace(it.Label), helpText: nonBlank(it.HelpText), answerType: db.ChecklistAnswerType(it.AnswerType),
			required: it.Required == nil || *it.Required, photoMax: 1,
			photoMode: db.ChecklistAttachModeNONE, noteMode: db.ChecklistAttachModeNONE, attachWhen: db.ChecklistAttachWhenALWAYS,
		}
		if it.ID != nil {
			if seenID[*it.ID] {
				fail(i, "id", "Butir yang sama dikirim lebih dari sekali.")
			}
			seenID[*it.ID] = true
			spec.id = *it.ID
		}
		if it.PhotoMode != nil {
			spec.photoMode = db.ChecklistAttachMode(*it.PhotoMode)
		}
		if it.NoteMode != nil {
			spec.noteMode = db.ChecklistAttachMode(*it.NoteMode)
		}
		if it.PhotoMax != nil {
			spec.photoMax = int16(*it.PhotoMax) //nolint:gosec // validated to 1..5
		}
		if it.AttachWhen != nil {
			spec.attachWhen = db.ChecklistAttachWhen(*it.AttachWhen)
		}

		switch it.AnswerType {
		case AnswerChoice:
			if len(it.Options) < 2 {
				fail(i, "options", "Pertanyaan pilihan butuh minimal 2 opsi.")
				break
			}
			values := make(map[string]bool, len(it.Options))
			for _, o := range it.Options {
				values[o.Value] = true
			}
			if len(values) != len(it.Options) {
				fail(i, "options", "Nilai opsi harus unik.")
				break
			}
			raw, err := json.Marshal(it.Options)
			if err != nil {
				return nil, fmt.Errorf("encode options: %w", err)
			}
			spec.options = raw
		default:
			if len(it.Options) > 0 {
				fail(i, "options", "Opsi hanya untuk pertanyaan bertipe CHOICE.")
			}
		}
		if spec.attachWhen == db.ChecklistAttachWhenONNO && it.AnswerType != AnswerBoolean {
			fail(i, "attach_when", "ON_NO hanya untuk pertanyaan bertipe BOOLEAN.")
		}
		out = append(out, spec)
	}
	if len(problems) > 0 {
		return nil, apperr.ErrValidation.WithFields(problems...)
	}
	return out, nil
}

func nonBlank(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

// replaceChecklist makes the zone's active items match specs, in the order sent. The caller holds
// the zone's row lock, so two admins editing the same checklist take turns instead of
// interleaving their inserts and deactivations.
//
// Items with an id are updated, items without are created, and active items missing from specs are
// deactivated and never deleted. ⚠ Editing an item never rewrites a scan that was already
// submitted: a scan keeps its own copy of each question (Phase 10.2), which is why an item can be
// changed freely here.
func replaceChecklist(ctx context.Context, q *db.Queries, c clock.Clock, zoneID int64, specs []itemSpec) (created, updated int, deactivated []int64, err error) {
	current, err := q.ListChecklistItems(ctx, zoneID)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("list checklist: %w", err)
	}
	active := make(map[int64]bool, len(current))
	for _, it := range current {
		active[it.ID] = true
	}
	// Refuse unknown ids before writing anything, and name the one that is wrong. An item that
	// was deactivated is not found either: to bring a question back, send it without an id.
	var problems []apperr.FieldError
	for i, s := range specs {
		if s.id != 0 && !active[s.id] {
			problems = append(problems, apperr.FieldError{
				Field: fmt.Sprintf("checklist[%d].id", i), Code: "INVALID", Message: "Butir tidak ditemukan di zona ini.",
			})
		}
	}
	if len(problems) > 0 {
		return 0, 0, nil, apperr.ErrValidation.WithFields(problems...)
	}

	now := ts(c.Now())
	keep := make([]int64, 0, len(specs))
	for i, s := range specs {
		if s.id == 0 {
			row, err := q.InsertChecklistItem(ctx, db.InsertChecklistItemParams{
				ZoneID: zoneID, SortOrder: int32(i), Label: s.label, HelpText: textOrNull(s.helpText), //nolint:gosec // at most 100
				AnswerType: s.answerType, Options: s.options, Required: s.required, PhotoMode: s.photoMode,
				PhotoMax: s.photoMax, NoteMode: s.noteMode, AttachWhen: s.attachWhen, Now: now,
			})
			if err != nil {
				return 0, 0, nil, fmt.Errorf("insert checklist item: %w", err)
			}
			keep = append(keep, row.ID)
			created++
			continue
		}
		row, err := q.UpdateChecklistItem(ctx, db.UpdateChecklistItemParams{
			ID: s.id, ZoneID: zoneID, SortOrder: int32(i), Label: s.label, HelpText: textOrNull(s.helpText), //nolint:gosec // at most 100
			AnswerType: s.answerType, Options: s.options, Required: s.required, PhotoMode: s.photoMode,
			PhotoMax: s.photoMax, NoteMode: s.noteMode, AttachWhen: s.attachWhen, Now: now,
		})
		if err != nil {
			return 0, 0, nil, fmt.Errorf("update checklist item: %w", err)
		}
		keep = append(keep, row.ID)
		updated++
	}
	deactivated, err = q.DeactivateChecklistItemsExcept(ctx, db.DeactivateChecklistItemsExceptParams{ZoneID: zoneID, KeepIds: keep, Now: now})
	if err != nil {
		return 0, 0, nil, fmt.Errorf("deactivate checklist items: %w", err)
	}
	return created, updated, deactivated, nil
}
