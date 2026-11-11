package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestComputeDutiesReportsDriver(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FanCount = 2
	sensors := []*Sensor{
		{Config: SensorConfig{Name: "CPU Temp"}},
		{Config: SensorConfig{Name: "X570 Temp"}},
	}
	fans := []FanConfig{
		{Name: "FAN1", Index: 0},
		{Name: "FAN2", Index: 1, Sensors: []string{"cpu temp"}},
	}
	readings := map[string]sensorReading{
		"CPU Temp":  {Temp: 45, Target: 50, Out: 30},
		"X570 Temp": {Temp: 41, Target: 35, Out: 70},
	}

	duty, drivers := computeDuties(cfg, sensors, fans, readings)
	if duty[0] != 70 || duty[1] != 30 {
		t.Fatalf("duty = %v", duty)
	}
	if want := (zoneDriver{"X570 Temp", 41, 35}); drivers[0] != want {
		t.Errorf("FAN1 driver = %+v, want %+v", drivers[0], want)
	}
	if want := (zoneDriver{"CPU Temp", 45, 50}); drivers[1] != want {
		t.Errorf("FAN2 driver = %+v, want %+v", drivers[1], want)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("fan duty", driverLogAttrs(fans, drivers)...)
	if got := buf.String(); !strings.Contains(got, `FAN1.driver="X570 Temp" FAN1.temp=41 FAN1.target=35`) {
		t.Errorf("unexpected log line: %s", got)
	}

	buf.Reset()
	logger.Info("fan duty", driverLogAttrs(fans[:1], drivers[:1])...)
	if got := buf.String(); !strings.Contains(got, `driver="X570 Temp" temp=41 target=35`) || strings.Contains(got, "FAN1.") {
		t.Errorf("unexpected log line: %s", got)
	}
}
