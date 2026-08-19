package sb

// Google's encoded-polyline algorithm: delta, zigzag, five-bit chunks offset by 63.
//
// Notable applies it to each coordinate axis independently at precision 2 — see
// `EncodePolyline.kt` in the BOOX app and `Polyline.swift` in the iPad's NotableKit, which this
// mirrors. Only decoding is implemented: this service reads ink and never writes it.

// decodePolyline returns the coordinates encoded in s, scaled down by 10^precision.
//
// A trailing incomplete chunk group — which only malformed input produces — is dropped rather
// than reported, matching both existing implementations. Being the odd one out here would mean a
// page that renders on the tablets and fails on the server.
func decodePolyline(s string, precision int) []float32 {
	factor := pow10(precision)

	// Chunks are gathered per coordinate: the high bit says "another chunk follows".
	groups := make([][]int, 1, len(s)/2+1)
	for _, r := range []byte(s) {
		value := int(r) - 63
		last := value&0x20 == 0
		value &= 0x1F
		groups[len(groups)-1] = append(groups[len(groups)-1], value)
		if last {
			groups = append(groups, nil)
		}
	}
	groups = groups[:len(groups)-1]

	out := make([]float32, 0, len(groups))
	var prev float64
	for _, group := range groups {
		coordinate := 0
		for i, chunk := range group {
			coordinate |= chunk << (i * 5)
		}
		// Zigzag: the low bit is the sign.
		if coordinate&1 != 0 {
			coordinate = ^coordinate
		}
		coordinate >>= 1
		prev += float64(coordinate) / factor
		// Rounded at the same scale the value was stored at, so a coordinate that survived a
		// round trip through the tablets reads back identically here.
		out = append(out, float32(round(prev*factor)/factor))
	}
	return out
}

func pow10(n int) float64 {
	result := 1.0
	for i := 0; i < n; i++ {
		result *= 10
	}
	return result
}

// round is math.Round's half-away-from-zero rule, spelled out to match what Swift's
// `rounded()` and Kotlin's `Math.round` agree on for these values.
func round(v float64) float64 {
	if v < 0 {
		return -float64(int64(-v + 0.5))
	}
	return float64(int64(v + 0.5))
}
