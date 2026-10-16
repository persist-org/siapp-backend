// Package personeltest is the in-process fake of Persist Personel (Personalia) for SiAPP's
// tests. It is the only fake: nothing else in the repo pretends to be Personalia.
//
// Handler serves the same paths, envelope, status codes and error codes as the integration
// in kpi-personalia-api (Phase 00.5 Part A), behind the same key check, and its fixtures
// mirror that repo's db/seed/40_siapp.sql, so a test written against the fake describes
// what the real API does. The live contract test (go test -tags personel_live) checks that
// claim against a real kpi-personalia-api; run it whenever either side changes.
//
// The proposed seams (Org S2, Attendance S3, Leave S5) have no HTTP endpoints yet, so Fake
// implements those interfaces directly, in memory.
package personeltest

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"siapp/pkg/platform/personel"
)

// BasePath is where Handler mounts the integration, as in kpi-personalia-api.
const BasePath = "/api/v3/integrations/siapp"

// DevKey is the integration key Fake accepts unless Fake.Key is changed.
const DevKey = "fake-integration-key"

// loginFailureLimit mirrors the integration's per-email limit (default 10). Unlike the real
// one it has no 15-minute window: a test that needs a fresh budget uses another email.
const loginFailureLimit = 10

// maxNIKs is the most distinct NIKs one /employees request may carry.
const maxNIKs = 200

// Fixture identities, mirroring kpi-personalia-api/db/seed/40_siapp.sql. Every account's
// password is Password.
const (
	Password = "Test1234!"

	OrgSatu    int64 = 900101 // "PT Siaga Satu (seed)"
	OrgDua     int64 = 900102 // "PT Siaga Dua (seed)"
	ClientMall int64 = 900103 // "PT Mall Klien (seed)": a placement client, not a tenant

	EmailDanruSatu      = "danru.satu@siapp.test"      // SIAPPA0001, siapp, company Satu
	EmailAnggota1Satu   = "anggota1.satu@siapp.test"   // SIAPPA0002, siapp, placed at the mall
	EmailAnggota2Satu   = "anggota2.satu@siapp.test"   // SIAPPA0003, siapp
	EmailNonaktifSatu   = "nonaktif.satu@siapp.test"   // SIAPPA0004, siapp, deactivated
	EmailSupervisorSatu = "supervisor.satu@siapp.test" // SIAPPA0005, personalia
	EmailDanruDua       = "danru.dua@siapp.test"       // SIAPPB0001, siapp, company Dua
	EmailAnggota1Dua    = "anggota1.dua@siapp.test"    // SIAPPB0002
	EmailAnggota2Dua    = "anggota2.dua@siapp.test"    // SIAPPB0003
	EmailNoNIK          = "tanpanik@siapp.test"        // no NIK: login is employee_work_missing

	NIKDanruSatu      = "SIAPPA0001"
	NIKAnggota1Satu   = "SIAPPA0002"
	NIKAnggota2Satu   = "SIAPPA0003"
	NIKNonaktifSatu   = "SIAPPA0004"
	NIKSupervisorSatu = "SIAPPA0005"
	NIKDanruDua       = "SIAPPB0001"
	NIKAnggota1Dua    = "SIAPPB0002"
	NIKAnggota2Dua    = "SIAPPB0003"
)

// Call is one request that reached the fake, in arrival order. Requests that fail the key
// check or an injected failure are recorded too: Calls() answers "how many times did SiAPP
// try", which is what the no-retry-on-login rule is about.
type Call struct {
	Method string // HTTP method, or "CALL" for the in-memory seams
	Path   string
}

type account struct {
	password string
	id       personel.Identity
	// noWork marks an account with no work row: login is employee_work_missing.
	noWork bool
}

// Fake is a fake Personalia. The zero value is not usable: use New. Safe for concurrent use.
type Fake struct {
	// Key is the integration key Handler expects in X-Integration-Key.
	Key string

	mu       sync.Mutex
	accounts []*account
	orgNames map[int64]string
	calls    []Call
	failNext []int
	down     bool
	failures map[string]int

	pushed []personel.AttendanceEvent
	leaves []personel.LeaveRecord
}

var (
	_ personel.Org        = (*Fake)(nil)
	_ personel.Attendance = (*Fake)(nil)
	_ personel.Leave      = (*Fake)(nil)
)

