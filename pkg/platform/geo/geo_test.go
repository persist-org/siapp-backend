package geo_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/geo"
)

func TestDistanceMeters(t *testing.T) {
	for _, tc := range []struct {
		name      string
		a, b      geo.Point
		want, tol float64
	}{
		{"the same point", geo.Point{Lat: -6.2, Lng: 106.8}, geo.Point{Lat: -6.2, Lng: 106.8}, 0, 0.001},
		// Monas to Bundaran HI: about 2.2 km.
		{"Monas to Bundaran HI", geo.Point{Lat: -6.175392, Lng: 106.827153}, geo.Point{Lat: -6.195137, Lng: 106.823036}, 2200, 100},
		// Jakarta to Surabaya: about 660 km.
		{"Jakarta to Surabaya", geo.Point{Lat: -6.2088, Lng: 106.8456}, geo.Point{Lat: -7.2575, Lng: 112.7521}, 662000, 5000},
		// One degree of latitude is 111.2 km on the mean sphere.
		{"one degree of latitude", geo.Point{Lat: 0, Lng: 0}, geo.Point{Lat: 1, Lng: 0}, 111195, 10},
		{"across the antimeridian", geo.Point{Lat: 0, Lng: 179.5}, geo.Point{Lat: 0, Lng: -179.5}, 111195, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.InDelta(t, tc.want, geo.DistanceMeters(tc.a, tc.b), tc.tol)
			require.InDelta(t, geo.DistanceMeters(tc.a, tc.b), geo.DistanceMeters(tc.b, tc.a), 0.001, "symmetric")
		})
	}
}
