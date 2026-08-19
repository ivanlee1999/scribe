package sb

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The golden payloads in testdata were produced by the iPad's `SBStrokeCodec.encode`, so these
// tests check this decoder against an implementation that is already in production rather than
// against my own reading of the format. That is the whole point of having them: a third
// implementation of a binary format is exactly where a plausible misreading survives review.
type vector struct {
	Name        string      `json:"name"`
	Points      [][]float64 `json:"points"`
	HasPressure bool        `json:"hasPressure"`
	HasTilt     bool        `json:"hasTilt"`
	HasDt       bool        `json:"hasDt"`
	Hex         string      `json:"hex"`
}

// truncate2 is what the encoders do to a coordinate: `Int(v * 100)`, dropping the remainder
// toward zero rather than rounding it.
func truncate2(v float64) float64 {
	return math.Trunc(v*100) / 100
}

func load(t *testing.T) []vector {
	t.Helper()
	data, err := os.ReadFile("testdata/sb-vectors.json")
	if err != nil {
		t.Fatalf("reading vectors: %v", err)
	}
	var vectors []vector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("parsing vectors: %v", err)
	}
	if len(vectors) == 0 {
		t.Fatal("no vectors")
	}
	return vectors
}

func TestDecodeMatchesTheiPadEncoder(t *testing.T) {
	for _, v := range load(t) {
		t.Run(v.Name, func(t *testing.T) {
			payload, err := hex.DecodeString(v.Hex)
			if err != nil {
				t.Fatalf("bad hex: %v", err)
			}

			points, err := Decode(payload)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(points) != len(v.Points) {
				t.Fatalf("got %d points, want %d", len(points), len(v.Points))
			}

			for i, want := range v.Points {
				got := points[i]
				// Coordinates come back *truncated* to hundredths, not rounded: the encoders
				// both spell the conversion `Int(value * 100)`, which drops the remainder
				// rather than rounding it. So 903.1599 is stored as 90315 and decodes to
				// 903.15 — a coordinate can legitimately return up to a full hundredth nearer
				// zero than it went in. Asserting the exact truncation rather than a tolerance
				// is what makes this a check on the format instead of a check on my patience.
				if diff := math.Abs(float64(got.X) - truncate2(want[0])); diff > 1e-4 {
					t.Errorf("point %d: x = %v, want %v (truncated from %v)",
						i, got.X, truncate2(want[0]), want[0])
				}
				if diff := math.Abs(float64(got.Y) - truncate2(want[1])); diff > 1e-4 {
					t.Errorf("point %d: y = %v, want %v (truncated from %v)",
						i, got.Y, truncate2(want[1]), want[1])
				}

				next := 2
				if v.HasPressure {
					if got.Pressure == nil {
						t.Fatalf("point %d: pressure missing", i)
					}
					// Quantized to 1/65535 on the way out.
					if math.Abs(float64(*got.Pressure)-want[next]) > 0.0001 {
						t.Errorf("point %d: pressure = %v, want %v", i, *got.Pressure, want[next])
					}
					next++
				} else if got.Pressure != nil {
					t.Errorf("point %d: pressure appeared from nowhere", i)
				}

				if v.HasTilt {
					if got.TiltX == nil || got.TiltY == nil {
						t.Fatalf("point %d: tilt missing", i)
					}
					if float64(*got.TiltX) != want[next] {
						t.Errorf("point %d: tiltX = %v, want %v", i, *got.TiltX, want[next])
					}
					next++
					if float64(*got.TiltY) != want[next] {
						t.Errorf("point %d: tiltY = %v, want %v", i, *got.TiltY, want[next])
					}
					next++
				} else if got.TiltX != nil || got.TiltY != nil {
					t.Errorf("point %d: tilt appeared from nowhere", i)
				}

				if v.HasDt {
					if got.DT == nil {
						t.Fatalf("point %d: dt missing", i)
					}
					if float64(*got.DT) != want[next] {
						t.Errorf("point %d: dt = %v, want %v", i, *got.DT, want[next])
					}
				} else if got.DT != nil {
					t.Errorf("point %d: dt appeared from nowhere", i)
				}
			}
		})
	}
}

func TestBothCompressionBranchesAreCovered(t *testing.T) {
	// A decoder that only ever sees short payloads passes every other test here while getting
	// LZ4 wrong, and long strokes are the common case in real notes.
	var plain, compressed int
	for _, v := range load(t) {
		payload, err := hex.DecodeString(v.Hex)
		if err != nil || len(payload) < headerSize {
			t.Fatalf("%s: unusable vector", v.Name)
		}
		switch payload[8] {
		case 0:
			plain++
		case 1:
			compressed++
		}
	}
	if plain == 0 || compressed == 0 {
		t.Fatalf("vectors cover only one compression branch: %d plain, %d compressed",
			plain, compressed)
	}
}

func TestAPayloadTooShortToBeInkDecodesToNothing(t *testing.T) {
	// The tablets write a two-byte placeholder for a stroke with no points. Refusing it would
	// stop the whole page being recognized over ink that was never there.
	for _, payload := range [][]byte{nil, {}, {0x00, 0x00}, {'S', 'B'}} {
		points, err := Decode(payload)
		if err != nil {
			t.Errorf("Decode(%x): %v", payload, err)
		}
		if len(points) != 0 {
			t.Errorf("Decode(%x) gave %d points", payload, len(points))
		}
	}
}

func TestSomethingThatIsNotInkIsRefused(t *testing.T) {
	// Long enough to pass the length check, so the magic is what has to catch it.
	_, err := Decode([]byte("not an SB payload at all"))
	if err != ErrBadMagic {
		t.Errorf("got %v, want ErrBadMagic", err)
	}
}

func TestAFutureFormatIsRefusedRatherThanMisread(t *testing.T) {
	payload := []byte{'S', 'B', 99, 0, 1, 0, 0, 0, 0}

	if _, err := Decode(payload); err == nil {
		t.Error("a version this decoder does not know was accepted")
	}
}

func TestATruncatedPayloadIsReported(t *testing.T) {
	// The header promises two points and a pressure channel; the body stops early.
	payload := []byte{'S', 'B', 2, maskPressure, 2, 0, 0, 0, 0, 2, 0, 0, 0}

	if _, err := Decode(payload); err == nil {
		t.Error("a truncated payload decoded without complaint")
	}
}

func TestPolylineRoundsTheWayTheTabletsDo(t *testing.T) {
	// Precision 2, so the decoder must land on hundredths rather than accumulating drift across
	// a long stroke of deltas.
	for _, v := range load(t) {
		if v.Name != "fractional-and-backwards" {
			continue
		}
		payload, _ := hex.DecodeString(v.Hex)
		points, err := Decode(payload)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		for i, p := range points {
			if math.Abs(float64(p.X)*100-math.Round(float64(p.X)*100)) > 1e-3 {
				t.Errorf("point %d: x = %v is not on a hundredth", i, p.X)
			}
		}
		return
	}
	t.Fatal("the fractional vector is missing")
}
