package patrol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"siapp/internal/db"
)

// maxTextAnswer is the longest TEXT answer, in characters.
const maxTextAnswer = 2000

// AnswerInput is the guard's answer to one question. Value is a boolean, a string, a number or an
// option value, by the question's answer_type; it is raw JSON so that the type can be judged
// against the question instead of being fixed by the struct.
type AnswerInput struct {
	ItemID   int64           `json:"item_id" validate:"required,min=1"`
	Value    json.RawMessage `json:"value"`
	Note     *string         `json:"note" validate:"omitempty,max=1000"`
	MediaIDs []int64         `json:"media_ids" validate:"omitempty,max=5,dive,min=1"`
}

// AnswerEntry is one question with its answer, as a scan keeps it. It copies the question as it
// was when the guard scanned, so a later edit of the checklist never rewrites history.
type AnswerEntry struct {
	ItemID     int64    `json:"item_id"`
	Label      string   `json:"label"`
	AnswerType string   `json:"answer_type"`
	Options    []Option `json:"options,omitempty"`
	Required   bool     `json:"required"`
	PhotoMode  string   `json:"photo_mode"`
	PhotoMax   int      `json:"photo_max"`
	NoteMode   string   `json:"note_mode"`
	AttachWhen string   `json:"attach_when"`
	// Value is nil when the question was left unanswered (only possible when it is not required).
	Value    any     `json:"value"`
	Note     *string `json:"note"`
	MediaIDs []int64 `json:"media_ids"`

	// Photos are the answer's photos as signed URLs, filled when a scan is read. Never stored.
	Photos []Photo `json:"-"`
}

// checkAnswers judges the answers of a scan against the zone's checklist as it was when the guard
// scanned (at), and returns one entry per question that applied, in the checklist's order.
//
// items is every item of the zone, deactivated ones too. The rules, in the order a client can act
// on them:
//   - an answer for an item that is not of this zone, that was deactivated before the scan, or that
//     appears twice → CHECKLIST_ITEM_INVALID;
//   - a required question with no answer → CHECKLIST_ANSWER_REQUIRED;
//   - a value of the wrong type, a CHOICE value outside its options, or more photos than photo_max →
//     CHECKLIST_ANSWER_INVALID, and so is a photo or a note sent where the question has none;
//   - a photo or a note the question demands, and is missing → CHECKLIST_ATTACHMENT_REQUIRED.
//
// ⚠ An offline scan is judged by the checklist as it was at `at`, not as it is now. A guard who
// scanned offline yesterday must not be rejected because the admin edited the checklist this
// morning:
//   - an item deactivated after `at` is ignored, and so is any answer to it: the guard saw it, the
//     form no longer has it, and the answer is dropped, not refused;
//   - an item created after `at` is not required: the guard could not have seen it. If they answered
//     it anyway it is judged like any other.
//
// An item that was only edited in place is judged by its definition today: edits are not versioned,
// so the old definition is gone. In practice that means an edit that changes the type of a question
// can refuse an old offline scan, which is the one case this cannot cover.
//
// It also returns the photos the answers carry, so the caller can count them and attach them.
func checkAnswers(items []db.PatrolChecklistItem, answers []AnswerInput, at time.Time) (entries []AnswerEntry, mediaIDs []int64, err error) {
	byID := make(map[int64]db.PatrolChecklistItem, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	answered := make(map[int64]AnswerInput, len(answers))
	for _, a := range answers {
		it, ok := byID[a.ItemID]
		if !ok {
			return nil, nil, itemError(errChecklistItemInvalid, a.ItemID, "", "bukan butir checklist zona ini.")
		}
		if _, dup := answered[a.ItemID]; dup {
			return nil, nil, itemError(errChecklistItemInvalid, a.ItemID, it.Label, "dijawab lebih dari sekali.")
		}
		if !it.Active && (!it.DeactivatedAt.Valid || !it.DeactivatedAt.Time.After(at)) {
			return nil, nil, itemError(errChecklistItemInvalid, a.ItemID, it.Label, "sudah tidak aktif.")
		}
		answered[a.ItemID] = a
	}

	// Never nil: it is stored as a JSON array, and a zone with no checklist has an empty one.
	entries = make([]AnswerEntry, 0, len(items))
	for _, it := range items {
		if !it.Active {
			continue // deactivated after the scan: the question and any answer to it are ignored
		}
		existed := !it.CreatedAt.Time.After(at)
		a, has := answered[it.ID]
		if !has && !existed {
			continue // created after the scan: not part of it
		}
		required := it.Required && existed
		entry, err := checkOne(it, a, has, required)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, entry)
		mediaIDs = append(mediaIDs, entry.MediaIDs...)
	}
	return entries, mediaIDs, nil
}

