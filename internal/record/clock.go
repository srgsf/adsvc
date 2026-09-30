package record

import (
	"sync"
	"time"
)

// MaxSkew is how far in the future a record's time may be: records beyond it are
// rejected, and they never move a clock forward.
const MaxSkew = 10 * time.Minute

// Clock is a hybrid logical clock in unix milliseconds: never behind the wall clock, never
// backwards, and past every record time seen. Last-writer-wins then orders a node's own
// writes after whatever it has already read.
type Clock struct {
	mu   sync.Mutex
	last int64
	now  func() time.Time
}

// NewClock is a clock that has seen last (0 if nothing).
func NewClock(last int64) *Clock { return &Clock{last: last, now: time.Now} }

// Next is a new time, later than every time handed out or observed.
func (c *Clock) Next() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = max(c.now().UnixMilli(), c.last+1)
	return c.last
}

// Observe moves the clock past ts, unless ts is further in the future than MaxSkew.
func (c *Clock) Observe(ts int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ts <= c.now().Add(MaxSkew).UnixMilli() {
		c.last = max(c.last, ts)
	}
}

// Last is the latest time handed out or observed (to persist across restarts).
func (c *Clock) Last() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}
