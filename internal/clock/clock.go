package clock

import "time"

type Clock interface {
	Now() time.Time
	After(duration time.Duration) <-chan time.Time
}

type Real struct{}

// Durable domain commands require the canonical UTC location. Keeping the
// production source canonical also covers provider/cache timestamps composed
// outside the foreground stage transitions.
func (Real) Now() time.Time                                { return time.Now().UTC() }
func (Real) After(duration time.Duration) <-chan time.Time { return time.After(duration) }
