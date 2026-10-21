package record

// Capture is a bounded copy of one output stream. It never blocks or fails the
// child: Write always reports the full length, even when bytes are dropped.
type Capture struct {
	Limit     int64  `json:"limit"`
	Total     int64  `json:"total"`     // bytes the child wrote
	Truncated bool   `json:"truncated"` // Total > len(Data)
	Data      []byte `json:"-"`         // stored as a blob, never inlined in JSON
}

// Write keeps which bytes once the stream exceeds Limit?
//
// Decision point. Options:
//   - head only: simplest; but test runners print the failure summary LAST.
//   - tail only: keeps the failure; loses the command banner / config echo.
//   - head + tail (e.g. Limit/4 head, rest as a ring): best for humans, more code.
//
// Current: head only.
func (c *Capture) Write(p []byte) (int, error) {
	c.Total += int64(len(p))
	if room := c.Limit - int64(len(c.Data)); room > 0 {
		c.Data = append(c.Data, p[:min(int64(len(p)), room)]...)
	}
	c.Truncated = c.Total > int64(len(c.Data))
	return len(p), nil
}
