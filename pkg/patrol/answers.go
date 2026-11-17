package patrol

// AnswerEntry is one question of the old zone checklist with its answer, as a scan made before the
// checkpoints kept it in patrol_scans.answers. The zone's checklist is gone (Phase 10.9) and a new scan
// stores [] there: its answers are rows of patrol_scan_actions. This stays only so the views of the old
// scans can still be read; Phase 10.10 replaces the views and drops the column.
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
	// Value is nil when the question was left unanswered.
	Value    any     `json:"value"`
	Note     *string `json:"note"`
	MediaIDs []int64 `json:"media_ids"`

	// Photos are the answer's photos as signed URLs, filled when a scan is read. Never stored.
	Photos []Photo `json:"-"`
}
