package main

import (
	"fmt"
	"os"
	"time"

	"github.com/u-root/u-root/pkg/ipmi"
)

// ASRock Rack's BMC on the X570D4U (confirmed against firmware 1.35,
// product ID 0x1005) exposes an OEM fan control interface under NetFn 0x3a
// that differs from the "raw 0x3a 0x01 <8 bytes>" command documented in
// various community write-ups for older/sibling boards -- this firmware
// rejects that command outright with completion code 0xc1 ("invalid
// command"), confirmed both through this tool and directly with ipmitool.
//
// The general shape of the protocol implemented here (which commands
// exist, roughly what they do) comes from
// https://github.com/pjbulls/x570d4ufans-plugin (see apply_fan_speed.sh),
// which reverse-engineered it for this board on different firmware. The
// exact payload lengths below were independently confirmed against real
// X570D4U hardware on firmware 1.35, since that plugin's assumptions
// (e.g. an 18-byte mode payload) turned out not to match this firmware
// revision (it returns completion code 0xc7, "request data length
// invalid"):
//
//   - cmd 0xd8, 16 data bytes, all 0x01: switch every fan zone to manual
//     (host-controlled) duty cycles. All 0x00 switches back to the BMC's
//     automatic control tables.
//   - cmd 0xd6, 16 data bytes: set the duty cycle for the board's 6 real
//     fan zones (FAN1, FAN2, FAN3, FAN4_1, FAN5_1, FAN6_1), followed by 10
//     padding bytes for header slots this board doesn't expose.
//   - cmd 0xd7, no data: some kind of fan status read (x570d4ufans-plugin
//     calls this "get_fan_speed"), but on this firmware its trailing bytes
//     read back as 0x64 (100 decimal) -- outside the 0-63 range the set
//     command actually accepts (see below) -- so it appears to report in a
//     different unit and is not used by this tool.
//   - cmd 0xda, no data: also read back current fan state; on this
//     firmware every byte of its response falls inside the same raw range
//     the set command accepts, and it was used here to read back the
//     board's current duty cycle as the starting point for ramping (see
//     maxDutyStep below).
//
// The duty byte is not a plain percentage. On this board/firmware the BMC
// uses a 30-63 raw scale, where a 20% duty request maps to 30 and 100%
// maps to 63 (0x3f), linearly in between -- confirmed on real hardware: 64
// (0x40) is rejected with completion code 0xcc ("invalid data field in
// request"), and so is 19 (0x13). 30 (0x1e) is the lowest value confirmed
// to work (used repeatedly, including as a uniform value across all 16
// bytes); the true floor may be lower still, somewhere in (19, 30), but
// that range was never pinned down more precisely, so 30 is used here.
//
//	raw = 30 + (percent-20)*33/80        (percent clamped to [20,100])
//
// 20% is the lowest duty this command accepts; it happens to match this
// tool's MinDuty default.
//
// The set command also rejects large single-step changes (also completion
// code 0xcc): going straight from raw 63 to 40 in one command failed, but
// 63->56->48->40 in three steps succeeded. So SetDuty rate-limits how much
// each byte is allowed to move per call (maxDutyStep) and lets duty
// converge over multiple calls -- in practice, multiple calls to the
// control loop's tick.
const (
	netfnOEM      ipmi.NetFn   = 0x3a
	cmdSetFanMode ipmi.Command = 0xd8
	cmdSetFanDuty ipmi.Command = 0xd6
	cmdGetFanDuty ipmi.Command = 0xda

	oemFanModeBytes = 16
	oemFanDutyBytes = 16 // 6 real fan zones + 10 unused padding slots
	oemFanZoneCount = 6  // FAN1, FAN2, FAN3, FAN4_1, FAN5_1, FAN6_1

	oemDutyRawMin   = 30
	oemDutyRawMax   = 63
	oemDutyPctFloor = 20

	// oemIdleRaw is applied to the 10 padding slots the board doesn't use.
	// Set to oemDutyRawMin, the lowest value confirmed valid on real hardware.
	oemIdleRaw = oemDutyRawMin

	// maxDutyStep is the largest per-call change (in raw units) applied to
	// any single byte. Confirmed-safe steps of 7-8 succeeded on real
	// hardware; this stays comfortably under that.
	maxDutyStep = 8
)

// FanController sends the OEM raw commands that control fan duty cycles.
type FanController struct {
	ipmi     *ipmi.IPMI
	fanCount int

	// last holds the raw byte values (length oemFanDutyBytes) most recently
	// sent to (or read back from) the BMC, used to rate-limit how far the
	// next SetDuty call is allowed to move each byte. nil until the first
	// SetDuty call, which seeds it by reading the BMC's current state.
	last []byte
}

func NewFanController(h *ipmi.IPMI, fanCount int) *FanController {
	return &FanController{ipmi: h, fanCount: fanCount}
}

