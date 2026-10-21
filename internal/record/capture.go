package record

import (
	"fmt"
	"slices"
)

// Capture is a bounded copy of one output stream. It never blocks or fails the
// child: Write always reports the full length, even when bytes are dropped.
//
// Under Limit the copy is complete. Past it, the first Limit/5 bytes and the
// last 4·Limit/5 bytes are kept, so both the command banner and the failure
// summary (which test runners print last) survive. Bytes() marks the gap.
// ponytail: in memory; streams to the blob store when that lands (then Limit can grow).
type Capture struct {
	Limit     int64 `json:"limit"`
	Total     int64 `json:"total"`     // bytes the child wrote
	Truncated bool  `json:"truncated"` // some middle bytes were dropped

	head []byte
	ring []byte // tail ring buffer, allocated on first overflow
	pos  int    // next write index in ring
	full bool   // ring has wrapped at least once
}

func (c *Capture) Write(p []byte) (int, error) {
	n := len(p)
	c.Total += int64(n)
	headCap := int(c.Limit / 5)
	if room := headCap - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if c.ring == nil {
		c.ring = make([]byte, c.Limit-int64(headCap))
	}
	if len(p) >= len(c.ring) { // only the last len(ring) bytes can survive
		copy(c.ring, p[len(p)-len(c.ring):])
		c.pos, c.full = 0, true
	} else {
		k := copy(c.ring[c.pos:], p)
		if k < len(p) {
			copy(c.ring, p[k:])
			c.full = true
		}
		c.pos = (c.pos + len(p)) % len(c.ring)
		if c.pos == 0 {
			c.full = true
		}
	}
	c.Truncated = c.Total > c.Limit
	return n, nil
}

// Bytes returns the kept output in order, with an explicit marker where bytes were dropped.
func (c *Capture) Bytes() []byte {
	tail := c.ring[:c.pos]
	if c.full {
		tail = slices.Concat(c.ring[c.pos:], c.ring[:c.pos])
	}
	if !c.Truncated {
		return slices.Concat(c.head, tail)
	}
	omitted := c.Total - int64(len(c.head)) - int64(len(tail))
	marker := fmt.Appendf(nil, "\n… cinema: %d bytes omitted …\n", omitted)
	return slices.Concat(c.head, marker, tail)
}
