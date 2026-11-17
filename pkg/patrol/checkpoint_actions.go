package patrol

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	"siapp/internal/db"
	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/clock"
)

// ActionInput is one action of a checkpoint's checklist as the admin sends it. Pointers tell "not
// sent" (the default applies) from a value, so max_photos 0 means "no photo slot" and is not read
// as "default".
//
// ⚠ There are no validate tags here on purpose: every value is checked in parseActions, so a bad
// action always answers 422 CHECKPOINT_ACTION_INVALID with the field named, not a generic validation
// error for some fields and the specific one for others.
type ActionInput struct {
	// ID names an existing action to update; nil creates one.
	ID          *string `json:"id"`
	Action      string  `json:"action"`
	Instruction *string `json:"instruction"`
	// MinPhotos is how many photos the guard must take: 0 makes the photo optional.
	MinPhotos *int `json:"min_photos"`
	// MaxPhotos is how many photos the guard may take: 0 means the action has no photo slot.
	MaxPhotos      *int  `json:"max_photos"`
	IsNoteRequired *bool `json:"is_note_required"`
}

// ActionsReplace is the body of PUT /admin/checkpoints/{id}/actions. An empty list is allowed: it
// clears the checklist.
type ActionsReplace struct {
	Actions []ActionInput `json:"actions" validate:"required"`
}

// The longest an action's name and instruction may be, in characters.
const (
	maxActionNameLength  = 300
	maxInstructionLength = 1000
)

// actionSpec is a checked, defaulted action, ready to be stored.
type actionSpec struct {
	id             pgtype.UUID // invalid for a new action
	action         string
	instruction    *string
	minPhotos      int16
	maxPhotos      int16
	isNoteRequired bool
}

// parseActions checks the combinations the struct tags cannot see and applies the defaults. It
// reports every bad field at once, named by its place in the body (actions[2].max_photos).
//
// The rules (the table has the same ones as CHECKs, so a mistake here cannot reach the data):
//   - 0 <= min_photos <= max_photos <= 5;
//   - max_photos left out is 1, or min_photos when that is more, so "min_photos: 2" alone is valid;
//   - an id is a uuid and appears once.
func parseActions(in []ActionInput) ([]actionSpec, error) {
	var problems []apperr.FieldError
	fail := func(i int, field, msg string) {
		problems = append(problems, apperr.FieldError{Field: fmt.Sprintf("actions[%d].%s", i, field), Code: "INVALID", Message: msg})
	}
	if len(in) > MaxCheckpointActions {
		problems = append(problems, apperr.FieldError{
			Field: "actions", Code: "INVALID", Message: fmt.Sprintf("Maksimal %d aksi per checkpoint.", MaxCheckpointActions),
		})
		return nil, actionInvalid(problems...)
	}
	out := make([]actionSpec, 0, len(in))
	seen := map[string]bool{}
	for i, a := range in {
		spec := actionSpec{
			action: strings.TrimSpace(a.Action), instruction: nonBlank(a.Instruction),
			isNoteRequired: a.IsNoteRequired != nil && *a.IsNoteRequired,
		}
		switch n := utf8.RuneCountInString(spec.action); {
		case n == 0:
			fail(i, "action", "Wajib diisi.")
		case n > maxActionNameLength:
			fail(i, "action", fmt.Sprintf("Maksimal %d karakter.", maxActionNameLength))
		}
		if spec.instruction != nil && utf8.RuneCountInString(*spec.instruction) > maxInstructionLength {
			fail(i, "instruction", fmt.Sprintf("Maksimal %d karakter.", maxInstructionLength))
		}
		if a.ID != nil {
			id, ok := parseUUID(*a.ID)
			switch {
			case !ok:
				fail(i, "id", "Harus berupa uuid.")
			case seen[uuidString(id)]:
				fail(i, "id", "Aksi yang sama dikirim lebih dari sekali.")
			default:
				seen[uuidString(id)] = true
				spec.id = id
			}
		}
		minPhotos := 0
		if a.MinPhotos != nil {
			minPhotos = *a.MinPhotos
		}
		maxPhotos := max(1, minPhotos)
		if a.MaxPhotos != nil {
			maxPhotos = *a.MaxPhotos
		}
		// The range checks repeat the struct tags: they also cover a caller that skipped validation,
		// and keep the narrowing to int16 below safe.
		switch {
		case minPhotos < 0 || minPhotos > MaxActionPhotos:
			fail(i, "min_photos", fmt.Sprintf("Harus antara 0 dan %d.", MaxActionPhotos))
		case maxPhotos < 0 || maxPhotos > MaxActionPhotos:
			fail(i, "max_photos", fmt.Sprintf("Harus antara 0 dan %d.", MaxActionPhotos))
		case minPhotos > maxPhotos:
			fail(i, "min_photos", "Tidak boleh lebih besar dari max_photos.")
		}
		spec.minPhotos, spec.maxPhotos = int16(minPhotos), int16(maxPhotos) //nolint:gosec // checked to 0..5 above
		out = append(out, spec)
	}
	if len(problems) > 0 {
		return nil, actionInvalid(problems...)
	}
	return out, nil
}