// New returns a Fake loaded with the fixtures.
func New() *Fake {
	f := &Fake{
		Key:      DevKey,
		orgNames: map[int64]string{OrgSatu: "PT Siaga Satu (seed)", OrgDua: "PT Siaga Dua (seed)"},
		failures: map[string]int{},
	}
	pos := func(org int64, danru bool) (int64, string) {
		switch {
		case org == OrgSatu && danru:
			return 900101, "Danru"
		case org == OrgSatu:
			return 900102, "Anggota Satpam"
		case danru:
			return 900103, "Danru"
		}
		return 900104, "Anggota Satpam"
	}
	add := func(uid int64, email, nik, name string, org int64, danru bool, client int64, domain personel.Domain, active bool, phone string) {
		pid, pname := pos(org, danru)
		f.accounts = append(f.accounts, &account{password: Password, id: personel.Identity{
			PersonelUserID: uid, Email: email, NIK: nik, FullName: name, Phone: phone,
			OrganizationID: org, OrganizationName: f.orgNames[org],
			ClientID: client, ClientName: f.clientName(client, org),
			PositionID: pid, PositionName: pname, Active: active, Domain: domain,
		}})
	}
	add(900101, EmailDanruSatu, NIKDanruSatu, "Danru Satu", OrgSatu, true, OrgSatu, personel.DomainSiAPP, true, "0812 0000 0001")
	add(900102, EmailAnggota1Satu, NIKAnggota1Satu, "Anggota Satu A", OrgSatu, false, ClientMall, personel.DomainSiAPP, true, "")
	add(900103, EmailAnggota2Satu, NIKAnggota2Satu, "Anggota Satu B", OrgSatu, false, OrgSatu, personel.DomainSiAPP, true, "")
	add(900104, EmailNonaktifSatu, NIKNonaktifSatu, "Mantan Anggota", OrgSatu, false, OrgSatu, personel.DomainSiAPP, false, "")
	add(900105, EmailSupervisorSatu, NIKSupervisorSatu, "Supervisor Satu", OrgSatu, true, OrgSatu, personel.DomainPersonalia, true, "")
	add(900106, EmailDanruDua, NIKDanruDua, "Danru Dua", OrgDua, true, OrgDua, personel.DomainSiAPP, true, "0812 0000 0002")
	add(900107, EmailAnggota1Dua, NIKAnggota1Dua, "Anggota Dua A", OrgDua, false, OrgDua, personel.DomainSiAPP, true, "")
	add(900108, EmailAnggota2Dua, NIKAnggota2Dua, "Anggota Dua B", OrgDua, false, OrgDua, personel.DomainSiAPP, true, "")
	f.accounts = append(f.accounts, &account{password: Password, noWork: true, id: personel.Identity{
		PersonelUserID: 900109, Email: EmailNoNIK, FullName: "Tanpa NIK", Domain: personel.DomainSiAPP,
	}})
	return f
}

func (f *Fake) clientName(client, org int64) string {
	if client == ClientMall {
		return "PT Mall Klien (seed)"
	}
	return f.orgNames[org]
}

// --- hooks -----------------------------------------------------------------------------

func (f *Fake) mutate(email, nik string, fn func(*account)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.accounts {
		if (email != "" && a.id.Email == email) || (nik != "" && a.id.NIK == nik) {
			fn(a)
			return
		}
	}
	panic(fmt.Sprintf("personeltest: no fixture with email %q / NIK %q", email, nik))
}

// Deactivate marks the person inactive, as HR would: their next login is employee_inactive
// and lookups return active:false.
func (f *Fake) Deactivate(nik string) {
	f.mutate("", nik, func(a *account) { a.id.Active = false })
}

// SetDomain changes the account's domain, as HR does in the admin panel. The integration
// login accepts both domains.
func (f *Fake) SetDomain(email string, d personel.Domain) {
	f.mutate(email, "", func(a *account) { a.id.Domain = d })
}

// Rename changes the person's name, as HR does in Personalia: SiAPP's cached copy catches up
// at the person's next login.
func (f *Fake) Rename(nik, fullName string) {
	f.mutate("", nik, func(a *account) { a.id.FullName = fullName })
}

// MoveToCompany moves the person to another employing company, which is another tenant.
func (f *Fake) MoveToCompany(nik string, orgID int64) {
	f.mutate("", nik, func(a *account) {
		name, ok := f.orgNamesLocked(orgID)
		if !ok {
			name = fmt.Sprintf("PT Company %d", orgID)
		}
		a.id.OrganizationID, a.id.OrganizationName = orgID, name
	})
}

