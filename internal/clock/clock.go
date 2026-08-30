package clock

import "time"

type Clock interface {
	Now() time.Time
	After(duration time.Duration) <-chan time.Time
}

type Real struct{}

func (Real) Now() time.Time                                { return time.Now() }
func (Real) After(duration time.Duration) <-chan time.Time { return time.After(duration) }
