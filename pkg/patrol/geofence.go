package patrol

import (
	"math"

	"siapp/pkg/platform/geo"
)

// maxAccuracyTolerance is the most the phone's own GPS accuracy may widen a zone's radius, in meters.
const maxAccuracyTolerance = 30.0

// insideZone reports whether a fix at distance meters from the zone's centre counts as inside a
// zone of the given radius: distance <= radius + min(accuracy, 30).
//
// ⚠ Why the tolerance is capped. A fix with an accuracy of 20 m may really be 20 m from where it
// says, so the guard standing at the edge should not be refused for it. But a fix that admits to
// an accuracy of 500 m is a fix that says almost nothing, and if the whole accuracy were allowed it
// would unlock any zone within 500 m of wherever the phone guessed: a guard could scan a post from
// the road. Capping the tolerance at 30 m keeps the benefit of the doubt small whatever the phone
// claims.
//
// This is what enforces the manual's "Anda berada diluar zona patroli" on the server, not just in
// the app. The gap design 05-gps-indoor-floor.md proposes warning instead of blocking when the fix
// is poor (DL-70); it is not adopted, so a fix outside the radius is blocked until it is.
func insideZone(distance, radius, accuracy float64) bool {
	return distance <= radius+math.Min(math.Max(accuracy, 0), maxAccuracyTolerance)
}

// distanceTo is the distance in meters between a fix and a zone's centre.
func distanceTo(zoneLat, zoneLng, lat, lng float64) float64 {
	return geo.DistanceMeters(geo.Point{Lat: zoneLat, Lng: zoneLng}, geo.Point{Lat: lat, Lng: lng})
}
