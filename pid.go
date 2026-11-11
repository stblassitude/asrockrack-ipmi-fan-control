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
//
// Integral windup is prevented in two ways:
//   - conditional integration: the error is not accumulated while the
//     output is already saturated in the direction the error pushes it (but
//     it is accumulated when it moves the output back out of saturation, so
//     the integral always unwinds once the error changes sign);
//   - the integral term on its own (Ki*integral) is clamped to
//     [OutMin, OutMax], so it can never hold the output saturated by itself
//     for longer than it takes to integrate back across that range.
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
		derivative = (err - p.prevErr) / dt
	}
	p.prevErr = err

	pd := p.Kp*err + p.Kd*derivative

	if dt > 0 && p.Ki != 0 {
		before := pd + p.Ki*p.integral
		saturatedHigh := before >= p.OutMax && p.Ki*err > 0
		saturatedLow := before <= p.OutMin && p.Ki*err < 0
		if !saturatedHigh && !saturatedLow {
			p.integral += err * dt
		}

		lo, hi := p.OutMin/p.Ki, p.OutMax/p.Ki
		if lo > hi {
			lo, hi = hi, lo
		}
		p.integral = clampFloat(p.integral, lo, hi)
	}

	return clampFloat(pd+p.Ki*p.integral, p.OutMin, p.OutMax)
}

// Reset clears accumulated state, e.g. after a safety override.
func (p *PID) Reset() {
	p.integral = 0
	p.prevErr = 0
	p.primed = false
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
