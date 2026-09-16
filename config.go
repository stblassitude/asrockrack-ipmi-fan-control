package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration file structure.
type Config struct {
	// IPMIDevice is the OpenIPMI character device used both for reading the
	// SDR temperature sensors and for issuing the OEM raw fan control command.
	IPMIDevice string `yaml:"ipmi_device"`

	// Interval is how often the control loop re-reads sensors and updates fan duty.
	Interval time.Duration `yaml:"interval"`

	// MinDuty/MaxDuty clamp the duty cycle percentage sent to the fans.
	MinDuty int `yaml:"min_duty"`
	MaxDuty int `yaml:"max_duty"`

	// FanCount is the number of real, independently controllable fan zones
	// on this board (FAN1, FAN2, FAN3, FAN4_1, FAN5_1, FAN6_1 on the
	// X570D4U). The OEM raw command's fixed-width payload is padded out
	// beyond this internally.
	FanCount int `yaml:"fan_count"`

	// NVMeDevices and DiskDevices list the block devices to poll for SMART
	// temperature. If left empty, they are auto-discovered.
	NVMeDevices []string `yaml:"nvme_devices"`
	DiskDevices []string `yaml:"disk_devices"`

	// AutoDiscoverStorage controls whether NVMe/disk devices are probed
	// automatically when the corresponding *Devices list above is empty.
	AutoDiscoverStorage *bool `yaml:"auto_discover_storage"`

	Sensors []SensorConfig `yaml:"sensors"`
	Fans    []FanConfig    `yaml:"fans"`

	// SafeShutdownDuty is the duty percentage applied to all fans when the
	// daemon exits, so a stopped controller doesn't leave fans at a low,
	// silent speed. Set to 0 to disable and leave fans at their last value.
	SafeShutdownDuty int `yaml:"safe_shutdown_duty"`
}

// SensorConfig configures a single temperature sensor and its PID loop.
type SensorConfig struct {
	// Name identifies the sensor.
	//   - For IPMI sensors, this must match (case-insensitively) the SDR name,
	//     e.g. "CPU Temp".
	//   - For NVMe/disk sensors, this is set automatically to the device path
	//     when auto-discovered.
	Name string `yaml:"name"`

	// Type is "ipmi", "nvme", or "disk". If empty, it is inferred: IPMI
	// sensors are matched by name against the SDR repository, others must
	// set this explicitly alongside Device.
	Type string `yaml:"type"`

	// Device is the block device path for nvme/disk sensors, e.g. "/dev/nvme0".
	Device string `yaml:"device"`

	Target float64 `yaml:"target"`
	Max    float64 `yaml:"max"`

	Kp float64 `yaml:"kp"`
	Ki float64 `yaml:"ki"`
	Kd float64 `yaml:"kd"`
}

// FanConfig configures a single fan zone.
type FanConfig struct {
	// Name is a human-readable label used only for logging.
	Name string `yaml:"name"`

	// Index is the 0-based position of this fan in the raw fan control
	// command's duty-cycle byte array.
	Index int `yaml:"index"`

	// Sensors restricts which sensors (by SensorConfig.Name) drive this fan.
	// If empty, every configured sensor's PID loop is considered and the
	// one demanding the most cooling wins.
	Sensors []string `yaml:"sensors"`
}

const (
	defaultGeneralTarget = 35.0
	defaultGeneralMax    = 70.0
	defaultCPUTarget     = 50.0
	defaultCPUMax        = 90.0

	defaultGeneralKp = 4.0
	defaultGeneralKi = 0.03
	defaultGeneralKd = 0.5

	defaultCPUKp = 3.0
	defaultCPUKi = 0.03
	defaultCPUKd = 0.3
)

// DefaultConfig returns the built-in defaults described in the README.
func DefaultConfig() *Config {
	return &Config{
		IPMIDevice:       "/dev/ipmi0",
		Interval:         5 * time.Second,
		MinDuty:          20,
		MaxDuty:          100,
		FanCount:         oemFanZoneCount,
		SafeShutdownDuty: 100,
	}
}

// LoadConfig reads and parses a YAML config file, if present, on top of the
// built-in defaults. An empty path means "use defaults only".
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	return cfg, nil
}

// IsCPUSensor reports whether a sensor name looks like a CPU sensor, used to
// pick between the "general" and "CPU" default target/max/PID values.
func IsCPUSensor(name string) bool {
	return strings.Contains(strings.ToUpper(name), "CPU")
}

// applyDefaults fills in target/max/PID gains left unset (zero) in the config
// with the sensible defaults from the README, based on whether the sensor
// looks like a CPU sensor.
func (s *SensorConfig) applyDefaults() {
	cpu := IsCPUSensor(s.Name)

	if s.Target == 0 {
		if cpu {
			s.Target = defaultCPUTarget
		} else {
			s.Target = defaultGeneralTarget
		}
	}
	if s.Max == 0 {
		if cpu {
			s.Max = defaultCPUMax
		} else {
			s.Max = defaultGeneralMax
		}
	}
	if s.Kp == 0 && s.Ki == 0 && s.Kd == 0 {
		if cpu {
			s.Kp, s.Ki, s.Kd = defaultCPUKp, defaultCPUKi, defaultCPUKd
		} else {
			s.Kp, s.Ki, s.Kd = defaultGeneralKp, defaultGeneralKi, defaultGeneralKd
		}
	}
}

func (c *Config) autoDiscoverStorage() bool {
	if c.AutoDiscoverStorage == nil {
		return true
	}
	return *c.AutoDiscoverStorage
}