// replaceActions makes the checkpoint's active actions match specs, in the order sent. The caller
// holds the checkpoint's row lock, so two admins editing the same checklist take turns instead of
// interleaving their inserts and deactivations.
//
// Actions with an id are updated, actions without are created, and active actions missing from specs
// are deactivated and never deleted. ⚠ Editing an action never rewrites a scan that was already
// submitted: a scan keeps its own copy of each action (patrol_scan_actions.snapshot), which is why an
// action can be changed freely here.
//
// It is the same shape as replaceChecklist (the zone's old checklist, which Phase 10.9 deletes), not
// a shared function: the two differ in the id type, the fields and the error names, and a generic
// over all three would be harder to read than the twenty lines it saves.
func replaceActions(ctx context.Context, q *db.Queries, c clock.Clock, checkpointID pgtype.UUID, specs []actionSpec) (created, updated int, deactivated []string, err error) {
	current, err := q.ListCheckpointActions(ctx, checkpointID)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("list checkpoint actions: %w", err)
	}
	active := make(map[string]bool, len(current))
	for _, a := range current {
		active[uuidString(a.ID)] = true
	}
	// Refuse unknown ids before writing anything, and name the one that is wrong. An action that was
	// deactivated is not found either: to bring one back, send it without an id.
	var problems []apperr.FieldError
	for i, s := range specs {
		if s.id.Valid && !active[uuidString(s.id)] {
			problems = append(problems, apperr.FieldError{Field: fmt.Sprintf("actions[%d].id", i), Code: "INVALID", Message: "Aksi tidak ditemukan di checkpoint ini."})
		}
	}
	if len(problems) > 0 {
		return 0, 0, nil, actionInvalid(problems...)
	}

	now := ts(c.Now())
	keep := make([]pgtype.UUID, 0, len(specs))
	for i, s := range specs {
		if !s.id.Valid {
			row, err := q.InsertCheckpointAction(ctx, db.InsertCheckpointActionParams{
				CheckpointID: checkpointID, SortOrder: int32(i), Action: s.action, Instruction: textOrNull(s.instruction), //nolint:gosec // at most 30
				MinPhotos: s.minPhotos, MaxPhotos: s.maxPhotos, IsNoteRequired: s.isNoteRequired, Now: now,
			})
			if err != nil {
				return 0, 0, nil, fmt.Errorf("insert checkpoint action: %w", err)
			}
			keep = append(keep, row.ID)
			created++
			continue
		}
		row, err := q.UpdateCheckpointAction(ctx, db.UpdateCheckpointActionParams{
			ID: s.id, CheckpointID: checkpointID, SortOrder: int32(i), Action: s.action, Instruction: textOrNull(s.instruction), //nolint:gosec // at most 30
			MinPhotos: s.minPhotos, MaxPhotos: s.maxPhotos, IsNoteRequired: s.isNoteRequired, Now: now,
		})
		if err != nil {
			return 0, 0, nil, fmt.Errorf("update checkpoint action: %w", err)
		}
		keep = append(keep, row.ID)
		updated++
	}
	ids, err := q.DeactivateCheckpointActionsExcept(ctx, db.DeactivateCheckpointActionsExceptParams{CheckpointID: checkpointID, KeepIds: keep, Now: now})
	if err != nil {
		return 0, 0, nil, fmt.Errorf("deactivate checkpoint actions: %w", err)
	}
	deactivated = make([]string, 0, len(ids))
	for _, id := range ids {
		deactivated = append(deactivated, uuidString(id))
	}
	return created, updated, deactivated, nil
}
