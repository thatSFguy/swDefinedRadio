// Package track turns a stream of decoded ADS-B squitters into live
// aircraft state.
package track

import (
	"math"
	"time"
)

// cprScale is 2^17, the full range of a CPR coordinate.
const cprScale = 131072.0

// nz is ICAO's number of latitude zones between the equator and a pole.
const nz = 15

// CPR is one compact position report: the raw coordinate pair plus the
// flag saying which of the two interleaved grids encoded it.
type CPR struct {
	Lat, Lon uint32
	Odd      bool
	Surface  bool
	At       time.Time
}

// mod is a modulo that returns a non-negative result, which is what the
// CPR formulas assume and what math.Mod does not give for negatives.
func mod(a, b float64) float64 {
	r := math.Mod(a, b)
	if r < 0 {
		r += b
	}
	return r
}

// nl gives the number of longitude zones at a latitude. Zones get wider
// towards the poles so that each stays roughly the same distance across.
func nl(lat float64) float64 {
	lat = math.Abs(lat)
	switch {
	case lat == 0:
		return 59
	case lat == 87:
		return 2
	case lat > 87:
		return 1
	}
	a := 1 - math.Cos(math.Pi/(2*nz))
	b := math.Cos(math.Pi / 180 * lat)
	return math.Floor(2 * math.Pi / math.Acos(1-a/(b*b)))
}

// GlobalAirborne recovers an absolute position from an even/odd pair of
// airborne reports, needing no prior knowledge of where the aircraft is.
//
// Both reports must come from the same aircraft close together in time —
// roughly ten seconds — or the shared zone index can come out wrong and
// place the aircraft hundreds of miles away.
func GlobalAirborne(even, odd CPR) (lat, lon float64, ok bool) {
	latE, lonE := float64(even.Lat)/cprScale, float64(even.Lon)/cprScale
	latO, lonO := float64(odd.Lat)/cprScale, float64(odd.Lon)/cprScale

	// j is the latitude zone index the two reports share.
	j := math.Floor(59*latE - 60*latO + 0.5)
	rlatE := (360.0 / 60) * (mod(j, 60) + latE)
	rlatO := (360.0 / 59) * (mod(j, 59) + latO)

	// CPR latitudes run 0-360; the southern hemisphere sits above 270.
	if rlatE >= 270 {
		rlatE -= 360
	}
	if rlatO >= 270 {
		rlatO -= 360
	}
	if math.Abs(rlatE) > 90 || math.Abs(rlatO) > 90 {
		return 0, 0, false
	}

	// A pair straddling a zone boundary cannot be combined; wait for the
	// next one rather than emitting a wrong fix.
	n := nl(rlatE)
	if n != nl(rlatO) {
		return 0, 0, false
	}

	// Longitude comes from whichever report is the more recent.
	m := math.Floor(lonE*(n-1) - lonO*n + 0.5)
	if odd.At.After(even.At) {
		lat = rlatO
		ni := math.Max(n-1, 1)
		lon = (360 / ni) * (mod(m, ni) + lonO)
	} else {
		lat = rlatE
		ni := math.Max(n, 1)
		lon = (360 / ni) * (mod(m, ni) + lonE)
	}
	if lon >= 180 {
		lon -= 360
	}
	return lat, lon, true
}

// Local recovers a position from a single report using a nearby
// reference point. It is unambiguous only within about 180 NM of the
// reference (45 NM for surface reports), but it works from one frame and
// so gives a fix sooner than waiting for an even/odd pair.
func Local(refLat, refLon float64, c CPR) (lat, lon float64) {
	span := 360.0
	if c.Surface {
		span = 90.0
	}
	cLat, cLon := float64(c.Lat)/cprScale, float64(c.Lon)/cprScale

	dLat := span / 60
	if c.Odd {
		dLat = span / 59
	}
	j := math.Floor(refLat/dLat) + math.Floor(mod(refLat, dLat)/dLat-cLat+0.5)
	lat = dLat * (j + cLat)

	n := nl(lat)
	if c.Odd {
		n--
	}
	dLon := span
	if n > 0 {
		dLon = span / n
	}
	m := math.Floor(refLon/dLon) + math.Floor(mod(refLon, dLon)/dLon-cLon+0.5)
	lon = dLon * (m + cLon)
	return lat, lon
}

// DistanceNM is the great-circle distance between two points in nautical
// miles.
func DistanceNM(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusNM = 3440.065
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return earthRadiusNM * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}
