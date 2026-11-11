// Command asrockrack-ipmi-fan-control runs a PID-based fan speed controller
// for ASRock Rack boards (e.g. X570D4U) using IPMI temperature sensors, NVMe
// SMART temperature, and disk SMART temperature (attribute #194), and an OEM
// raw IPMI command to set fan duty cycles.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// Sensor is one temperature source with its own PID loop.
type Sensor struct {
	Config SensorConfig
	PID    *PID
	Read   func() (float64, error)
}

func buildSensors(cfg *Config, ipmiReader *IPMISensorReader) ([]*Sensor, error) {
	byIPMIName := map[string]*SensorConfig{}
	byDevice := map[string]*SensorConfig{}
	for i := range cfg.Sensors {
		sc := &cfg.Sensors[i]
		switch sc.Type {
		case "nvme", "disk":
			byDevice[sc.Device] = sc
		default:
			byIPMIName[strings.ToUpper(sc.Name)] = sc
		}
	}

	var sensors []*Sensor

	discovered, err := ipmiReader.DiscoverTemperatureSensors()
	if err != nil {
		return nil, fmt.Errorf("discovering IPMI temperature sensors: %w", err)
	}
	for _, s := range discovered {
		sc := SensorConfig{Name: s.Name, Type: "ipmi"}
		if existing, ok := byIPMIName[strings.ToUpper(s.Name)]; ok {
			sc = *existing
			sc.Name, sc.Type = s.Name, "ipmi"
		}
		sc.applyDefaults()

		ipmiSensor := s
		sensors = append(sensors, &Sensor{
			Config: sc,
			PID:    &PID{Kp: sc.Kp, Ki: sc.Ki, Kd: sc.Kd, OutMin: 0, OutMax: 100},
			Read:   func() (float64, error) { return ipmiReader.ReadTemperature(ipmiSensor) },
		})
	}

	nvmeDevices := cfg.NVMeDevices
	if len(nvmeDevices) == 0 && cfg.autoDiscoverStorage() {
		nvmeDevices = DiscoverNVMeDevices()
	}
	for _, dev := range nvmeDevices {
		sc := SensorConfig{Name: dev, Type: "nvme", Device: dev}
		if existing, ok := byDevice[dev]; ok {
			sc = *existing
			sc.Type, sc.Device = "nvme", dev
			if sc.Name == "" {
				sc.Name = dev
			}
		}
		sc.applyDefaults()

		ss := &StorageSensor{Device: dev}
		sensors = append(sensors, &Sensor{
			Config: sc,
			PID:    &PID{Kp: sc.Kp, Ki: sc.Ki, Kd: sc.Kd, OutMin: 0, OutMax: 100},
			Read:   ss.ReadTemperature,
		})
	}

	diskDevices := cfg.DiskDevices
	if len(diskDevices) == 0 && cfg.autoDiscoverStorage() {
		diskDevices = DiscoverDiskDevices()
	}
	for _, dev := range diskDevices {
		sc := SensorConfig{Name: dev, Type: "disk", Device: dev}
		if existing, ok := byDevice[dev]; ok {
			sc = *existing
			sc.Type, sc.Device = "disk", dev
			if sc.Name == "" {
				sc.Name = dev
			}
		}
		sc.applyDefaults()

		ss := &StorageSensor{Device: dev}
		sensors = append(sensors, &Sensor{
			Config: sc,
			PID:    &PID{Kp: sc.Kp, Ki: sc.Ki, Kd: sc.Kd, OutMin: 0, OutMax: 100},
			Read:   ss.ReadTemperature,
		})
	}

	return sensors, nil
}

// defaultFanNames labels the X570D4U's real fan zones, in the order the OEM
// raw fan duty command expects them.
var defaultFanNames = [oemFanZoneCount]string{
	"FAN1", "FAN2", "FAN3", "FAN4_1", "FAN5_1", "FAN6_1",
}

func defaultFans(cfg *Config) []FanConfig {
	fans := make([]FanConfig, cfg.FanCount)
	for i := range fans {
		name := fmt.Sprintf("Fan%d", i)
		if i < len(defaultFanNames) {
			name = defaultFanNames[i]
		}
		fans[i] = FanConfig{Name: name, Index: i}
	}
	return fans
}