// checkOne judges the answer to one question (has is false when there is none) and returns its
// entry.
func checkOne(it db.PatrolChecklistItem, a AnswerInput, has, required bool) (AnswerEntry, error) {
	entry := AnswerEntry{
		ItemID: it.ID, Label: it.Label, AnswerType: string(it.AnswerType), Required: required,
		PhotoMode: string(it.PhotoMode), PhotoMax: int(it.PhotoMax), NoteMode: string(it.NoteMode),
		AttachWhen: string(it.AttachWhen), MediaIDs: []int64{},
	}
	if it.AnswerType == db.ChecklistAnswerTypeCHOICE {
		if err := json.Unmarshal(it.Options, &entry.Options); err != nil {
			return AnswerEntry{}, fmt.Errorf("checklist item %d: decode options: %w", it.ID, err)
		}
	}

	value, hasValue, problem := parseValue(it, entry.Options, a.Value)
	if problem != "" {
		return AnswerEntry{}, itemError(errAnswerInvalid, it.ID, it.Label, problem)
	}
	if !hasValue {
		if required {
			return AnswerEntry{}, itemError(errAnswerRequired, it.ID, it.Label, "")
		}
		// An optional question left alone: its entry is kept, empty, so the scan shows it was asked.
		if !has {
			return entry, nil
		}
	}
	entry.Value = value
	if has {
		entry.MediaIDs = append(entry.MediaIDs, a.MediaIDs...)
		if note := strings.TrimSpace(deref(a.Note)); note != "" {
			entry.Note = &note
		}
	}

	if len(entry.MediaIDs) > 0 && it.PhotoMode == db.ChecklistAttachModeNONE {
		return AnswerEntry{}, itemError(errAnswerInvalid, it.ID, it.Label, "pertanyaan ini tidak menerima foto.")
	}
	if len(entry.MediaIDs) > int(it.PhotoMax) {
		return AnswerEntry{}, itemError(errAnswerInvalid, it.ID, it.Label, fmt.Sprintf("maksimum %d foto.", it.PhotoMax))
	}
	if entry.Note != nil && it.NoteMode == db.ChecklistAttachModeNONE {
		return AnswerEntry{}, itemError(errAnswerInvalid, it.ID, it.Label, "pertanyaan ini tidak menerima catatan.")
	}

	// A photo or a note that is demanded. Only an answered question demands anything, and with
	// attach_when ON_NO only the answer "no" does: that is the "report a problem with proof" case.
	if hasValue && demanded(it, value) {
		if it.PhotoMode == db.ChecklistAttachModeREQUIRED && len(entry.MediaIDs) == 0 {
			return AnswerEntry{}, itemError(errAttachmentRequired, it.ID, it.Label, "foto wajib dilampirkan.")
		}
		if it.NoteMode == db.ChecklistAttachModeREQUIRED && entry.Note == nil {
			return AnswerEntry{}, itemError(errAttachmentRequired, it.ID, it.Label, "catatan wajib diisi.")
		}
	}
	return entry, nil
}

// demanded says whether the question's REQUIRED attachments are owed for this answer.
func demanded(it db.PatrolChecklistItem, value any) bool {
	if it.AttachWhen == db.ChecklistAttachWhenONNO {
		b, ok := value.(bool)
		return ok && !b
	}
	return true
}

// parseValue reads the raw value as the question's type. hasValue is false for a missing value, a
// null, and a TEXT that is blank: none of them answers the question. problem is a sentence for the
// client when the value is of the wrong kind, "" otherwise.
func parseValue(it db.PatrolChecklistItem, options []Option, raw json.RawMessage) (value any, hasValue bool, problem string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, false, ""
	}
	switch it.AnswerType {
	case db.ChecklistAnswerTypeBOOLEAN:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, false, "harus true atau false."
		}
		return b, true, ""
	case db.ChecklistAnswerTypeTEXT:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false, "harus berupa teks."
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, false, ""
		}
		if len([]rune(s)) > maxTextAnswer {
			return nil, false, fmt.Sprintf("maksimum %d karakter.", maxTextAnswer)
		}
		return s, true, ""
	case db.ChecklistAnswerTypeNUMBER:
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, false, "harus berupa angka."
		}
		return n, true, ""
	case db.ChecklistAnswerTypeCHOICE:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false, "harus berupa nilai salah satu opsi."
		}
		for _, o := range options {
			if o.Value == s {
				return s, true, ""
			}
		}
		return nil, false, "bukan salah satu opsi."
	}
	return nil, false, "tipe pertanyaan tidak dikenal."
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
