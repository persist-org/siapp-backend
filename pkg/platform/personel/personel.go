// Package personel is the only code in SiAPP that talks to Persist Personel (Personalia).
// Domains depend on the interfaces below, never on HTTP, so tests run against the
// in-process fake (personeltest) and a later, richer Personel API only changes this package.
//
// The seams are numbered as in docs/plan/README.md:
//
//	S1 login            Auth       built (kpi-personalia-api: POST .../integrations/siapp/login)
//	S2 org              Org        interface and fake only (endpoints proposed)
//	S3 attendance       Attendance interface and fake only (endpoints proposed)
//	S4 employees        Directory  built (POST .../integrations/siapp/employees)
//	S5 leave            Leave      interface and fake only (endpoints proposed)
//	S6 change events    -          webhooks, received in Phase 05.3
//
// Client implements Auth and Directory. Org, Attendance and Leave are implemented by
// personeltest.Fake until the endpoints exist; the phase of the module that first needs
// one adds its HTTP implementation here.
package personel

import (
	"context"
	"errors"
	"time"
)

// Domain says which products a Personalia account may use (m_users.domain).
type Domain string

const (
	// DomainPersonalia accounts use the Personalia app, its admin panel and SiAPP.
	DomainPersonalia Domain = "personalia"
	// DomainSiAPP accounts use SiAPP only.
	DomainSiAPP Domain = "siapp"
)

// Valid reports whether d is a domain Personalia is known to send.
func (d Domain) Valid() bool { return d == DomainPersonalia || d == DomainSiAPP }

// Identity is one person as Personalia knows them. SiAPP keeps no credentials: this is
// all it learns from a login.
type Identity struct {
	PersonelUserID int64
	Email          string
	// NIK is empty for accounts without an employee number (D4).
	NIK      string
	FullName string
	Phone    string
	PhotoURL string
	// OrganizationID is the SiAPP tenant: the group's one organization when the Client has a
	// Group (D17), the employing company itself otherwise. It is 0 when the person has no
	// work row (which a login never returns: that is ErrAccountDisabled) and when they work
	// for a company outside the group (CompanyID is then set).
	OrganizationID   int64
	OrganizationName string
	// CompanyID is the employing company (PT) in Personalia, for display. It is not the
	// tenant: KPI, IDS and PCD share one organization.
	CompanyID   int64
	CompanyName string
	// ClientID is the placement client, which is not a tenant.
	ClientID     int64
	ClientName   string
	PositionID   int64
	PositionName string
	// Active is false for a deactivated or resigned person. Directory lookups still return
	// them, so SiAPP can deactivate its own rows.
	Active bool
	Domain Domain
}

// Typed errors. Callers match with errors.Is.
var (
	// ErrInvalidCredentials: wrong email or password. Personalia answers both the same. An
	// email Personalia refuses as malformed is a wrong email too.
	ErrInvalidCredentials = errors.New("personel: invalid credentials")
	// ErrAccountDisabled: the password is right but the person may not sign in. See
	// DisabledError for the reason.
	ErrAccountDisabled = errors.New("personel: account disabled")
	// ErrRateLimited: Personalia refused the login because of too many failures for this
	// email, whatever the password.
	ErrRateLimited = errors.New("personel: rate limited")
	// ErrNotFound: no such person (or, for a scoped lookup, not in that organization).
	ErrNotFound = errors.New("personel: not found")
	// ErrUnavailable: Personalia cannot answer now: network error, timeout, 5xx, an open
	// circuit breaker, or a rejected integration key (a configuration error, logged
	// loudly). It is never "invalid credentials". The API reports it as PERSONEL_UNAVAILABLE.
	ErrUnavailable = errors.New("personel: unavailable")
)

// Why a person who proved their password may still not sign in.
const (
	DisabledInactive    = "employee_inactive"     // deactivated, or resignation in effect
	DisabledWorkMissing = "employee_work_missing" // no work row (no NIK, or never assigned)
)

// DisabledError is the error behind ErrAccountDisabled, carrying Personalia's reason.
type DisabledError struct{ Reason string }

