package main

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"
)

// fakeBMC implements rawCommander. Reads of the current duty (cmd 0xda)
// return reads[0], reads[1], ... (repeating the last one); set duty
// commands (cmd 0xd6) are recorded and fail while failSets > 0.
type fakeBMC struct {
	reads    [][]byte
	nReads   int
	sets     [][]byte
	failSets int
}

func (b *fakeBMC) RawCmd(param []byte) ([]byte, error) {
	switch param[1] {
	case byte(cmdGetFanDuty):
		r := b.reads[min(b.nReads, len(b.reads)-1)]
		b.nReads++
		return append([]byte{0x00}, r...), nil
	case byte(cmdSetFanDuty):
		b.sets = append(b.sets, append([]byte(nil), param[2:]...))
		if b.failSets > 0 {
			b.failSets--
			return nil, errors.New("invalid data field in request")
		}
		return []byte{0x00}, nil
	}
	return nil, errors.New("unexpected command")
}

func newTestFanController(bmc *fakeBMC) *FanController {
	return NewFanController(bmc, oemFanZoneCount, slog.New(slog.DiscardHandler))
}

func uniformDuty(pct int) []int {
	d := make([]int, oemFanZoneCount)
	for i := range d {
		d[i] = pct
	}
	return d
}

func raw(b ...byte) []byte { return b }

// The production seed: valid real zones, garbage in the padding slots.
var garbledSeed = raw(0x28, 0x28, 0x28, 0x28, 0x28, 0x2d, 0x2d, 0x2d, 0, 0, 0, 0, 0, 0, 0, 0)

func TestSetDutyPaddingIsAlwaysPassThrough(t *testing.T) {
	bmc := &fakeBMC{reads: [][]byte{garbledSeed}}
	f := newTestFanController(bmc)

	for i := 0; i < 10; i++ {
		if _, err := f.SetDuty(uniformDuty(100)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := f.SetDuty(uniformDuty(20)); err != nil {
			t.Fatal(err)
		}
	}

	var prev []byte = garbledSeed
	for n, p := range bmc.sets {
		for i := oemFanZoneCount; i < oemFanDutyBytes; i++ {
			if p[i] != oemIdleRaw {
				t.Fatalf("set %d: padding slot %d = 0x%02x, want 0x%02x (payload % x)", n, i, p[i], oemIdleRaw, p)
			}
		}
		for i := 0; i < oemFanZoneCount; i++ {
			if d := int(p[i]) - int(prev[i]); d > maxDutyStep || d < -maxDutyStep {
				t.Fatalf("set %d: zone %d moved by %d (payload % x)", n, i, d, p)
			}
			if p[i] < oemDutyRawMin || p[i] > oemDutyRawMax {
				t.Fatalf("set %d: zone %d = 0x%02x out of range", n, i, p[i])
			}
		}
		prev = p
	}
	if last := bmc.sets[len(bmc.sets)-1]; !bytes.Equal(last[:oemFanZoneCount], bytes.Repeat([]byte{oemDutyRawMin}, oemFanZoneCount)) {
		t.Fatalf("did not converge to min duty: % x", last)
	}
}

func TestSeedRetriesInvalidRead(t *testing.T) {
	good := raw(0x28, 0x28, 0x28, 0x28, 0x28, 0x28, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	bad := raw(0x28, 0x00, 0x28, 0x28, 0x28, 0x28, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	bmc := &fakeBMC{reads: [][]byte{bad, good}}
	f := newTestFanController(bmc)

	if _, err := f.SetDuty(uniformDuty(100)); err != nil {
		t.Fatal(err)
	}
	if bmc.nReads != 2 {
		t.Fatalf("expected 2 seed reads, got %d", bmc.nReads)
	}
	if got := bmc.sets[0][0]; got != 0x28+maxDutyStep {
		t.Fatalf("expected ramp from the valid seed, got first zone 0x%02x", got)
	}
}

func TestSeedFallsBackToTarget(t *testing.T) {
	bad := make([]byte, oemFanDutyBytes)
	bmc := &fakeBMC{reads: [][]byte{bad}}
	f := newTestFanController(bmc)

	if _, err := f.SetDuty(uniformDuty(50)); err != nil {
		t.Fatal(err)
	}
	if bmc.nReads != seedReadAttempts {
		t.Fatalf("expected %d seed reads, got %d", seedReadAttempts, bmc.nReads)
	}
	want := bytes.Repeat([]byte{percentToRaw(50)}, oemFanZoneCount)
	if got := bmc.sets[0][:oemFanZoneCount]; !bytes.Equal(got, want) {
		t.Fatalf("expected target sent as-is, got % x", got)
	}
}

func TestFailedWriteReseedsFromBMC(t *testing.T) {
	first := raw(0x28, 0x28, 0x28, 0x28, 0x28, 0x28, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	second := raw(0x3a, 0x3a, 0x3a, 0x3a, 0x3a, 0x3a, 0x1e, 0x1e, 0x1e, 0x1e, 0x1e, 0x1e, 0x1e, 0x1e, 0x1e, 0x1e)
	bmc := &fakeBMC{reads: [][]byte{first, second}, failSets: 1}
	f := newTestFanController(bmc)

	if _, err := f.SetDuty(uniformDuty(100)); err == nil {
		t.Fatal("expected first write to fail")
	}
	if !bytes.Equal(f.last, second) {
		t.Fatalf("prev not re-seeded after failed write: % x", f.last)
	}

	if _, err := f.SetDuty(uniformDuty(100)); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(bmc.sets[0], bmc.sets[1]) {
		t.Fatalf("payload did not change after re-seed: % x", bmc.sets[1])
	}
	if got := bmc.sets[1][0]; got != oemDutyRawMax {
		t.Fatalf("expected ramp from re-read 0x3a to 0x3f, got 0x%02x", got)
	}
}
