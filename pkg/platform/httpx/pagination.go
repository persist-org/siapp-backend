package httpx

// Page limits.
const (
	DefaultPage  = 1
	DefaultLimit = 20
	MaxLimit     = 100
)

// Page is the pagination block of a ListEnvelope. Total is the number of rows matching the
// query, ignoring page and limit.
type Page struct {
	Page  int
	Limit int
	Total int
}

// TotalPages is the number of pages of Limit rows needed for Total rows (0 when empty).
func (p Page) TotalPages() int {
	if p.Limit <= 0 {
		return 0
	}
	return (p.Total + p.Limit - 1) / p.Limit
}

// Offset is the SQL OFFSET of the page.
func (p Page) Offset() int { return (p.Page - 1) * p.Limit }