func (e *DisabledError) Error() string { return "personel: account disabled: " + e.Reason }

// Is makes errors.Is(err, ErrAccountDisabled) true.
func (e *DisabledError) Is(target error) bool { return target == ErrAccountDisabled }

// LoginInput is S1's request. ClientIP and UserAgent describe the person's phone: they only
// go to Personalia's security log, so HR sees the real device and not SiAPP's server.
type LoginInput struct {
	Email     string
	Password  string
	ClientIP  string
	UserAgent string
}

// Auth is seam S1: the Personalia account login. It never caches and never retries (a
// retry would count as a second failed attempt against the account). It issues no token
// and binds no device: both are SiAPP's (module 01).
type Auth interface {
	Login(ctx context.Context, in LoginInput) (Identity, error)
}

// Directory is seam S4: employee lookup by NIK. Lookups are scoped to an organization,
// the employing company: a person employed elsewhere is ErrNotFound, so one tenant can
// never read another tenant's people.
type Directory interface {
	EmployeeByNIK(ctx context.Context, orgID int64, nik string) (Identity, error)
	// EmployeesByNIKs omits unknown NIKs and other organizations' people; the order of the
	// result is not the order of niks.
	EmployeesByNIKs(ctx context.Context, orgID int64, niks []string) ([]Identity, error)
}

// Project is a place of work (project/area/location) in the Personel org hierarchy.
type Project struct {
	ID       int64
	Name     string
	ClientID int64
}

// Placement puts a person on a project.
type Placement struct {
	ProjectID  int64
	NIK        string
	PositionID int64
}

// Position is a job title.
type Position struct {
	ID   int64
	Name string
}

// Org is seam S2 (proposed): the org hierarchy, read-only.
type Org interface {
	Projects(ctx context.Context, orgID int64) ([]Project, error)
	Project(ctx context.Context, orgID, projectID int64) (Project, error)
	Placements(ctx context.Context, orgID, projectID int64) ([]Placement, error)
	Positions(ctx context.Context, orgID int64) ([]Position, error)
}

// AttendanceEvent is one check-in or check-out SiAPP captured. Key makes the push
// idempotent: the same Key is the same event.
type AttendanceEvent struct {
	Key        string
	NIK        string
	Kind       string // "check_in" | "check_out"
	OccurredAt time.Time
	ProjectID  int64
}

// AttendanceStatus is where Personel's record stands for one person and day.
type AttendanceStatus struct {
	NIK        string
	Date       time.Time
	CheckedIn  bool
	CheckedOut bool
}

// Attendance is seam S3 (proposed): SiAPP captures evidence and pushes the events;
// Personel is the system of record.
type Attendance interface {
	PushEvents(ctx context.Context, orgID int64, events []AttendanceEvent) error
	Status(ctx context.Context, orgID int64, nik string, date time.Time) (AttendanceStatus, error)
}

// LeaveType is a leave category (izin) defined in Personel.
type LeaveType struct {
	ID   int64
	Name string
}

// LeaveRequest is a leave application submitted through SiAPP.
type LeaveRequest struct {
	NIK         string
	LeaveTypeID int64
	From, To    time.Time
	Reason      string
}

// Leave states.
const (
	LeavePending  = "pending"
	LeaveApproved = "approved"
	LeaveRejected = "rejected"
)

// LeaveRecord is a leave request as Personel holds it.
type LeaveRecord struct {
	ID string
	LeaveRequest
	State string
}

// LeaveFilter narrows Leave.List; zero fields match everything.
type LeaveFilter struct {
	NIK   string
	State string
}

// Leave is seam S5 (proposed): the DANRU decides in SiAPP, SiAPP forwards the decision.
type Leave interface {
	Types(ctx context.Context, orgID int64) ([]LeaveType, error)
	Submit(ctx context.Context, orgID int64, req LeaveRequest) (LeaveRecord, error)
	Decide(ctx context.Context, orgID int64, id string, approve bool, note string) error
	List(ctx context.Context, orgID int64, f LeaveFilter) ([]LeaveRecord, error)
}
