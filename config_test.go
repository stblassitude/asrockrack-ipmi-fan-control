package main

import "testing"

func TestSensorDefaultClasses(t *testing.T) {
	tests := []struct {
		name        string
		target, max float64
	}{
		{"CPU Temp", defaultCPUTarget, defaultCPUMax},
		{"X570 Temp", defaultChipsetTarget, defaultChipsetMax},
		{"b550 temp", defaultChipsetTarget, defaultChipsetMax},
		{"PCH Temp", defaultChipsetTarget, defaultChipsetMax},
		{"Chipset", defaultChipsetTarget, defaultChipsetMax},
		{"MB Temp", defaultGeneralTarget, defaultGeneralMax},
		{"/dev/nvme0n1", defaultGeneralTarget, defaultGeneralMax},
	}
	for _, tt := range tests {
		sc := SensorConfig{Name: tt.name}
		sc.applyDefaults()
		if sc.Target != tt.target || sc.Max != tt.max {
			t.Errorf("%q: got target %v max %v, want %v/%v", tt.name, sc.Target, sc.Max, tt.target, tt.max)
		}
	}

	sc := SensorConfig{Name: "X570 Temp", Target: 45}
	sc.applyDefaults()
	if sc.Target != 45 || sc.Max != defaultChipsetMax {
		t.Errorf("explicit target overridden: %+v", sc)
	}
}
