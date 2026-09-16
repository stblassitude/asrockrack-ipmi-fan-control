package main

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/anatol/smart.go"
)

// StorageSensor reads the temperature of an NVMe or SATA/SAS disk directly
// via the kernel's NVMe admin passthrough / ATA SMART ioctls (through
// github.com/anatol/smart.go), equivalent to what `nvme smart-log` (NVMe
// composite temperature) or `smartctl -a` (ATA attribute #194,
// Temperature_Celsius) report, without shelling out to either tool.
type StorageSensor struct {
	Device string
}

// ReadTemperature opens the device, reads its generic SMART attributes, and
// returns the temperature in Celsius. The device is opened and closed on
// every call since these are polled infrequently and devices can disappear
// (e.g. hot-unplugged NVMe).
func (s *StorageSensor) ReadTemperature() (float64, error) {
	dev, err := smart.Open(s.Device)
	if err != nil {
		return 0, fmt.Errorf("opening %s: %w", s.Device, err)
	}
	defer dev.Close()

	attrs, err := dev.ReadGenericAttributes()
	if err != nil {
		return 0, fmt.Errorf("reading SMART attributes for %s: %w", s.Device, err)
	}
	if attrs.Temperature == 0 {
		return 0, fmt.Errorf("%s: no temperature attribute reported", s.Device)
	}

	return float64(attrs.Temperature), nil
}

// DiscoverNVMeDevices globs for NVMe namespace block devices, e.g. /dev/nvme0n1.
func DiscoverNVMeDevices() []string {
	matches, _ := filepath.Glob("/dev/nvme[0-9]n[0-9]")
	more, _ := filepath.Glob("/dev/nvme[0-9][0-9]n[0-9]")
	matches = append(matches, more...)
	sort.Strings(matches)
	return matches
}

// DiscoverDiskDevices globs for SATA/SAS disk block devices, e.g. /dev/sda.
// NVMe devices are excluded since they are handled by DiscoverNVMeDevices.
func DiscoverDiskDevices() []string {
	matches, _ := filepath.Glob("/dev/sd[a-z]")
	more, _ := filepath.Glob("/dev/sd[a-z][a-z]")
	matches = append(matches, more...)
	sort.Strings(matches)
	return matches
}
