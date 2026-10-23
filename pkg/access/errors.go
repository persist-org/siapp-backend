package access

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Error reasons (details.reason). A reason that has shipped is never renamed.
//
// NOT_ADMIN keeps the name the plan and docs/api-response.md already give it (it is the
// reason of the web login gate too); the others carry the module prefix that
// docs/api-response.md asks for.
const (
	ReasonNotAdmin      = "NOT_ADMIN"
	ReasonOrgAdminOnly  = "ACCESS_ORG_ADMIN_ONLY"
	ReasonOwnRoleLocked = "ACCESS_OWN_ROLE_PROTECTED"
	// ReasonOutsideScope keeps the wire value it shipped with in Phase 01.3, when the scope
	// was a list of projects; it now means "outside your region".
	ReasonOutsideScope = "ACCESS_OUTSIDE_ADMIN_SCOPE"
	// ReasonPermissionUnknown keeps the name the plan gives it (Phase 03), without the module
	// prefix, like NOT_ADMIN.
	ReasonPermissionUnknown = "PERMISSION_UNKNOWN"
	// ReasonPermissionDenied and ReasonFeatureNotEnabled are what the field routes answer
	// (Phase 03.2); same naming as the plan.
	ReasonPermissionDenied  = "PERMISSION_DENIED"
	ReasonFeatureNotEnabled = "FEATURE_NOT_ENABLED"
)

// ErrNotAdmin is returned to a person with no web role assignment: field staff, who cannot
// sign in on the web and have no business on an /admin route.
var ErrNotAdmin = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonNotAdmin, "Anda bukan admin. Akses ini hanya untuk admin.")

// ErrOrgAdminOnly is returned to a REGION_ADMIN who tries something only an ORG_ADMIN may do.
// It is a role check, so ROLE_DENIED.
var ErrOrgAdminOnly = apperr.New(http.StatusForbidden, apperr.CodeRoleDenied,
	ReasonOrgAdminOnly, "Hanya admin organisasi yang dapat melakukan ini.")

// ErrOutsideAdminScope is returned to an admin who reaches for a person or a record outside
// their region. It is a scope check, so SCOPE_DENIED.
var ErrOutsideAdminScope = apperr.New(http.StatusForbidden, apperr.CodeScopeDenied,
	ReasonOutsideScope, "Data ini berada di luar wilayah yang Anda kelola.")

// ErrPermissionUnknown is returned when a request names a permission that is not in the
// catalog. The fields say which grants.
var ErrPermissionUnknown = apperr.New(http.StatusUnprocessableEntity, apperr.CodeValidation,
	ReasonPermissionUnknown, "Ada hak akses yang tidak dikenal.")

// ErrPermissionDenied is returned to a person whose role in their regu does not give them a
// permission, or does not cover the regu they ask about.
var ErrPermissionDenied = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonPermissionDenied, "Anda tidak memiliki izin untuk tindakan ini.")

// ErrFeatureNotEnabled is returned when a feature is used on a project whose business line does
// not have it (E-Memo exists for cleaning projects only).
var ErrFeatureNotEnabled = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonFeatureNotEnabled, "Fitur ini tidak tersedia untuk proyek ini.")

// ErrOwnRoleProtected is returned when an admin tries to remove or narrow their own
// assignment, so that the last admin cannot lock everyone out.
var ErrOwnRoleProtected = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonOwnRoleLocked, "Anda tidak dapat mencabut atau mempersempit peran Anda sendiri.")
