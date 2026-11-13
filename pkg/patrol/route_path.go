package patrol

import (
	"math"
	"slices"
	"time"

	"siapp/pkg/platform/geo"
)

// The numbers that shape a stored path (plan 10.4).
const (
	// maxPointAccuracy is the worst accuracy, in meters, a point may admit to and still be drawn.
	maxPointAccuracy = 50.0
	// maxSpeed is the fastest a guard can move between two points, in meters per second. 15 m/s is
	// 54 km/h: a guard on a motorbike passes, a jump across town does not.
	maxSpeed = 15.0
	// simplifyTolerance is how far, in meters, the simplified line may stray from the points it drops.
	simplifyTolerance = 3.0
	// maxPathPoints caps the stored path.
	maxPathPoints = 20000
	// reanchorAfter is how many points in a row may be refused as jumps before the next one is
	// believed. See filterPoints.
	reanchorAfter = 5
)

// RoutePoint is one position of a patrol: where, when, and how sure the phone was.
type RoutePoint struct {
	Seq      int32
	At       time.Time
	Lat, Lng float64
	Accuracy float64
}

func (p RoutePoint) geo() geo.Point { return geo.Point{Lat: p.Lat, Lng: p.Lng} }

// BuildPath turns the raw points of a patrol, in the order of their seq, into the line that is
// stored and drawn, and its length in meters. scans are the positions of the scans made during the
// patrol; the line keeps the point nearest to each.
//
// The steps, each of which has a reason:
//
//  1. Drop the noise (filterPoints).
//  2. Simplify what is left (simplify), keeping the first point, the last and the one nearest each scan.
//  3. Keep the line under maxPathPoints.
//  4. Measure the length over the points that are kept.
func BuildPath(points []RoutePoint, scans []geo.Point) (path []RoutePoint, distanceMeters float64) {
	kept := filterPoints(points)
	if len(kept) == 0 {
		return []RoutePoint{}, 0
	}

	protected := protectedIndexes(kept, scans)
	keep := simplify(kept, protected, simplifyTolerance)

	// A very long patrol can still have more points than the cap after simplifying at 3 m. A
	// coarser tolerance keeps the shape; cutting the tail, or every other point, would not.
	tolerance := simplifyTolerance
	for countTrue(keep) > maxPathPoints && tolerance < 1e6 {
		tolerance *= 2
		keep = simplify(kept, protected, tolerance)
	}

	path = make([]RoutePoint, 0, countTrue(keep))
	for i, p := range kept {
		if keep[i] {
			path = append(path, p)
		}
	}
	// ⚠ The length is measured over the simplified line, not over every raw point. A guard standing
	// still at a post draws a cloud of points a few meters apart, and adding every hop of that
	// cloud would count a walk that never happened. The simplified line cuts a corner by at most
	// the tolerance, which is the smaller error.
	for i := 1; i < len(path); i++ {
		distanceMeters += geo.DistanceMeters(path[i-1].geo(), path[i].geo())
	}
	return path, distanceMeters
}

// filterPoints drops the points that cannot be believed.
//
// ⚠ GPS noise draws spikes: a fix that lands a block away for one second makes the line look as if
// the guard flew there and back. Two rules catch them. A point whose accuracy is worse than
// maxPointAccuracy says itself that it says little. A point that would need more than maxSpeed to
// get to from the last point kept is not a place the guard was; a point whose time is not after the
// last kept one is a duplicate or a clock going backwards, and cannot be placed in the walk.
//
// A rule that compares with the last point KEPT has a trap: if that point was itself the spike
// (the very first fix is the usual one), every honest point after it is a "jump" and the whole walk
// disappears. So after reanchorAfter refusals in a row the next point is believed instead, and when
// the anchor is the only point kept so far it is replaced rather than joined.
func filterPoints(points []RoutePoint) []RoutePoint {
	kept := make([]RoutePoint, 0, len(points))
	refused := 0
	for _, p := range points {
		if p.Accuracy > maxPointAccuracy {
			continue
		}
		if len(kept) == 0 {
			kept = append(kept, p)
			continue
		}
		last := kept[len(kept)-1]
		dt := p.At.Sub(last.At).Seconds()
		jump := dt <= 0 || geo.DistanceMeters(last.geo(), p.geo())/dt > maxSpeed
		if jump && refused < reanchorAfter {
			refused++
			continue
		}
		if jump && len(kept) == 1 {
			kept[0] = p
		} else {
			kept = append(kept, p)
		}
		refused = 0
	}
	return kept
}

// protectedIndexes are the points the simplified line must keep: the first, the last, and for each
// scan the point nearest to it. They keep the shape of the walk and where the checks happened; a
// scan's pin on the map sits on the line instead of beside it.
func protectedIndexes(points []RoutePoint, scans []geo.Point) []int {
	set := map[int]bool{0: true, len(points) - 1: true}
	for _, s := range scans {
		nearest, best := 0, math.MaxFloat64
		for i, p := range points {
			if d := geo.DistanceMeters(s, p.geo()); d < best {
				nearest, best = i, d
			}
		}
		set[nearest] = true
	}
	out := make([]int, 0, len(set))
	for i := range set {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// simplify is Douglas-Peucker: it drops the points that lie within tolerance meters of the line
// between their neighbours that are kept. keep[i] says whether point i stays. The protected points
// always stay, and the line is simplified between each pair of them on its own, so no stretch of
// it can drop a protected point.
//
// It walks with its own stack: a patrol of tens of thousands of points in a straight line would
// recurse as deep as it is long.
func simplify(points []RoutePoint, protected []int, tolerance float64) []bool {
	keep := make([]bool, len(points))
	if len(points) == 0 {
		return keep
	}
	for _, i := range protected {
		keep[i] = true
	}
	// Meters on a flat map around the middle of the walk: for a patrol (kilometers) the error of
	// that is far under a meter, and the distance to a segment becomes plain geometry.
	xy := project(points)

	type span struct{ from, to int }
	var stack []span
	for k := 1; k < len(protected); k++ {
		stack = append(stack, span{protected[k-1], protected[k]})
	}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s.to-s.from < 2 {
			continue
		}
		far, farDist := -1, tolerance
		for i := s.from + 1; i < s.to; i++ {
			if d := distanceToSegment(xy[i], xy[s.from], xy[s.to]); d > farDist {
				far, farDist = i, d
			}
		}
		if far < 0 {
			continue
		}
		keep[far] = true
		stack = append(stack, span{s.from, far}, span{far, s.to})
	}
	return keep
}

type xyPoint struct{ x, y float64 }

// project puts the points on a flat map, in meters, east and north of the first point.
func project(points []RoutePoint) []xyPoint {
	const metersPerDegree = 111_319.49 // an arc of one degree along the equator
	origin := points[0]
	cosLat := math.Cos(origin.Lat * math.Pi / 180)
	out := make([]xyPoint, len(points))
	for i, p := range points {
		out[i] = xyPoint{x: (p.Lng - origin.Lng) * metersPerDegree * cosLat, y: (p.Lat - origin.Lat) * metersPerDegree}
	}
	return out
}

// distanceToSegment is how far p is from the segment a-b.
func distanceToSegment(p, a, b xyPoint) float64 {
	dx, dy := b.x-a.x, b.y-a.y
	if dx == 0 && dy == 0 {
		return math.Hypot(p.x-a.x, p.y-a.y)
	}
	t := ((p.x-a.x)*dx + (p.y-a.y)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(p.x-(a.x+t*dx), p.y-(a.y+t*dy))
}

func countTrue(v []bool) int {
	n := 0
	for _, b := range v {
		if b {
			n++
		}
	}
	return n
}
