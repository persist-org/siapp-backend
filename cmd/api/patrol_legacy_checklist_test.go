package main

import (
	"encoding/json"

	"github.com/stretchr/testify/require"
)

// The zone's checklist is gone from the API (Phase 10.8: the checklist moved to the checkpoint), but
// the scan still reads the old table until Phase 10.9 rewrites it. These helpers put checklist items
// straight into patrol_checklist_items, as the admin's API used to, so the scan tests written for the
// zone checklist keep running until then. Delete this file together with those tests in 10.9.

// legacyDefaults are what the old admin API stored for a setting the body left out.
func legacyDefaults(it map[string]any) checklistOut {
	str := func(key, def string) string {
		if v, ok := it[key].(string); ok {
			return v
		}
		return def
	}
	out := checklistOut{
		Label: str("label", ""), AnswerType: str("answer_type", "TEXT"), Required: true,
		PhotoMode: str("photo_mode", "NONE"), PhotoMax: 1, NoteMode: str("note_mode", "NONE"), AttachWhen: str("attach_when", "ALWAYS"),
	}
	if v, ok := it["required"].(bool); ok {
		out.Required = v
	}
	if v, ok := it["photo_max"].(int); ok {
		out.PhotoMax = v
	}
	if v, ok := it["help_text"].(string); ok {
		out.HelpText = &v
	}
	if v, ok := it["options"].([]map[string]string); ok {
		for _, o := range v {
			out.Options = append(out.Options, optionOut{Value: o["value"], Label: o["label"]})
		}
	}
	return out
}

// legacyWrite stores one item of a zone's checklist: a new row, or the row `id` when it is not zero,
// with the settings the body left out put back to their defaults.
func (f *flow) legacyWrite(zoneID int64, order int, id int64, it map[string]any) checklistOut {
	f.t.Helper()
	item := legacyDefaults(it)
	item.SortOrder = order
	var options any
	if len(item.Options) > 0 {
		raw, err := json.Marshal(item.Options)
		require.NoError(f.t, err)
		options = string(raw)
	}
	var help any
	if item.HelpText != nil {
		help = *item.HelpText
	}
	if id == 0 {
		f.query([]any{&id}, `INSERT INTO patrol_checklist_items (zone_id, sort_order, label, help_text, answer_type, options, required,
		                                                         photo_mode, photo_max, note_mode, attach_when)
		                     VALUES ($1, $2, $3, $4, $5::checklist_answer_type, $6::jsonb, $7, $8::checklist_attach_mode, $9,
		                             $10::checklist_attach_mode, $11::checklist_attach_when) RETURNING id`,
			zoneID, order, item.Label, help, item.AnswerType, options, item.Required, item.PhotoMode, item.PhotoMax, item.NoteMode, item.AttachWhen)
	} else {
		f.exec(`UPDATE patrol_checklist_items SET sort_order = $2, label = $3, help_text = $4, answer_type = $5::checklist_answer_type,
		               options = $6::jsonb, required = $7, photo_mode = $8::checklist_attach_mode, photo_max = $9,
		               note_mode = $10::checklist_attach_mode, attach_when = $11::checklist_attach_when, updated_at = now()
		        WHERE id = $1`,
			id, order, item.Label, help, item.AnswerType, options, item.Required, item.PhotoMode, item.PhotoMax, item.NoteMode, item.AttachWhen)
	}
	item.ID = id
	return item
}

// legacyChecklist gives a new zone its checklist and returns the items as the API used to return them.
func (f *flow) legacyChecklist(zoneID int64, items []map[string]any) []checklistOut {
	f.t.Helper()
	out := make([]checklistOut, 0, len(items))
	for i, it := range items {
		out = append(out, f.legacyWrite(zoneID, i, 0, it))
	}
	return out
}

// legacyReplaceChecklist is what PUT /admin/zones/{id}/checklist did: an item with an id is updated, one
// without is created, and every active item left out is deactivated, never deleted.
func (f *flow) legacyReplaceChecklist(zoneID int64, items []map[string]any) []checklistOut {
	f.t.Helper()
	out := make([]checklistOut, 0, len(items))
	keep := make([]int64, 0, len(items))
	for i, it := range items {
		id, _ := it["id"].(int64)
		written := f.legacyWrite(zoneID, i, id, it)
		keep = append(keep, written.ID)
		out = append(out, written)
	}
	f.exec(`UPDATE patrol_checklist_items SET active = false, deactivated_at = now(), updated_at = now()
	        WHERE zone_id = $1 AND active AND NOT (id = ANY($2::bigint[]))`, zoneID, keep)
	return out
}