func (f *Fake) orgNamesLocked(orgID int64) (string, bool) {
	n, ok := f.orgNames[orgID]
	return n, ok
}

// FailNext makes the next request answer with this HTTP status and no JSON body, like a
// gateway error. Call it again to queue more.
func (f *Fake) FailNext(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext = append(f.failNext, status)
}

// Down makes every request answer 503 until Up.
func (f *Fake) Down() { f.setDown(true) }

// Up undoes Down.
func (f *Fake) Up() { f.setDown(false) }

func (f *Fake) setDown(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = v
}

// Calls returns every request so far, oldest first.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallCount counts the requests whose Path ends with suffix, e.g. CallCount("/login").
func (f *Fake) CallCount(suffix string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasSuffix(c.Path, suffix) {
			n++
		}
	}
	return n
}

// --- HTTP: S1 login and S4 employees ---------------------------------------------------

// Handler serves BasePath+"/login" and BasePath+"/employees".
func (f *Fake) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+BasePath+"/login", f.guarded(f.login))
	mux.HandleFunc("POST "+BasePath+"/employees", f.guarded(f.employees))
	return mux
}

type envelope struct {
	Data         any    `json:"data"`
	Error        any    `json:"error"`
	Message      string `json:"message"`
	InternalCode int    `json:"internal_code"`
	ErrorType    any    `json:"error_type"`
}

func write(w http.ResponseWriter, status int, env envelope) {
	env.InternalCode = status
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

func ok(w http.ResponseWriter, data any, msg string) {
	write(w, http.StatusOK, envelope{Data: data, Message: msg})
}

func fail(w http.ResponseWriter, status int, code any, msg, typ string) {
	write(w, status, envelope{Error: code, Message: msg, ErrorType: typ})
}

// guarded records the call, then applies outage injection and the key check, in the order
// a real deployment would: the gateway is in front of the key check.
func (f *Fake) guarded(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, Call{Method: r.Method, Path: r.URL.Path})
		injected := 0
		switch {
		case f.down:
			injected = http.StatusServiceUnavailable
		case len(f.failNext) > 0:
			injected, f.failNext = f.failNext[0], f.failNext[1:]
		}
		key := f.Key
		f.mu.Unlock()

		if injected != 0 {
			http.Error(w, "upstream failure", injected)
			return
		}
		if got := r.Header.Get("X-Integration-Key"); got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			fail(w, http.StatusUnauthorized, "invalid_integration_key", "Silakan login kembali.", "unauthorized")
			return
		}
		next(w, r)
	}
}

type identityDTO struct {
	UserID   int64           `json:"user_id"`
	Email    string          `json:"email"`
	NIK      *string         `json:"nik"`
	FullName string          `json:"full_name"`
	Phone    *string         `json:"phone"`
	PhotoURL *string         `json:"photo_url"`
	Company  *namedRef       `json:"company"`
	Client   *namedRef       `json:"client"`
	Position *namedRef       `json:"position"`
	Active   bool            `json:"active"`
	Domain   personel.Domain `json:"domain"`
}

type namedRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ref(id int64, name string) *namedRef {
	if id == 0 {
		return nil
	}
	return &namedRef{ID: id, Name: name}
}

func toDTO(id personel.Identity) identityDTO {
	return identityDTO{
		UserID: id.PersonelUserID, Email: id.Email, NIK: nullable(id.NIK), FullName: id.FullName,
		Phone: nullable(id.Phone), PhotoURL: nil, // always null in the real API, for now
		Company:  ref(id.OrganizationID, id.OrganizationName),
		Client:   ref(id.ClientID, id.ClientName),
		Position: ref(id.PositionID, id.PositionName),
		Active:   id.Active, Domain: id.Domain,
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "invalid_request", "Permintaan tidak valid.", "bad_request")
		return false
	}
	return true
}

