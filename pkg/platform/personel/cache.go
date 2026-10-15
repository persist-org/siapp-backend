package personel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// personelCacheTTL is how long Directory and Org answers are kept. The webhook receiver
// (Phase 05.3) calls Invalidate when Personel says something changed, so the TTL only
// bounds how stale a missed webhook can leave us.
const personelCacheTTL = 10 * time.Minute

// Cache kinds, the {kind} part of the key org:{orgID}:personel:{kind}:{id}.
const (
	KindEmployee   = "employee"   // id: NIK
	KindProjects   = "projects"   // id: "all"
	KindProject    = "project"    // id: project id
	KindPlacements = "placements" // id: project id
	KindPositions  = "positions"  // id: "all"
)

// Cached is a Redis read-through cache in front of Directory and Org. Auth is never cached
// (a login must always reach Personalia), so Cached does not implement it.
//
// Errors are not cached, and neither are "not found" answers. A Redis failure is logged and
// the call goes to Personalia: the cache is an optimisation, not a dependency.
type Cached struct {
	dir Directory
	org Org // nil when only Directory is cached
	rdb redis.Cmdable
	log *slog.Logger
	ttl time.Duration
}

var (
	_ Directory = (*Cached)(nil)
	_ Org       = (*Cached)(nil)
)

// NewCached wraps dir and org. org may be nil until S2 exists; Org methods then return
// ErrUnavailable.
func NewCached(dir Directory, org Org, rdb redis.Cmdable, log *slog.Logger) *Cached {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Cached{dir: dir, org: org, rdb: rdb, log: log, ttl: personelCacheTTL}
}

func key(orgID int64, kind, id string) string {
	return fmt.Sprintf("org:%d:personel:%s:%s", orgID, kind, id)
}

// Invalidate drops one cached entry. Called by the webhook receiver.
func (c *Cached) Invalidate(ctx context.Context, orgID int64, kind, id string) error {
	return c.rdb.Del(ctx, key(orgID, kind, id)).Err()
}

// EmployeeByNIK implements Directory.
func (c *Cached) EmployeeByNIK(ctx context.Context, orgID int64, nik string) (Identity, error) {
	return readThrough(ctx, c, key(orgID, KindEmployee, nik), func() (Identity, error) {
		return c.dir.EmployeeByNIK(ctx, orgID, nik)
	})
}

// EmployeesByNIKs implements Directory. Cached people are served from Redis; only the
// missing NIKs go to Personalia, in one request.
func (c *Cached) EmployeesByNIKs(ctx context.Context, orgID int64, niks []string) ([]Identity, error) {
	result := make([]Identity, 0, len(niks))
	var missing []string
	seen := map[string]bool{}
	for _, nik := range niks {
		if nik == "" || seen[nik] {
			continue
		}
		seen[nik] = true
		var id Identity
		if c.get(ctx, key(orgID, KindEmployee, nik), &id) {
			result = append(result, id)
		} else {
			missing = append(missing, nik)
		}
	}
	if len(missing) == 0 {
		return result, nil
	}
	fetched, err := c.dir.EmployeesByNIKs(ctx, orgID, missing)
	if err != nil {
		return nil, err
	}
	for _, id := range fetched {
		c.set(ctx, key(orgID, KindEmployee, id.NIK), id)
		result = append(result, id)
	}
	return result, nil
}

// Projects implements Org.
func (c *Cached) Projects(ctx context.Context, orgID int64) ([]Project, error) {
	return readThrough(ctx, c, key(orgID, KindProjects, "all"), func() ([]Project, error) {
		if c.org == nil {
			return nil, fmt.Errorf("%w: org seam (S2) is not available", ErrUnavailable)
		}
		return c.org.Projects(ctx, orgID)
	})
}

// Project implements Org.
func (c *Cached) Project(ctx context.Context, orgID, projectID int64) (Project, error) {
	return readThrough(ctx, c, key(orgID, KindProject, strconv.FormatInt(projectID, 10)), func() (Project, error) {
		if c.org == nil {
			return Project{}, fmt.Errorf("%w: org seam (S2) is not available", ErrUnavailable)
		}
		return c.org.Project(ctx, orgID, projectID)
	})
}

// Placements implements Org.
func (c *Cached) Placements(ctx context.Context, orgID, projectID int64) ([]Placement, error) {
	return readThrough(ctx, c, key(orgID, KindPlacements, strconv.FormatInt(projectID, 10)), func() ([]Placement, error) {
		if c.org == nil {
			return nil, fmt.Errorf("%w: org seam (S2) is not available", ErrUnavailable)
		}
		return c.org.Placements(ctx, orgID, projectID)
	})
}

// Positions implements Org.
func (c *Cached) Positions(ctx context.Context, orgID int64) ([]Position, error) {
	return readThrough(ctx, c, key(orgID, KindPositions, "all"), func() ([]Position, error) {
		if c.org == nil {
			return nil, fmt.Errorf("%w: org seam (S2) is not available", ErrUnavailable)
		}
		return c.org.Positions(ctx, orgID)
	})
}

func readThrough[T any](ctx context.Context, c *Cached, k string, load func() (T, error)) (T, error) {
	var v T
	if c.get(ctx, k, &v) {
		return v, nil
	}
	v, err := load()
	if err != nil {
		var zero T
		return zero, err
	}
	c.set(ctx, k, v)
	return v, nil
}

func (c *Cached) get(ctx context.Context, k string, into any) bool {
	raw, err := c.rdb.Get(ctx, k).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.log.Warn("personel cache read failed", "error", err)
		}
		return false
	}
	// An entry we cannot decode (a field was added) is a miss, and is overwritten.
	return json.Unmarshal(raw, into) == nil
}

func (c *Cached) set(ctx context.Context, k string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	if err := c.rdb.Set(ctx, k, raw, c.ttl).Err(); err != nil {
		c.log.Warn("personel cache write failed", "error", err)
	}
}
