package main

import (
	"io"
	"math"
	"math/rand"
)

// gear is the gear-hash table. Seed 0x45524f4653 ("EROFS") with math/rand's
// Go 1 source: its sequence is frozen by the Go 1 compatibility promise, so
// chunk boundaries stay stable across runs and builds. Never change the seed.
var gear = func() (t [256]uint64) {
	r := rand.New(rand.NewSource(0x45524f4653))
	for i := range t {
		t[i] = r.Uint64()
	}
	return
}()

// split reads r sequentially and calls emit with each content-defined chunk
// (emit owns the slice). Cut when len >= min and the low bits of the gear hash
// are zero, or when len == max; the last chunk may be shorter. The mask has
// round(log2(avg-min)) bits, so the mean chunk is ~avg (before the max clamp).
// min must be >= 64.
func split(r io.Reader, min, avg, max int, emit func([]byte) error) error {
	mask := uint64(1)<<int(math.Round(math.Log2(float64(avg-min)))) - 1
	buf := make([]byte, 4<<20+max)
	start, end := 0, 0
	eof := false
	for {
		if !eof && end-start < max {
			end = copy(buf, buf[start:end])
			start = 0
			for !eof && end < len(buf) {
				n, err := r.Read(buf[end:])
				end += n
				if err == io.EOF {
					eof = true
				} else if err != nil {
					return err
				}
			}
		}
		if start == end {
			return nil
		}
		b := buf[start:end]
		n := len(b)
		if n > max {
			n = max
		}
		if n > min {
			// The 64-bit gear hash only depends on the last 64 bytes, so
			// starting at min-64 gives the same cuts as hashing from 0.
			var h uint64
			for i := min - 64; i < n; i++ {
				h = h<<1 + gear[b[i]]
				if i+1 >= min && h&mask == 0 {
					n = i + 1
					break
				}
			}
		}
		c := make([]byte, n)
		copy(c, b)
		if err := emit(c); err != nil {
			return err
		}
		start += n
	}
}
