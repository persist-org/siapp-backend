package patrol

import "strings"

// Answer types, attachment modes and the ON_NO rule of the zone checklist, as stored.
//
// The zone checklist is gone from the API (Phase 10.8: the checklist moved to the checkpoint). The
// table and these names stay only because the scan still reads it until Phase 10.9 rewrites the scan,
// which deletes this file's constants with it.
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

// nonBlank trims s and returns nil when nothing is left.
func nonBlank(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}
