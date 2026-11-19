package patrol

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"siapp/internal/db"
)

// maxActionNote is the longest note a guard writes on one action, in characters.
const maxActionNote = 1000

// ActionAnswerInput is the guard's answer to one action of the checkpoint: the note and the photos. The
// checkpoint's checklist has no questions with values: doing the action and showing it is the answer.
type ActionAnswerInput struct {
	// ActionID is the uuid of the action. It is judged in checkActions, not by a tag, so that an id that is
	// not a uuid is CHECKLIST_ITEM_INVALID like any other id that is not this checkpoint's.
	ActionID string  `json:"action_id" validate:"required,max=100"`
	Note     *string `json:"note" validate:"omitempty,max=1000"`
	// MediaIDs are the photos of this action (POST /media with the purpose PATROL_SCAN).
	MediaIDs []int64 `json:"media_ids" validate:"omitempty,max=20,dive,min=1"`
	// DeviceCondition is the phone's condition when this action was answered; the scan's own is the default
	// for an action without one.
	DeviceCondition json.RawMessage `json:"device_condition"`
}

// actionEntry is one answered action: the action as it stood when the guard scanned, and what they gave.
type actionEntry struct {
	action          db.PatrolCheckpointAction
	note            *string
	mediaIDs        []int64
	deviceCondition json.RawMessage // nil when neither the action nor the scan had one
}

// checkActions judges the answers of a scan against the checkpoint's checklist as it was when the guard
// scanned (at), and returns one entry per action that applied, in the checklist's order, with the photos
// they carry so the caller can attach them.
//
// actions is every action of the checkpoint, the deactivated ones too. The rules, in the order a client
// can act on them:
//   - an answer for an action that is not this checkpoint's, that was deactivated before the scan, or that
//     appears twice → CHECKLIST_ITEM_INVALID;
//   - an action that was on the checklist and has no answer → CHECKLIST_ANSWER_REQUIRED (every action is
//     required: there is no "skip");
//   - more photos than max_photos, or the same photo used twice in the scan → CHECKLIST_ANSWER_INVALID;
//   - fewer photos than min_photos, or a blank note where is_note_required → CHECKLIST_ATTACHMENT_REQUIRED.
//
// ⚠ An offline scan is judged by the checklist as it was at `at`, not as it is now. A guard who scanned
// offline yesterday must not be rejected because the admin edited the checklist this morning:
//   - an action deactivated after `at` is ignored, and so is any answer to it: the guard saw it, the
//     checklist no longer has it, and the answer is dropped, not refused;
//   - an action created after `at` is not required: the guard could not have seen it. If they answered it
//     anyway it is judged like any other.
//
// An action that was only edited in place is judged by its definition today: edits are not versioned, so the
// old definition is gone. An edit that raises min_photos can refuse an old offline scan, which is the one
// case this cannot cover.
func checkActions(actions []db.PatrolCheckpointAction, in []ActionAnswerInput, at time.Time) (entries []actionEntry, mediaIDs []int64, err error) {
	byID := make(map[string]db.PatrolCheckpointAction, len(actions))
	for _, a := range actions {
		byID[uuidString(a.ID)] = a
	}
	answered := make(map[string]ActionAnswerInput, len(in))
	for _, ans := range in {
		id, ok := parseUUID(ans.ActionID)
		a, found := byID[uuidString(id)]
		if !ok || !found {
			return nil, nil, actionError(errChecklistItemInvalid, ans.ActionID, "", "bukan aksi checkpoint ini.")
		}
		key := uuidString(id)
		if _, dup := answered[key]; dup {
			return nil, nil, actionError(errChecklistItemInvalid, key, a.Action, "dijawab lebih dari sekali.")
		}
		if !a.Active && (!a.DeactivatedAt.Valid || !a.DeactivatedAt.Time.After(at)) {
			return nil, nil, actionError(errChecklistItemInvalid, key, a.Action, "sudah tidak aktif.")
		}
		answered[key] = ans
	}

	// Never nil: every caller ranges over it.
	entries = make([]actionEntry, 0, len(actions))
	usedMedia := map[int64]bool{}
	for _, a := range actions {
		if !a.Active {
			continue // deactivated after the scan: the action and any answer to it are ignored
		}
		key := uuidString(a.ID)
		existed := !a.CreatedAt.Time.After(at)
		ans, has := answered[key]
		if !has {
			if !existed {
				continue // created after the scan: not part of it
			}
			return nil, nil, actionError(errAnswerRequired, key, a.Action, "")
		}

		photos := len(ans.MediaIDs)
		if photos > int(a.MaxPhotos) {
			why := fmt.Sprintf("paling banyak %d foto.", a.MaxPhotos)
			if a.MaxPhotos == 0 {
				why = "aksi ini tidak menerima foto."
			}
			return nil, nil, actionError(errAnswerInvalid, key, a.Action, why)
		}
		for _, m := range ans.MediaIDs {
			if usedMedia[m] {
				return nil, nil, actionError(errAnswerInvalid, key, a.Action, "foto yang sama dipakai lebih dari sekali dalam satu scan.")
			}
			usedMedia[m] = true
		}
		if photos < int(a.MinPhotos) {
			return nil, nil, actionError(errAttachmentRequired, key, a.Action, fmt.Sprintf("minimal %d foto wajib dilampirkan.", a.MinPhotos))
		}
		note := nonBlank(ans.Note)
		if a.IsNoteRequired && note == nil {
			return nil, nil, actionError(errAttachmentRequired, key, a.Action, "catatan wajib diisi.")
		}
		if note != nil && utf8.RuneCountInString(*note) > maxActionNote {
			return nil, nil, actionError(errAnswerInvalid, key, a.Action, fmt.Sprintf("catatan paling panjang %d karakter.", maxActionNote))
		}

		entries = append(entries, actionEntry{
			action: a, note: note, mediaIDs: append([]int64{}, ans.MediaIDs...), deviceCondition: ans.DeviceCondition,
		})
		mediaIDs = append(mediaIDs, ans.MediaIDs...)
	}
	return entries, mediaIDs, nil
}