// EnableManualControl switches every fan zone from BMC automatic control to
// manual, host-controlled duty cycles. Call this once before the first
// SetDuty call; the BMC stays in manual mode until DisableManualControl is
// called or the BMC itself is reset/power cycled.
func (f *FanController) EnableManualControl() error {
	return f.setMode(0x01)
}

// DisableManualControl returns fan control to the BMC's automatic tables.
func (f *FanController) DisableManualControl() error {
	return f.setMode(0x00)
}

func (f *FanController) setMode(b byte) error {
	payload := make([]byte, oemFanModeBytes)
	for i := range payload {
		payload[i] = b
	}
	param := append([]byte{byte(netfnOEM), byte(cmdSetFanMode)}, payload...)
	if _, err := f.ipmi.RawCmd(param); err != nil {
		return fmt.Errorf("set fan mode: %w", err)
	}
	return nil
}

// percentToRaw converts a 0-100 duty percentage to this board's raw duty
// byte scale (15-63). Values below 20% are raised to 20%, the lowest duty
// the board's manual control accepts.
func percentToRaw(percent int) byte {
	if percent < oemDutyPctFloor {
		percent = oemDutyPctFloor
	}
	if percent > 100 {
		percent = 100
	}
	raw := oemDutyRawMin + (percent-oemDutyPctFloor)*(oemDutyRawMax-oemDutyRawMin)/(100-oemDutyPctFloor)
	return byte(raw)
}

// clampStep moves current towards target by at most maxStep.
func clampStep(current, target byte, maxStep int) byte {
	diff := int(target) - int(current)
	if diff > maxStep {
		diff = maxStep
	} else if diff < -maxStep {
		diff = -maxStep
	}
	return byte(int(current) + diff)
}

// readCurrentDuty reads the BMC's current raw fan duty bytes, used to seed
// the ramp in SetDuty.
func (f *FanController) readCurrentDuty() ([]byte, error) {
	param := []byte{byte(netfnOEM), byte(cmdGetFanDuty)}
	resp, err := f.ipmi.RawCmd(param)
	if err != nil {
		return nil, fmt.Errorf("get fan duty: %w", err)
	}
	// resp[0] is the completion code; RawCmd already validated it is OK.
	if len(resp) < 1+oemFanDutyBytes {
		return nil, fmt.Errorf("get fan duty: short response (%d bytes)", len(resp))
	}
	return resp[1 : 1+oemFanDutyBytes], nil
}

// SetDuty sets the duty cycle percentage (0-100) for each of the board's
// real fan zones. duty must have exactly fanCount entries (expected to be
// oemFanZoneCount: FAN1, FAN2, FAN3, FAN4_1, FAN5_1, FAN6_1). The remaining
// header slots up to the fixed 16-byte command width are filled with a
// safe idle value, matching the board's own reference implementation.
//
// Each byte only moves by up to maxDutyStep per call towards its target;
// reaching a far-away target takes multiple calls (in practice, multiple
// control loop ticks). The returned bool is true once every byte has
// reached its target (no more ramping needed for this duty).
func (f *FanController) SetDuty(duty []int) (bool, error) {
	if len(duty) != f.fanCount {
		return false, fmt.Errorf("expected %d duty values, got %d", f.fanCount, len(duty))
	}

	if f.last == nil {
		cur, err := f.readCurrentDuty()
		if err != nil {
			return false, fmt.Errorf("seeding fan duty ramp: %w", err)
		}
		fmt.Fprintf(os.Stderr, "fan duty ramp seeded from BMC: % x\n", cur)
		f.last = cur
	}
	prev := f.last

	target := make([]byte, oemFanDutyBytes)
	for i := range target {
		target[i] = oemIdleRaw
	}
	for i, d := range duty {
		if i >= oemFanDutyBytes {
			break
		}
		target[i] = percentToRaw(d)
	}

	payload := make([]byte, oemFanDutyBytes)
	converged := true
	for i := range payload {
		payload[i] = clampStep(prev[i], target[i], maxDutyStep)
		if payload[i] != target[i] {
			converged = false
		}
	}

	param := append([]byte{byte(netfnOEM), byte(cmdSetFanDuty)}, payload...)
	if _, err := f.ipmi.RawCmd(param); err != nil {
		return false, fmt.Errorf("set fan duty: %w (prev=% x target=% x payload=% x)", err, prev, target, payload)
	}
	f.last = payload
	return converged, nil
}

// RampToBlocking calls SetDuty repeatedly, sleeping sleepBetween in between,
// until the target duty is fully reached or timeout elapses. Used on
// shutdown, where a single rate-limited SetDuty call usually can't reach a
// far-away safety duty in one step.
func (f *FanController) RampToBlocking(duty []int, timeout, sleepBetween time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		converged, err := f.SetDuty(duty)
		if err != nil || converged {
			return err
		}
		if time.Now().Add(sleepBetween).After(deadline) {
			return nil
		}
		time.Sleep(sleepBetween)
	}
}
