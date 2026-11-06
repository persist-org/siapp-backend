package attendance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"siapp/internal/db"
	"siapp/pkg/platform/timeutil"
)

// eventPayload is what personel_outbox.payload holds for an attendance event. It is the evidence
// SiAPP captured, by reference: no photo bytes, and no NIK or Personel project id, which the
// delivery (Phase 07.3) adds when it sends, from the person and the project as they are THEN.
// Keeping them out of the stored row means a NIK is never copied into a table nobody scrubs.
type eventPayload struct {
	Type         string `json:"type"`
	AttendanceID int64  `json:"attendance_id"`
	PersonnelID  int64  `json:"personnel_id"`
	TeamCode     string `json:"team_code"`
	ProjectCode  string `json:"project_code"`
	WorkDate     string `json:"work_date"`
	ShiftID      int64  `json:"shift_id"`
	ShiftStarts  string `json:"shift_starts_at"`
	ShiftEnds    string `json:"shift_ends_at"`
	Timezone     string `json:"timezone"`
	Status       string `json:"status"`
	// OccurredAt is when the change happened, which for a check-in or check-out is its own time.
	OccurredAt string `json:"occurred_at"`

	CheckInAt     *string `json:"check_in_at"`
	CheckInMethod *string `json:"check_in_method"`
	CheckInInArea *bool   `json:"check_in_within_area"`
	CheckInMedia  *int64  `json:"check_in_media_id"`
	// CheckInQRDanruID is the DANRU whose QR the check-in scanned, null for any other method.
	CheckInQRDanruID *int64 `json:"check_in_qr_danru_id"`
	LateMinutes      int    `json:"late_minutes"`

	CheckOutAt     *string `json:"check_out_at"`
	CheckOutMethod *string `json:"check_out_method"`
	CheckOutInArea *bool   `json:"check_out_within_area"`
	CheckOutMedia  *int64  `json:"check_out_media_id"`
	// CheckOutQRDanruID is the same for the check-out.
	CheckOutQRDanruID *int64 `json:"check_out_qr_danru_id"`

	// ShiftSwapID is set when the shift is a covered one (07.7): Personel credits it to the replacer.
	ShiftSwapID *int64 `json:"shift_swap_id"`
}

// writeEvent writes the Personel outbox row for a change of an attendance, through q: the
// transaction that makes the change. Every capture usecase calls it, so a change of the row and its
// event commit or roll back together: Personel is never told of something that did not happen, and
// never misses something that did.
//
// The row is PENDING and due now; the worker of Phase 07.3 delivers it, in order per attendance.
func writeEvent(ctx context.Context, q *db.Queries, a db.Attendance, eventType string, now time.Time) error {
	payload, err := json.Marshal(eventPayload{
		Type: eventType, AttendanceID: a.ID, PersonnelID: a.PersonnelID, TeamCode: a.TeamCode, ProjectCode: a.ProjectCode,
		WorkDate: dateValue(a.WorkDate).String(), ShiftID: a.ShiftID,
		ShiftStarts: timeutil.FormatTimestamp(a.ShiftStartsAt.Time), ShiftEnds: timeutil.FormatTimestamp(a.ShiftEndsAt.Time),
		Timezone: a.Timezone, Status: string(a.Status), OccurredAt: timeutil.FormatTimestamp(now),
		CheckInAt: formatted(a.CheckInAt), CheckInMethod: methodString(a.CheckInMethod),
		CheckInInArea: boolPtr(a.CheckInWithinArea), CheckInMedia: int8Ptr(a.CheckInMediaID), CheckInQRDanruID: int8Ptr(a.CheckInQrDanruID), LateMinutes: int(a.LateMinutes),
		CheckOutAt: formatted(a.CheckOutAt), CheckOutMethod: methodString(a.CheckOutMethod),
		CheckOutInArea: boolPtr(a.CheckOutWithinArea), CheckOutMedia: int8Ptr(a.CheckOutMediaID), CheckOutQRDanruID: int8Ptr(a.CheckOutQrDanruID),
		ShiftSwapID: int8Ptr(a.ShiftSwapID),
	})
	if err != nil {
		return fmt.Errorf("encode %s event: %w", eventType, err)
	}
	if _, err := q.InsertPersonelOutbox(ctx, db.InsertPersonelOutboxParams{
		Kind: db.OutboxKindATTENDANCE, AggregateID: a.ID, Payload: payload, Now: ts(now),
	}); err != nil {
		return fmt.Errorf("insert %s event: %w", eventType, err)
	}
	return nil
}
