package patrol

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"siapp/pkg/platform/geo"
)

const (
	degPerMeter = 1.0 / 111_319.49 // degrees of latitude per meter
	baseLat     = -6.2
	baseLng     = 106.8
)

var baseTime = time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)

// pt is a point east/north meters from the base, seconds after the start, with an accuracy.
func pt(seq int32, east, north float64, sec int, acc float64) RoutePoint {
	return RoutePoint{
		Seq: seq, At: baseTime.Add(time.Duration(sec) * time.Second), Accuracy: acc,
		Lat: baseLat + north*degPerMeter,
		Lng: baseLng + east*degPerMeter/math.Cos(baseLat*math.Pi/180),
	}
}

func seqs(path []RoutePoint) []int32 {
	out := make([]int32, 0, len(path))
	for _, p := range path {
		out = append(out, p.Seq)
	}
	return out
}

func TestBuildPath(t *testing.T) {
	// A straight walk north, a point every 10 m and 5 s (2 m/s), eleven points.
	straight := func() []RoutePoint {
		var out []RoutePoint
		for i := 0; i <= 10; i++ {
			out = append(out, pt(int32(i), 0, float64(i)*10, i*5, 5))
		}
		return out
	}

	tests := []struct {
		name  string
		input []RoutePoint
		scans []geo.Point
		// want is the seq of the points the line keeps, in order.
		want []int32
	}{
		{name: "no points make no line", input: nil, want: []int32{}},
		{name: "one point is a line of one", input: []RoutePoint{pt(0, 0, 0, 0, 5)}, want: []int32{0}},
		{name: "a straight line simplifies to its two ends", input: straight(), want: []int32{0, 10}},
		{
			name: "a point with accuracy over 50 m is dropped",
			input: []RoutePoint{
				pt(0, 0, 0, 0, 5), pt(1, 0, 10, 5, 80), pt(2, 0, 20, 10, 5),
			},
			want: []int32{0, 2},
		},
		{
			name: "a point at exactly 50 m is kept, one just over is not",
			input: []RoutePoint{
				pt(0, 0, 0, 0, 5), pt(1, 40, 0, 5, 50), pt(2, 0, 100, 60, 50.01), pt(3, 0, 0, 120, 5),
			},
			// 0 -> 1 is 40 m in 5 s (8 m/s, fine), so 1 is a corner; 2 is refused; 3 closes the line.
			want: []int32{0, 1, 3},
		},
		{
			name: "a spike that would need 15 m/s or more is dropped",
			input: []RoutePoint{
				pt(0, 0, 0, 0, 5), pt(1, 0, 10, 5, 5),
				pt(2, 3000, 3000, 10, 5), // 4 km in 5 s
				pt(3, 0, 30, 15, 5), pt(4, 0, 40, 20, 5),
			},
			want: []int32{0, 4},
		},
		{
			name: "a corner is kept",
			input: []RoutePoint{
				pt(0, 0, 0, 0, 5), pt(1, 0, 50, 25, 5), pt(2, 50, 50, 50, 5),
			},
			want: []int32{0, 1, 2},
		},
		{
			name: "a wobble inside 3 m is dropped",
			input: []RoutePoint{
				pt(0, 0, 0, 0, 5), pt(1, 2, 50, 25, 5), pt(2, 0, 100, 50, 5),
			},
			want: []int32{0, 2},
		},
		{
			name: "the point nearest a scan is kept even when it is a wobble",
			input: []RoutePoint{
				pt(0, 0, 0, 0, 5), pt(1, 2, 50, 25, 5), pt(2, 0, 100, 50, 5),
			},
			scans: []geo.Point{{Lat: baseLat + 50*degPerMeter, Lng: baseLng}},
			want:  []int32{0, 1, 2},
		},
		{
			name: "a time that does not move forward is dropped",
			input: []RoutePoint{
				pt(0, 0, 0, 10, 5), pt(1, 0, 30, 5, 5), pt(2, 0, 100, 60, 5),
			},
			want: []int32{0, 2},
		},
		{
			// The first fix of a walk is the usual spike. Everything after it would be a "jump" from
			// it; after a few refusals the honest points are believed and the anchor is replaced.
			name: "a spike as the very first point does not erase the walk",
			input: func() []RoutePoint {
				out := []RoutePoint{pt(0, 5000, 5000, 0, 5)}
				for i := 1; i <= 9; i++ {
					out = append(out, pt(int32(i), 0, float64(i)*10, i*5, 5))
				}
				return out
			}(),
			want: []int32{6, 9}, // the walk from the sixth honest point on: five were refused first
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _ := BuildPath(tt.input, tt.scans)
			require.Equal(t, tt.want, seqs(path))
		})
	}
}

