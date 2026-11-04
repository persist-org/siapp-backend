package masterdata

import (
	"context"
	"errors"
	"fmt"

	"siapp/pkg/platform/apperr"
	"siapp/pkg/platform/auth"
	"siapp/pkg/platform/personel"
)

// LeaveCategory is a leave category (Sakit, Izin, Cuti), as Personel defines it.
type LeaveCategory struct {
	ID   int64
	Name string
}

// LeaveCategories returns the leave categories of the caller's organization.
//
// ⚠ SiAPP stores none, and has no admin endpoint for them: leave belongs to Personel (LOCK/01 §2).
// A table here would be a second list that drifts from the one HR maintains, and a leave request
// made under a category Personel does not know would be refused there. The list is read through
// the Personel cache, so an outage of Personel is 503 PERSONEL_UNAVAILABLE rather than an empty
// list the app would take as "no categories".
func (s *Service) LeaveCategories(ctx context.Context, p auth.Principal) ([]LeaveCategory, error) {
	if s.leave == nil {
		return nil, apperr.ErrPersonelUnavailable
	}
	types, err := s.leave.LeaveTypes(ctx, p.OrganizationID)
	if errors.Is(err, personel.ErrUnavailable) {
		return nil, apperr.ErrPersonelUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("leave types: %w", err)
	}
	out := make([]LeaveCategory, 0, len(types))
	for _, t := range types {
		out = append(out, LeaveCategory{ID: t.ID, Name: t.Name})
	}
	return out, nil
}
