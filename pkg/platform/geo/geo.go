// Package geo holds the small geographic value types and calculations SiAPP shares between modules.
package geo

import "math"

// Point is a place on the earth in decimal degrees (WGS 84): the same unit the database
// stores and the apps send.
type Point struct {
	Lat float64
	Lng float64
}

// earthRadiusMeters is the mean radius of the earth (IUGG: 6 371 008.8 m). The haversine formula
// treats the earth as a sphere, which is off by at most about 0.5% against the real ellipsoid:
// a few meters over the hundreds of meters a geofence measures, far below GPS accuracy.
const earthRadiusMeters = 6371008.8

// DistanceMeters is the great-circle distance between a and b in meters, by the haversine formula:
//
//	h = sin²(Δφ/2) + cos φa · cos φb · sin²(Δλ/2)
//	d = 2 · R · asin(√h)
//
// where φ is the latitude and λ the longitude in radians. Haversine is used rather than the
// spherical law of cosines because it stays accurate for the short distances a geofence checks.
func DistanceMeters(a, b Point) float64 {
	phiA, phiB := radians(a.Lat), radians(b.Lat)
	dPhi, dLambda := radians(b.Lat-a.Lat), radians(b.Lng-a.Lng)
	h := math.Sin(dPhi/2)*math.Sin(dPhi/2) + math.Cos(phiA)*math.Cos(phiB)*math.Sin(dLambda/2)*math.Sin(dLambda/2)
	// Rounding can push h a hair above 1 for antipodal points; asin would then be NaN.
	return 2 * earthRadiusMeters * math.Asin(math.Sqrt(math.Min(1, h)))
}

func radians(degrees float64) float64 { return degrees * math.Pi / 180 }
