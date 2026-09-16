package main

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/u-root/u-root/pkg/ipmi"
)

// This file implements enough of the IPMI Sensor Data Record (SDR) repository
// and sensor reading commands to enumerate "Full Sensor Record" temperature
// sensors and convert their raw readings to Celsius, equivalent to what
// `ipmitool sdr type Temperature` does -- but talking to /dev/ipmi0 directly
// instead of shelling out.

const (
	netfnStorage ipmi.NetFn = 0x0a
	netfnSensor  ipmi.NetFn = 0x04

	cmdReserveSDR   ipmi.Command = 0x22
	cmdGetSDR       ipmi.Command = 0x23
	cmdGetSensorRdg ipmi.Command = 0x2d

	sdrRecordTypeFullSensor = 0x01
	sensorTypeTemperature   = 0x01
)

// IPMISensor is a discovered IPMI temperature sensor.
type IPMISensor struct {
	Name      string
	SensorNum byte
	m         int32
	b         int32
	bExp      int32
	rExp      int32
	analogFmt byte // 0=unsigned, 1=1's complement, 2=2's complement, 3=non-analog
	linear    byte
}

// IPMISensorReader talks to the local BMC over the OpenIPMI kernel driver.
type IPMISensorReader struct {
	ipmi *ipmi.IPMI
}

func OpenIPMISensorReader(device string) (*IPMISensorReader, error) {
	h, err := ipmi.OpenPath(device)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", device, err)
	}
	return &IPMISensorReader{ipmi: h}, nil
}

func (r *IPMISensorReader) Close() error {
	return r.ipmi.Close()
}

func (r *IPMISensorReader) rawCmd(netfn ipmi.NetFn, cmd ipmi.Command, data []byte) ([]byte, error) {
	param := append([]byte{byte(netfn), byte(cmd)}, data...)
	resp, err := r.ipmi.RawCmd(param)
	if err != nil {
		return nil, err
	}
	// resp[0] is the completion code; RawSendRecv already validated it is OK.
	if len(resp) < 1 {
		return nil, fmt.Errorf("short response")
	}
	return resp[1:], nil
}

func (r *IPMISensorReader) reserveSDR() (uint16, error) {
	data, err := r.rawCmd(netfnStorage, cmdReserveSDR, nil)
	if err != nil {
		return 0, fmt.Errorf("reserve SDR repository: %w", err)
	}
	if len(data) < 2 {
		return 0, fmt.Errorf("reserve SDR repository: short response")
	}
	return binary.LittleEndian.Uint16(data[0:2]), nil
}

// getSDRChunk issues one "Get SDR" command for a partial record read.
func (r *IPMISensorReader) getSDRChunk(reservation, recordID uint16, offset, count byte) (nextID uint16, chunk []byte, err error) {
	req := make([]byte, 6)
	binary.LittleEndian.PutUint16(req[0:2], reservation)
	binary.LittleEndian.PutUint16(req[2:4], recordID)
	req[4] = offset
	req[5] = count

	data, err := r.rawCmd(netfnStorage, cmdGetSDR, req)
	if err != nil {
		return 0, nil, err
	}
	if len(data) < 2 {
		return 0, nil, fmt.Errorf("get SDR: short response")
	}
	nextID = binary.LittleEndian.Uint16(data[0:2])
	chunk = data[2:]
	return nextID, chunk, nil
}

// getSDR reads a full SDR record, handling the header-then-body two-step read.
func (r *IPMISensorReader) getSDR(reservation, recordID uint16) (nextID uint16, record []byte, err error) {
	// First read just the 5-byte record header to learn the record length.
	nextID, header, err := r.getSDRChunk(reservation, recordID, 0, 5)
	if err != nil {
		return 0, nil, err
	}
	if len(header) < 5 {
		return nextID, nil, fmt.Errorf("get SDR: short header")
	}
	recLen := header[4]

	record = make([]byte, 0, 5+int(recLen))
	record = append(record, header...)

	const maxChunk = 20 // conservative, well within KCS transfer limits
	for read := byte(0); read < recLen; {
		want := recLen - read
		if want > maxChunk {
			want = maxChunk
		}
		_, chunk, err := r.getSDRChunk(reservation, recordID, 5+read, want)
		if err != nil {
			return 0, nil, err
		}
		if len(chunk) == 0 {
			break
		}
		record = append(record, chunk...)
		read += byte(len(chunk))
	}

	return nextID, record, nil
}

func sext(v uint32, bits uint) int32 {
	shift := 32 - bits
	return int32(v<<shift) >> shift
}

