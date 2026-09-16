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

// tick runs one read-sensors/compute-PID/apply-fans iteration.
func tick(cfg *Config, sensors []*Sensor, fans []FanConfig, fanCtl *FanController, dryRun bool, logger *slog.Logger) {
	now := time.Now()
	outputs := make(map[string]float64, len(sensors))

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
		outputs[s.Config.Name] = out
		logger.Debug("sensor reading", "sensor", s.Config.Name, "temp", temp, "target", s.Config.Target, "duty", out)
	}

	duty := make([]int, cfg.FanCount)
	if len(outputs) == 0 {
		logger.Error("no sensors could be read, forcing all fans to safety duty")
		for i := range duty {
			duty[i] = 100
		}
	} else {
		for _, fan := range fans {
			maxOut := -1.0
			found := false
			for _, s := range sensors {
				if len(fan.Sensors) > 0 && !containsFold(fan.Sensors, s.Config.Name) {
					continue
				}
				if v, ok := outputs[s.Config.Name]; ok {
					found = true
					if v > maxOut {
						maxOut = v
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
	}

	logger.Info("fan duty", "duty", duty, "dry_run", dryRun)
	if dryRun {
		return
	}
	if _, err := fanCtl.SetDuty(duty); err != nil {
		logger.Error("setting fan duty failed", "err", err)
	}
}

func run(cfg *Config, sensors []*Sensor, fanCtl *FanController, dryRun, once bool, logger *slog.Logger) {
	fans := cfg.Fans
	if len(fans) == 0 {
		fans = defaultFans(cfg)
	}

	tick(cfg, sensors, fans, fanCtl, dryRun, logger)
	if once {
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			tick(cfg, sensors, fans, fanCtl, dryRun, logger)
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
			return
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

	fanCtl := NewFanController(ipmiReader.ipmi, cfg.FanCount)
	if !dryRun {
		if err := fanCtl.EnableManualControl(); err != nil {
			logger.Error("enabling manual fan control", "err", err)
			os.Exit(1)
		}
	}

	run(cfg, sensors, fanCtl, dryRun, once, logger)
}
