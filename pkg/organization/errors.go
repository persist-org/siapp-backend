// Package organization owns SiAPP's operating hierarchy: koordinator wilayah → kantor cabang
// → klien → proyek → regu, the people in each regu, and their shifts (module 02).
//
// ⚠ D17/D19. The hierarchy is SiAPP master data, not a copy of Personalia's org structure,
// and every row is identified by its code (OS1, 1K, 1K-IBMI, 1K-IBMI-S, 1K-IBMI-S1), never by
// a number. A code contains its parent's code, so it is never edited after creation. People
// stay in Personalia: this package only says which regu they serve in.
//
// Phase 02.1 starts it with the regions; the rest of the hierarchy follows in 02.2.
package organization

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Error reasons (details.reason). A reason that has shipped is never renamed. They keep the
// names the plan (docs/plan/02-organization.md) gives them.
const (
	ReasonRegionNotFound    = "REGION_NOT_FOUND"
	ReasonCodeTaken         = "CODE_TAKEN"
	ReasonHasActiveChildren = "HAS_ACTIVE_CHILDREN"
)

// ErrRegionNotFound is returned for a koordinator wilayah that does not exist (or was deleted).
var ErrRegionNotFound = apperr.New(http.StatusNotFound, apperr.CodeNotFound,
	ReasonRegionNotFound, "Koordinator wilayah tidak ditemukan.")

// ErrCodeTaken is returned when a code is already used, including by a deleted row: a code is
// never handed out twice (D19), because records made under it still point at it.
var ErrCodeTaken = apperr.New(http.StatusConflict, apperr.CodeDuplicate,
	ReasonCodeTaken, "Kode sudah dipakai.")

// ErrHasActiveChildren is returned when something is deleted while rows below it (or admins
// scoped to it) still depend on it.
var ErrHasActiveChildren = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonHasActiveChildren, "Masih ada data aktif di bawahnya. Pindahkan atau hapus terlebih dahulu.")
