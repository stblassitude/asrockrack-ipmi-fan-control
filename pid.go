package main

import "time"

// PID is a simple PID controller producing a duty-cycle percentage.
// Positive error (measured above setpoint) increases output (more cooling).
type PID struct {
	Kp, Ki, Kd float64
	OutMin     float64
	OutMax     float64

	integral float64
	prevErr  float64
	lastTime time.Time
	primed   bool
}

// Update runs one PID step and returns the clamped output.
func (p *PID) Update(setpoint, measured float64, now time.Time) float64 {
	err := measured - setpoint

	var dt float64
	if p.primed {
		dt = now.Sub(p.lastTime).Seconds()
	}
	p.lastTime = now
	p.primed = true

	var derivative float64
	if dt > 0 {
		p.integral += err * dt
		derivative = (err - p.prevErr) / dt
	}
	p.prevErr = err

	out := p.Kp*err + p.Ki*p.integral + p.Kd*derivative

	if out > p.OutMax {
		if dt > 0 && p.Ki != 0 {
			p.integral -= err * dt // anti-windup: undo the integration that pushed us over
		}
		out = p.OutMax
	} else if out < p.OutMin {
		if dt > 0 && p.Ki != 0 {
			p.integral -= err * dt
		}
		out = p.OutMin
	}

	return out
}

// Reset clears accumulated state, e.g. after a safety override.
func (p *PID) Reset() {
	p.integral = 0
	p.prevErr = 0
	p.primed = false
}