// actionSnapshot is the answered action as one JSON object: the action as it stood at scan time (so a
// later edit never rewrites it) and the answer. It is stored in patrol_scan_actions.snapshot, next to the
// note and media_ids columns that the code reads: both are written from the same values by the same insert,
// and the row is never updated, so they cannot drift apart. The shape is what the table's CHECK expects.
func actionSnapshot(e actionEntry) (json.RawMessage, error) {
	var note any
	if e.note != nil {
		note = *e.note
	}
	raw, err := json.Marshal(map[string]any{
		"action":           e.action.Action,
		"instruction":      textPtr(e.action.Instruction),
		"min_photos":       e.action.MinPhotos,
		"max_photos":       e.action.MaxPhotos,
		"is_note_required": e.action.IsNoteRequired,
		"sort_order":       e.action.SortOrder,
		"note":             note,
		"media_ids":        e.mediaIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("encode action snapshot: %w", err)
	}
	return raw, nil
}

// withDeviceConditions parses the device condition of the scan and of each action, and gives each action
// its own or, failing that, the scan's. It returns the answers with the condition each will be stored with
// (nil when there is none). Errors name the field: device_condition, or actions[i].device_condition.
func withDeviceConditions(scan json.RawMessage, in []ActionAnswerInput) ([]ActionAnswerInput, error) {
	def, err := parseDeviceCondition(scan, "device_condition")
	if err != nil {
		return nil, err
	}
	out := make([]ActionAnswerInput, len(in))
	for i, a := range in {
		out[i] = a
		own, err := parseDeviceCondition(a.DeviceCondition, fmt.Sprintf("actions[%d].device_condition", i))
		if err != nil {
			return nil, err
		}
		if own == nil {
			own = def
		}
		out[i].DeviceCondition = own
	}
	return out, nil
}

// subjectOf is the subject of a scan: the one the app sent, or the checkpoint's name.
func subjectOf(sent *string, checkpointName string) string {
	if s := strings.TrimSpace(derefString(sent)); s != "" {
		return s
	}
	return checkpointName
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
