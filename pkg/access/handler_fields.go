package access

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"siapp/internal/apigen"
	"siapp/pkg/platform/httpx"
)

// grantsRequest is the body of PUT /admin/field-permissions/{team_role}. The generated
// contract type decodes the body (unknown fields and wrong types are refused there); this is
// the input the rules are checked on. Grants stays nil when the client sent none, so
// "required" can tell "no list" from "an empty list".
//
// The cap of 200 (the contract's maxItems) is far above the catalog's size, so a longer list
// is wrong whatever it holds, and the cap keeps the error list short.
type grantsRequest struct {
	Grants []grantRequest `json:"grants" validate:"required,max=200,dive"`
}

type grantRequest struct {
	Permission string `json:"permission" validate:"required,max=64"`
	Scope      string `json:"scope" validate:"required,max=16"`
}

func (h *Handler) listFieldPermissions(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	matrix, err := h.fields.List(r.Context(), p, scope)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	catalog := make([]apigen.FieldPermissionDefinition, 0, len(matrix.Catalog))
	for _, e := range matrix.Catalog {
		lines := make([]apigen.BusinessLine, 0, len(e.BusinessLines))
		for _, l := range e.BusinessLines {
			lines = append(lines, apigen.BusinessLine(l))
		}
		catalog = append(catalog, apigen.FieldPermissionDefinition{
			Permission: string(e.Key), Module: e.Module, Description: e.Description, BusinessLines: lines,
			Defaults: apigen.FieldPermissionDefaults{
				DANRU:  apigen.FieldPermissionScope(e.Defaults[RoleDanru]),
				MEMBER: apigen.FieldPermissionScope(e.Defaults[RoleMember]),
			},
		})
	}
	roles := make([]apigen.FieldPermissionRole, 0, len(matrix.Roles))
	for _, role := range matrix.Roles {
		roles = append(roles, toRoleAPI(role))
	}
	httpx.OK(w, r, http.StatusOK, apigen.FieldPermissionMatrix{Catalog: catalog, Roles: roles})
}

func (h *Handler) putFieldPermissions(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := caller(w, r)
	if !ok {
		return
	}
	var req apigen.FieldPermissionsInput
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := grantsRequest{}
	if req.Grants != nil {
		in.Grants = make([]grantRequest, 0, len(req.Grants))
		for _, g := range req.Grants {
			in.Grants = append(in.Grants, grantRequest{Permission: g.Permission, Scope: string(g.Scope)})
		}
	}
	if err := httpx.Validate(&in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	grants := make([]Grant, 0, len(in.Grants))
	for _, g := range in.Grants {
		grants = append(grants, Grant{Permission: Permission(g.Permission), Scope: Scope(g.Scope)})
	}
	// The role is not checked here: Replace answers a REGION_ADMIN with 403 before it looks at
	// anything else, and a role that is not DANRU or MEMBER with 404.
	out, err := h.fields.Replace(r.Context(), p, scope, TeamRole(chi.URLParam(r, "team_role")), grants)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.OK(w, r, http.StatusOK, toRoleAPI(out))
}

func toRoleAPI(r RoleGrants) apigen.FieldPermissionRole {
	grants := make([]apigen.FieldPermissionGrant, 0, len(r.Grants))
	for _, g := range r.Grants {
		grants = append(grants, apigen.FieldPermissionGrant{Permission: string(g.Permission), Scope: apigen.FieldPermissionScope(g.Scope)})
	}
	return apigen.FieldPermissionRole{TeamRole: apigen.TeamRole(r.TeamRole), Grants: grants}
}