func (f *Fake) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email     string `json:"email"`
		Password  string `json:"password"`
		UserAgent string `json:"user_agent"`
		ClientIP  string `json:"client_ip"`
	}
	if !decode(w, r, &req) {
		return
	}
	// The real LoginRequest rules: email required,email; password required;
	// user_agent max=512; client_ip max=64 (characters). net/mail stands in for the
	// validator's email rule: they agree on the addresses tests use.
	invalid := map[string][]string{}
	if addr, err := mail.ParseAddress(req.Email); err != nil || addr.Address != req.Email {
		invalid["email"] = []string{"format tidak valid"}
	}
	if req.Password == "" {
		invalid["password"] = []string{"wajib diisi"}
	}
	if utf8.RuneCountInString(req.UserAgent) > 512 {
		invalid["user_agent"] = []string{"maksimal 512 karakter"}
	}
	if utf8.RuneCountInString(req.ClientIP) > 64 {
		invalid["client_ip"] = []string{"maksimal 64 karakter"}
	}
	if len(invalid) > 0 {
		write(w, http.StatusUnprocessableEntity, envelope{Error: invalid, Message: "Data tidak valid.", ErrorType: "validation"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	email := strings.ToLower(req.Email)
	// Past the limit even the right password is refused, so a guesser learns nothing.
	if f.failures[email] >= loginFailureLimit {
		w.Header().Set("Retry-After", "900")
		fail(w, http.StatusTooManyRequests, "rate_limited", "Terlalu banyak percobaan.", "too_many_requests")
		return
	}
	var acc *account
	for _, a := range f.accounts {
		if a.id.Email == email {
			acc = a
		}
	}
	// One answer for an unknown email and a wrong password.
	if acc == nil || acc.password != req.Password {
		f.failures[email]++
		fail(w, http.StatusUnauthorized, "invalid_credentials", "Silakan login kembali.", "unauthorized")
		return
	}
	switch {
	case acc.noWork:
		fail(w, http.StatusForbidden, personel.DisabledWorkMissing, "Hubungi HR.", "forbidden")
	case !acc.id.Active:
		fail(w, http.StatusForbidden, personel.DisabledInactive, "Akun karyawan tidak aktif.", "forbidden")
	default:
		ok(w, toDTO(acc.id), "Login berhasil.")
	}
}

func (f *Fake) employees(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NIKs []string `json:"niks"`
	}
	if !decode(w, r, &req) {
		return
	}
	// The real rule: niks required,min=1,dive,required,max=100.
	if len(req.NIKs) == 0 || slices.ContainsFunc(req.NIKs, func(n string) bool { return n == "" || utf8.RuneCountInString(n) > 100 }) {
		write(w, http.StatusUnprocessableEntity, envelope{
			Error: map[string][]string{"niks": {"wajib diisi"}}, Message: "Data tidak valid.", ErrorType: "validation",
		})
		return
	}
	// Like the real API, the 200 limit counts distinct NIKs (trimmed), not the
	// raw list.
	var unique []string
	for _, nik := range req.NIKs {
		nik = strings.TrimSpace(nik)
		if nik != "" && !slices.Contains(unique, nik) {
			unique = append(unique, nik)
		}
	}
	if len(unique) > maxNIKs {
		fail(w, http.StatusUnprocessableEntity, "too_many_niks", "Data tidak valid.", "validation")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	data := make([]identityDTO, 0, len(unique))
	for _, nik := range unique {
		for _, a := range f.accounts {
			if a.id.NIK == nik && !a.noWork {
				data = append(data, toDTO(a.id))
			}
		}
	}
	ok(w, data, "Data berhasil diambil.")
}

// --- in-memory seams: S2 org, S3 attendance, S5 leave ----------------------------------

func (f *Fake) record(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, Call{Method: "CALL", Path: path})
	if f.down {
		return fmt.Errorf("%w: fake is down", personel.ErrUnavailable)
	}
	return nil
}

// Projects implements personel.Org: one project per client the org's people are placed at.
func (f *Fake) Projects(_ context.Context, orgID int64) ([]personel.Project, error) {
	if err := f.record("org/projects"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projectsLocked(orgID), nil
}

func (f *Fake) projectsLocked(orgID int64) []personel.Project {
	var out []personel.Project
	seen := map[int64]bool{}
	for _, a := range f.accounts {
		id := a.id
		if id.OrganizationID == orgID && id.ClientID != 0 && !seen[id.ClientID] {
			seen[id.ClientID] = true
			out = append(out, personel.Project{ID: id.ClientID, Name: id.ClientName, ClientID: id.ClientID})
		}
	}
	slices.SortFunc(out, func(a, b personel.Project) int { return int(a.ID - b.ID) })
	return out
}

// Project implements personel.Org.
func (f *Fake) Project(_ context.Context, orgID, projectID int64) (personel.Project, error) {
	if err := f.record("org/project"); err != nil {
		return personel.Project{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.projectsLocked(orgID) {
		if p.ID == projectID {
			return p, nil
		}
	}
	return personel.Project{}, personel.ErrNotFound
}

// Placements implements personel.Org.
func (f *Fake) Placements(_ context.Context, orgID, projectID int64) ([]personel.Placement, error) {
	if err := f.record("org/placements"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []personel.Placement
	for _, a := range f.accounts {
		id := a.id
		if id.OrganizationID == orgID && id.ClientID == projectID && id.NIK != "" {
			out = append(out, personel.Placement{ProjectID: projectID, NIK: id.NIK, PositionID: id.PositionID})
		}
	}
	return out, nil
}

// Positions implements personel.Org.
func (f *Fake) Positions(_ context.Context, orgID int64) ([]personel.Position, error) {
	if err := f.record("org/positions"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[int64]bool{}
	var out []personel.Position
	for _, a := range f.accounts {
		id := a.id
		if id.OrganizationID == orgID && id.PositionID != 0 && !seen[id.PositionID] {
			seen[id.PositionID] = true
			out = append(out, personel.Position{ID: id.PositionID, Name: id.PositionName})
		}
	}
	slices.SortFunc(out, func(a, b personel.Position) int { return int(a.ID - b.ID) })
	return out, nil
}

// PushEvents implements personel.Attendance. Events are idempotent on Key.
func (f *Fake) PushEvents(_ context.Context, _ int64, events []personel.AttendanceEvent) error {
	if err := f.record("attendance/push"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range events {
		if !slices.ContainsFunc(f.pushed, func(p personel.AttendanceEvent) bool { return p.Key == e.Key }) {
			f.pushed = append(f.pushed, e)
		}
	}
	return nil
}

// Status implements personel.Attendance, from the events pushed so far.
func (f *Fake) Status(_ context.Context, _ int64, nik string, date time.Time) (personel.AttendanceStatus, error) {
	if err := f.record("attendance/status"); err != nil {
		return personel.AttendanceStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := personel.AttendanceStatus{NIK: nik, Date: date}
	for _, e := range f.pushed {
		if e.NIK != nik || !sameDay(e.OccurredAt, date) {
			continue
		}
		switch e.Kind {
		case "check_in":
			st.CheckedIn = true
		case "check_out":
			st.CheckedOut = true
		}
	}
	return st, nil
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// Pushed returns the attendance events received, in order, without duplicates.
func (f *Fake) Pushed() []personel.AttendanceEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pushed)
}

// Types implements personel.Leave.
func (f *Fake) Types(_ context.Context, _ int64) ([]personel.LeaveType, error) {
	if err := f.record("leave/types"); err != nil {
		return nil, err
	}
	return []personel.LeaveType{{ID: 1, Name: "Sakit"}, {ID: 2, Name: "Izin"}, {ID: 3, Name: "Cuti"}}, nil
}

// Submit implements personel.Leave.
func (f *Fake) Submit(_ context.Context, _ int64, req personel.LeaveRequest) (personel.LeaveRecord, error) {
	if err := f.record("leave/submit"); err != nil {
		return personel.LeaveRecord{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := personel.LeaveRecord{ID: fmt.Sprintf("leave-%d", len(f.leaves)+1), LeaveRequest: req, State: personel.LeavePending}
	f.leaves = append(f.leaves, rec)
	return rec, nil
}

// Decide implements personel.Leave.
func (f *Fake) Decide(_ context.Context, _ int64, id string, approve bool, _ string) error {
	if err := f.record("leave/decide"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.leaves {
		if f.leaves[i].ID == id {
			f.leaves[i].State = personel.LeaveRejected
			if approve {
				f.leaves[i].State = personel.LeaveApproved
			}
			return nil
		}
	}
	return personel.ErrNotFound
}

// List implements personel.Leave.
func (f *Fake) List(_ context.Context, _ int64, filter personel.LeaveFilter) ([]personel.LeaveRecord, error) {
	if err := f.record("leave/list"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []personel.LeaveRecord
	for _, l := range f.leaves {
		if (filter.NIK == "" || l.NIK == filter.NIK) && (filter.State == "" || l.State == filter.State) {
			out = append(out, l)
		}
	}
	return out, nil
}
