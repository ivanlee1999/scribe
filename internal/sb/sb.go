// Package sb decodes Notable's "SB" stroke-points binary.
//
// This is the third implementation of the format, after `StrokePointConverter.kt` on the BOOX
// and `SBStrokeCodec.swift` on the iPad. It decodes only — the server reads ink and never
// writes it — which keeps it to a fraction of what the other two carry.
//
// Layout, version 2, little-endian:
//
//	'S' 'B' version(1) mask(1) count(int32) compression(1)
//	[if LZ4] rawBodySize(int32)
//	body: int32 xLen, x polyline (precision 2)
//	      int32 yLen, y polyline
//	      [mask&1] uint16[count] pressure, quantized against 65535
//	      [mask&2] int8[count]   tiltX
//	      [mask&4] int8[count]   tiltY
//	      [mask&8] uint16[count] dt
//
// LZ4 is the raw block format, applied to the whole body when it reaches 512 bytes and
// compression saves at least a quarter.
package sb

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/pierrec/lz4/v4"
)

// Point is one sample from a pen. Optional channels are nil when the stroke did not record them;
// which channels exist is a property of the whole stroke, not of individual points.
type Point struct {
	X, Y     float32
	Pressure *float32
	TiltX    *int8
	TiltY    *int8
	DT       *uint16
}

const (
	maskPressure = 1 << 0
	maskTiltX    = 1 << 1
	maskTiltY    = 1 << 2
	maskDT       = 1 << 3

	formatVersion = 2
	headerSize    = 9
	pressureScale = 65535.0
	precision     = 2
)

var (
	ErrBadMagic    = errors.New("not an SB payload")
	ErrTruncated   = errors.New("payload ends mid-field")
	ErrPointCounts = errors.New("x and y hold different numbers of points")
)

// Decode parses an SB payload into its points.
//
// An empty payload decodes to no points rather than an error: a stroke can legitimately carry
// none, and the tablets both write a two-byte placeholder for one.
func Decode(data []byte) ([]Point, error) {
	if len(data) < headerSize {
		// Anything this short cannot be a v2 payload. Treated as "no ink" rather than as a
		// failure, so one odd stroke cannot stop a page from being recognized.
		return nil, nil
	}
	if data[0] != 'S' || data[1] != 'B' {
		return nil, ErrBadMagic
	}
	if version := data[2]; version != formatVersion {
		return nil, fmt.Errorf("unsupported SB version %d", version)
	}

	mask := int(data[3])
	count := int(int32(binary.LittleEndian.Uint32(data[4:8])))
	if count < 0 {
		return nil, fmt.Errorf("negative point count %d", count)
	}
	if count == 0 {
		return nil, nil
	}

	compressed := data[8]
	body := data[headerSize:]

	switch compressed {
	case 0:
	case 1:
		if len(body) < 4 {
			return nil, ErrTruncated
		}
		rawSize := int(int32(binary.LittleEndian.Uint32(body[:4])))
		if rawSize < 0 {
			return nil, fmt.Errorf("negative decompressed size %d", rawSize)
		}
		out := make([]byte, rawSize)
		n, err := lz4.UncompressBlock(body[4:], out)
		if err != nil {
			return nil, fmt.Errorf("lz4: %w", err)
		}
		body = out[:n]
	default:
		return nil, fmt.Errorf("unknown compression flag %d", compressed)
	}

	r := &reader{buf: body}

	xs, err := r.polyline()
	if err != nil {
		return nil, err
	}
	ys, err := r.polyline()
	if err != nil {
		return nil, err
	}
	if len(xs) != len(ys) {
		return nil, ErrPointCounts
	}
	// The count in the header is authoritative for the fixed-width channels that follow, so a
	// disagreement with the polylines would misread every one of them.
	if len(xs) != count {
		return nil, fmt.Errorf("%w: header says %d, coordinates give %d",
			ErrPointCounts, count, len(xs))
	}

	points := make([]Point, count)
	for i := range points {
		points[i] = Point{X: xs[i], Y: ys[i]}
	}

	if mask&maskPressure != 0 {
		for i := 0; i < count; i++ {
			raw, err := r.uint16()
			if err != nil {
				return nil, err
			}
			value := float32(raw) / pressureScale
			points[i].Pressure = &value
		}
	}
	if mask&maskTiltX != 0 {
		if err := r.int8s(count, func(i int, v int8) { points[i].TiltX = &v }); err != nil {
			return nil, err
		}
	}
	if mask&maskTiltY != 0 {
		if err := r.int8s(count, func(i int, v int8) { points[i].TiltY = &v }); err != nil {
			return nil, err
		}
	}
	if mask&maskDT != 0 {
		for i := 0; i < count; i++ {
			raw, err := r.uint16()
			if err != nil {
				return nil, err
			}
			value := raw
			points[i].DT = &value
		}
	}

	return points, nil
}

type reader struct {
	buf []byte
	pos int
}

func (r *reader) take(n int) ([]byte, error) {
	if n < 0 || r.pos+n > len(r.buf) {
		return nil, ErrTruncated
	}
	out := r.buf[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

func (r *reader) int32() (int, error) {
	b, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return int(int32(binary.LittleEndian.Uint32(b))), nil
}

func (r *reader) uint16() (uint16, error) {
	b, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (r *reader) int8s(count int, set func(i int, v int8)) error {
	b, err := r.take(count)
	if err != nil {
		return err
	}
	for i, v := range b {
		set(i, int8(v))
	}
	return nil
}

func (r *reader) polyline() ([]float32, error) {
	length, err := r.int32()
	if err != nil {
		return nil, err
	}
	b, err := r.take(length)
	if err != nil {
		return nil, err
	}
	return decodePolyline(string(b), precision), nil
}
