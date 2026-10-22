package personel

import "slices"

// Group says which Personalia companies form the one SiAPP organization.
//
// ⚠ D17. SiAPP's tenant is the whole group (KPI, IDS and PCD), not the employing company: a
// koordinator wilayah spans all three, and its admin must see all three. The company is kept
// on the person for display. This is the one place that maps a company to a tenant, so the
// rest of SiAPP only ever sees Identity.OrganizationID.
//
// The zero Group (no CompanyIDs) makes the company its own tenant, as before D17. Only
// tests and the live contract test use that: config refuses to start without
// GROUP_COMPANY_IDS, so a deployed API always has a group.
type Group struct {
	// CompanyIDs are the Personalia company ids (m_companies.id) that belong to the group.
	CompanyIDs []int64
	// OrganizationID and OrganizationName are the group's one organization.
	OrganizationID   int64
	OrganizationName string
}

// tenant returns the organization of a person employed by the company. ok is false when the
// company is outside the group: such a person cannot sign in, and a lookup does not find
// them.
func (g Group) tenant(companyID int64, companyName string) (orgID int64, orgName string, ok bool) {
	if len(g.CompanyIDs) == 0 {
		return companyID, companyName, true
	}
	if !slices.Contains(g.CompanyIDs, companyID) {
		return 0, "", false
	}
	return g.OrganizationID, g.OrganizationName, true
}
