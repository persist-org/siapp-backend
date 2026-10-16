package access

import (
	"net/http"

	"siapp/pkg/platform/apperr"
)

// Error reasons (details.reason). A reason that has shipped is never renamed.
const (
	ReasonNotAdmin       = "NOT_ADMIN"
	ReasonOrgAdminOnly   = "ORG_ADMIN_ONLY"
	ReasonOwnRoleLocked  = "OWN_ROLE_PROTECTED"
	ReasonOutsideProject = "OUTSIDE_ADMIN_SCOPE"
)

// ErrNotAdmin is returned to a person with no web role assignment: field staff, who cannot
// sign in on the web and have no business on an /admin route.
var ErrNotAdmin = apperr.New(http.StatusForbidden, apperr.CodeForbidden,
	ReasonNotAdmin, "Anda bukan admin. Akses ini hanya untuk admin.")

// ErrOrgAdminOnly is returned to a PROJECT_ADMIN who tries something only an ORG_ADMIN may do.
var ErrOrgAdminOnly = apperr.New(http.StatusForbidden, apperr.CodeScopeDenied,
	ReasonOrgAdminOnly, "Hanya admin organisasi yang dapat melakukan ini.")

// ErrOutsideAdminScope is returned to an admin who reaches for a person or project outside
// their scope.
var ErrOutsideAdminScope = apperr.New(http.StatusForbidden, apperr.CodeScopeDenied,
	ReasonOutsideProject, "Orang ini berada di luar proyek yang Anda kelola.")

// ErrOwnRoleProtected is returned when an admin tries to remove or narrow their own
// assignment, so that the last admin cannot lock everyone out.
var ErrOwnRoleProtected = apperr.New(http.StatusConflict, apperr.CodeConflict,
	ReasonOwnRoleLocked, "Anda tidak dapat mencabut atau mempersempit peran Anda sendiri.")
