package main

import (
	"testing"
	"time"
)

func newTestPID() *PID {
	return &PID{Kp: defaultGeneralKp, Ki: defaultGeneralKi, Kd: defaultGeneralKd, OutMin: 0, OutMax: 100}
}

// A sensor held a few degrees above target for a long time saturates the
// loop at 100%. Once it drops below target, the output must come back down
// promptly instead of staying pinned at 100 by a wound-up integral.
func TestPIDUnwindsAfterLongSaturation(t *testing.T) {
	const (
		target   = 35.0
		interval = 5 * time.Second
	)
	p := newTestPID()
	now := time.Unix(0, 0)

	var out float64
	for i := 0; i < 10000; i++ {
		out = p.Update(target, target+4, now)
		now = now.Add(interval)
	}
	if out != 100 {
		t.Fatalf("expected output saturated at 100 after holding +4 above target, got %v", out)
	}
	if term := p.Ki * p.integral; term > p.OutMax {
		t.Fatalf("integral term %v exceeds OutMax %v", term, p.OutMax)
	}

	out = p.Update(target, target-2, now)
	now = now.Add(interval)
	if out >= 100 {
		t.Fatalf("output still %v on the first tick below target", out)
	}

	// With the default gains, the integral unwinds at Ki*2K*5s = 0.3% per
	// tick, so it reaches 20% (the default min_duty) after ~190 ticks and 0
	// after ~280. Allow some slack.
	reachedMin, reachedZero := -1, -1
	for i := 1; i <= 400; i++ {
		out = p.Update(target, target-2, now)
		now = now.Add(interval)
		if reachedMin < 0 && out <= 20 {
			reachedMin = i
		}
		if reachedZero < 0 && out == 0 {
			reachedZero = i
			break
		}
	}
	if reachedMin < 0 || reachedMin > 250 {
		t.Errorf("output did not drop to 20%% within 250 ticks below target (reached at %d)", reachedMin)
	}
	if reachedZero < 0 || reachedZero > 350 {
		t.Errorf("output did not drop to 0%% within 350 ticks below target (reached at %d)", reachedZero)
	}
}

// A sensor held far below target must not build up negative integral that
// would delay the response once it goes above target.
func TestPIDNoNegativeWindup(t *testing.T) {
	p := newTestPID()
	now := time.Unix(0, 0)
	for i := 0; i < 10000; i++ {
		p.Update(50, 30, now)
		now = now.Add(5 * time.Second)
	}
	p.Update(50, 54, now)
	now = now.Add(5 * time.Second)
	if out := p.Update(50, 54, now); out < p.Kp*4 {
		t.Fatalf("expected at least the proportional response %v above target, got %v", p.Kp*4, out)
	}
}