func containsFold(list []string, name string) bool {
	for _, l := range list {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// maxConsecutiveWriteFailures is how many control loop ticks in a row may
// fail to set the fan duty before the daemon gives up and exits non-zero,
// so systemd restarts it with fresh BMC state (about a minute at the
// default 5s interval).
const maxConsecutiveWriteFailures = 12

// sensorReading is one sensor's temperature and PID output for a tick.
type sensorReading struct {
	Temp, Target, Out float64
}

// zoneDriver identifies the sensor that determined a fan zone's duty. Sensor
// is empty if none of the zone's sensors could be read.
type zoneDriver struct {
	Sensor       string
	Temp, Target float64
}

func (d zoneDriver) attrs() []any {
	if d.Sensor == "" {
		return []any{"driver", "none"}
	}
	return []any{"driver", d.Sensor, "temp", d.Temp, "target", d.Target}
}

// computeDuties maps per-sensor PID outputs to fan zone duties: each zone
// takes the maximum output of the sensors controlling it. It also returns,
// per entry in fans, which sensor determined that maximum.
func computeDuties(cfg *Config, sensors []*Sensor, fans []FanConfig, readings map[string]sensorReading) ([]int, []zoneDriver) {
	duty := make([]int, cfg.FanCount)
	drivers := make([]zoneDriver, len(fans))
	if len(readings) == 0 {
		for i := range duty {
			duty[i] = 100
		}
		return duty, drivers
	}

	for fi, fan := range fans {
		maxOut := -1.0
		found := false
		for _, s := range sensors {
			if len(fan.Sensors) > 0 && !containsFold(fan.Sensors, s.Config.Name) {
				continue
			}
			if r, ok := readings[s.Config.Name]; ok {
				found = true
				if r.Out > maxOut {
					maxOut = r.Out
					drivers[fi] = zoneDriver{Sensor: s.Config.Name, Temp: r.Temp, Target: r.Target}
				}
			}
		}

		d := 100 // safety default if this fan's controlling sensors are all unreadable
		if found {
			d = int(math.Round(maxOut))
		}
		d = clamp(d, cfg.MinDuty, cfg.MaxDuty)

		if fan.Index >= 0 && fan.Index < len(duty) {
			duty[fan.Index] = d
		}
	}
	return duty, drivers
}

// driverLogAttrs returns log attributes naming the sensor(s) that determined
// the fan duty: flat driver/temp/target if every zone has the same driver
// (the common case), otherwise one group per fan zone.
func driverLogAttrs(fans []FanConfig, drivers []zoneDriver) []any {
	if len(drivers) == 0 {
		return nil
	}
	same := true
	for _, d := range drivers[1:] {
		if d != drivers[0] {
			same = false
			break
		}
	}
	if same {
		return drivers[0].attrs()
	}
	attrs := make([]any, 0, len(fans))
	for i, fan := range fans {
		attrs = append(attrs, slog.Group(fan.Name, drivers[i].attrs()...))
	}
	return attrs
}

// tick runs one read-sensors/compute-PID/apply-fans iteration. It returns
// the error from setting the fan duty, if any.
func tick(cfg *Config, sensors []*Sensor, fans []FanConfig, fanCtl *FanController, dryRun bool, logger *slog.Logger) error {
	now := time.Now()
	readings := make(map[string]sensorReading, len(sensors))

	for _, s := range sensors {
		temp, err := s.Read()
		if err != nil {
			logger.Warn("sensor read failed", "sensor", s.Config.Name, "type", s.Config.Type, "err", err)
			continue
		}

		out := s.PID.Update(s.Config.Target, temp, now)
		if temp >= s.Config.Max {
			logger.Warn("sensor at or above max temperature, forcing full speed",
				"sensor", s.Config.Name, "temp", temp, "max", s.Config.Max)
			out = 100
			s.PID.Reset()
		}
		readings[s.Config.Name] = sensorReading{Temp: temp, Target: s.Config.Target, Out: out}
		logger.Debug("sensor reading", "sensor", s.Config.Name, "temp", temp, "target", s.Config.Target, "duty", out)
	}

	if len(readings) == 0 {
		logger.Error("no sensors could be read, forcing all fans to safety duty")
	}
	duty, drivers := computeDuties(cfg, sensors, fans, readings)

	attrs := append([]any{"duty", duty}, driverLogAttrs(fans, drivers)...)
	logger.Info("fan duty", append(attrs, "dry_run", dryRun)...)
	if dryRun {
		return nil
	}
	if _, err := fanCtl.SetDuty(duty); err != nil {
		logger.Error("setting fan duty failed", "err", err)
		return err
	}
	return nil
}

func run(cfg *Config, sensors []*Sensor, fanCtl *FanController, dryRun, once bool, logger *slog.Logger) error {
	fans := cfg.Fans
	if len(fans) == 0 {
		fans = defaultFans(cfg)
	}

	failures := 0
	step := func() error {
		if err := tick(cfg, sensors, fans, fanCtl, dryRun, logger); err != nil {
			failures++
			if failures >= maxConsecutiveWriteFailures {
				return fmt.Errorf("setting fan duty failed %d times in a row, giving up so the service manager can restart with fresh BMC state: %w", failures, err)
			}
			return nil
		}
		failures = 0
		return nil
	}

	if err := step(); err != nil {
		return err
	}
	if once {
		if failures > 0 {
			return fmt.Errorf("setting fan duty failed")
		}
		return nil
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := step(); err != nil {
				return err
			}
		case sig := <-sigCh:
			logger.Info("received signal, shutting down", "signal", sig.String())
			if cfg.SafeShutdownDuty > 0 && !dryRun {
				safe := make([]int, cfg.FanCount)
				for i := range safe {
					safe[i] = cfg.SafeShutdownDuty
				}
				if err := fanCtl.RampToBlocking(safe, 30*time.Second, time.Second); err != nil {
					logger.Error("failed to set safe shutdown fan duty", "err", err)
				}
			}
			return nil
		}
	}
}

func main() {
	var (
		configPath   string
		dryRun       bool
		once         bool
		verbose      bool
		printVersion bool
	)
	flag.StringVar(&configPath, "config", "", "path to YAML config file (optional; built-in defaults are used otherwise)")
	flag.BoolVar(&dryRun, "dry-run", false, "compute fan duty cycles but do not send IPMI commands")
	flag.BoolVar(&once, "once", false, "run a single control loop iteration and exit")
	flag.BoolVar(&verbose, "verbose", false, "enable debug logging")
	flag.BoolVar(&printVersion, "version", false, "print version and exit")
	flag.Parse()

	if printVersion {
		fmt.Println(version)
		return
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := LoadConfig(configPath)
	if err != nil {
		logger.Error("loading config", "err", err)
		os.Exit(1)
	}
	if cfg.FanCount > oemFanZoneCount {
		logger.Warn("fan_count exceeds the number of real fan zones this board exposes, extra fans will be ignored",
			"fan_count", cfg.FanCount, "max", oemFanZoneCount)
	}

	ipmiReader, err := OpenIPMISensorReader(cfg.IPMIDevice)
	if err != nil {
		logger.Error("opening IPMI device", "device", cfg.IPMIDevice, "err", err)
		os.Exit(1)
	}
	defer ipmiReader.Close()

	sensors, err := buildSensors(cfg, ipmiReader)
	if err != nil {
		logger.Error("discovering sensors", "err", err)
		os.Exit(1)
	}
	if len(sensors) == 0 {
		logger.Error("no temperature sensors found")
		os.Exit(1)
	}
	for _, s := range sensors {
		logger.Info("sensor configured",
			"name", s.Config.Name, "type", s.Config.Type,
			"target", s.Config.Target, "max", s.Config.Max,
			"kp", s.Config.Kp, "ki", s.Config.Ki, "kd", s.Config.Kd)
	}

	fanCtl := NewFanController(ipmiReader.ipmi, cfg.FanCount, logger)
	if !dryRun {
		if err := fanCtl.EnableManualControl(); err != nil {
			logger.Error("enabling manual fan control", "err", err)
			os.Exit(1)
		}
	}

	if err := run(cfg, sensors, fanCtl, dryRun, once, logger); err != nil {
		logger.Error("exiting", "err", err)
		ipmiReader.Close()
		os.Exit(1)
	}
}