// parseFullSensorRecord parses an IPMI "Full Sensor Record" (type 0x01).
// See IPMI v2.0 spec, table 43-1.
func parseFullSensorRecord(record []byte) (*IPMISensor, bool) {
	if len(record) < 5 || record[3] != sdrRecordTypeFullSensor {
		return nil, false
	}
	body := record[5:]
	if len(body) < 43 {
		return nil, false
	}

	if body[7] != sensorTypeTemperature {
		return nil, false
	}

	sensorNum := body[2]
	unitsByte1 := body[15]
	linearization := body[18] & 0x7f

	mLow := uint32(body[19])
	mHigh := uint32(body[20]&0xc0) << 2
	m := sext(mLow|mHigh, 10)

	bLow := uint32(body[21])
	bHigh := uint32(body[22]&0xc0) << 2
	b := sext(bLow|bHigh, 10)

	rExpBExp := body[24]
	bExp := sext(uint32(rExpBExp&0x0f), 4)
	rExp := sext(uint32(rExpBExp&0xf0)>>4, 4)

	analogFmt := (unitsByte1 >> 6) & 0x03

	idTypeLen := body[42]
	idType := idTypeLen >> 6
	idLen := int(idTypeLen & 0x3f)
	idBytes := body[43:]
	if idLen > len(idBytes) {
		idLen = len(idBytes)
	}
	idBytes = idBytes[:idLen]

	name := decodeSDRString(idType, idBytes)

	return &IPMISensor{
		Name:      name,
		SensorNum: sensorNum,
		m:         m,
		b:         b,
		bExp:      bExp,
		rExp:      rExp,
		analogFmt: analogFmt,
		linear:    linearization,
	}, true
}

func decodeSDRString(idType byte, raw []byte) string {
	switch idType {
	case 3: // 8-bit ASCII + Latin1
		return strings.TrimRight(string(raw), "\x00 ")
	default:
		// Unicode / BCD-plus / 6-bit packed ASCII are not used by the
		// temperature sensors on the boards this tool targets; fall back to
		// a best-effort raw dump rather than failing the whole sensor.
		return strings.TrimRight(string(raw), "\x00 ")
	}
}

// DiscoverTemperatureSensors walks the SDR repository and returns every
// Full Sensor Record of sensor type "Temperature".
func (r *IPMISensorReader) DiscoverTemperatureSensors() ([]*IPMISensor, error) {
	reservation, err := r.reserveSDR()
	if err != nil {
		return nil, err
	}

	var sensors []*IPMISensor
	recordID := uint16(0)
	for {
		nextID, record, err := r.getSDR(reservation, recordID)
		if err != nil {
			return nil, fmt.Errorf("get SDR record 0x%04x: %w", recordID, err)
		}
		if s, ok := parseFullSensorRecord(record); ok {
			sensors = append(sensors, s)
		}
		if nextID == 0xffff || nextID == recordID {
			break
		}
		recordID = nextID
	}

	return sensors, nil
}

// ReadTemperature issues Get Sensor Reading for this sensor and converts the
// raw byte to Celsius using the SDR's linear conversion formula:
//
//	y = (M*raw + B*10^bExp) * 10^rExp
//
// This assumes a linear sensor, which is always true for the temperature
// sensors exposed by these boards.
func (r *IPMISensorReader) ReadTemperature(s *IPMISensor) (float64, error) {
	data, err := r.rawCmd(netfnSensor, cmdGetSensorRdg, []byte{s.SensorNum})
	if err != nil {
		return 0, err
	}
	if len(data) < 2 {
		return 0, fmt.Errorf("get sensor reading: short response")
	}

	raw := data[0]
	stateFlags := data[1]
	// Bit 5 ("reading/state unavailable") set means the reading is not valid.
	if stateFlags&0x20 != 0 {
		return 0, fmt.Errorf("sensor %s: reading unavailable", s.Name)
	}

	var rawSigned float64
	switch s.analogFmt {
	case 0: // unsigned
		rawSigned = float64(raw)
	case 1: // 1's complement
		if raw&0x80 != 0 {
			rawSigned = -float64(^raw & 0xff)
		} else {
			rawSigned = float64(raw)
		}
	case 2: // 2's complement
		rawSigned = float64(int8(raw))
	default:
		return 0, fmt.Errorf("sensor %s: non-analog sensor, cannot convert", s.Name)
	}

	value := (float64(s.m)*rawSigned + float64(s.b)*pow10(s.bExp)) * pow10(s.rExp)
	return value, nil
}

func pow10(exp int32) float64 {
	if exp == 0 {
		return 1
	}
	result := 1.0
	neg := exp < 0
	if neg {
		exp = -exp
	}
	for i := int32(0); i < exp; i++ {
		result *= 10
	}
	if neg {
		return 1 / result
	}
	return result
}