func TestBuildPathDistance(t *testing.T) {
	// 11 points, 10 m apart along a meridian: the walk is 100 m.
	var straight []RoutePoint
	for i := 0; i <= 10; i++ {
		straight = append(straight, pt(int32(i), 0, float64(i)*10, i*5, 5))
	}
	_, d := BuildPath(straight, nil)
	require.InDelta(t, 100, d, 0.5)

	// A square of 100 m, back to the start: 400 m. The four corners stay, the points between drop.
	var square []RoutePoint
	corners := [][2]float64{{0, 0}, {0, 100}, {100, 100}, {100, 0}, {0, 0}}
	seq := int32(0)
	sec := 0
	for i := 1; i < len(corners); i++ {
		for step := 0; step < 10; step++ {
			f := float64(step) / 10
			square = append(square, pt(seq, corners[i-1][0]+(corners[i][0]-corners[i-1][0])*f,
				corners[i-1][1]+(corners[i][1]-corners[i-1][1])*f, sec, 5))
			seq++
			sec += 5
		}
	}
	square = append(square, pt(seq, 0, 0, sec, 5))
	path, d := BuildPath(square, nil)
	require.InDelta(t, 400, d, 1)
	require.Len(t, path, 5, "the four corners and the way back")

	// Standing still: a cloud of points within 2 m is not a walk.
	var still []RoutePoint
	for i := 0; i < 40; i++ {
		still = append(still, pt(int32(i), float64(i%3), float64(i%2), i*10, 5))
	}
	_, d = BuildPath(still, nil)
	require.Less(t, d, 5.0, "the jitter of a guard at a post is not distance")
}

func TestBuildPathKeepsFirstAndLast(t *testing.T) {
	var zigzag []RoutePoint
	for i := 0; i < 50; i++ {
		zigzag = append(zigzag, pt(int32(i), float64(i%2)*20, float64(i)*10, i*5, 5))
	}
	path, _ := BuildPath(zigzag, nil)
	require.Equal(t, int32(0), path[0].Seq)
	require.Equal(t, int32(49), path[len(path)-1].Seq)
	require.Greater(t, len(path), 2, "a zigzag of 20 m is not a straight line")
}

func TestBuildPathCapsTheStoredPath(t *testing.T) {
	// 60 000 points a meter and a half apart north, each thrown up to 6 m sideways by a fixed
	// pseudo-random sequence (the golden angle never repeats): at 3 m the simplified line keeps most
	// of them, far more than the cap, so the tolerance has to grow until the line fits.
	const n = 60000
	noisy := make([]RoutePoint, 0, n)
	for i := 0; i < n; i++ {
		noisy = append(noisy, pt(int32(i), 6*math.Sin(float64(i)*2.399963), float64(i)*1.5, i, 5))
	}
	path, d := BuildPath(noisy, nil)
	require.LessOrEqual(t, len(path), maxPathPoints)
	require.Equal(t, int32(0), path[0].Seq)
	require.Equal(t, int32(n-1), path[len(path)-1].Seq)
	require.Greater(t, d, 89000.0, "the shape is kept: the walk is 90 km north")
}
