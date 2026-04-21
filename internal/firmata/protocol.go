// Package firmata implements a minimal subset of the Firmata 2.x wire protocol
// sufficient to drive digital GPIO pins on an Arduino running ConfigurableFirmata.
// It is pure Go against io.ReadWriteCloser and has no serial-port dependency.
package firmata

// Command bytes (bit 7 set). See https://github.com/firmata/protocol/blob/master/protocol.md
const (
	cmdDigitalMessage uint8 = 0x90 // low nibble = port index
	cmdReportAnalog   uint8 = 0xC0
	cmdReportDigital  uint8 = 0xD0
	cmdAnalogMessage  uint8 = 0xE0
	cmdSetPinMode     uint8 = 0xF4
	cmdStartSysex     uint8 = 0xF0
	cmdEndSysex       uint8 = 0xF7
	cmdReportVersion  uint8 = 0xF9
)

// PinMode values per the Firmata spec.
type PinMode uint8

const (
	PinModeInput       PinMode = 0x00
	PinModeOutput      PinMode = 0x01
	PinModeAnalog      PinMode = 0x02
	PinModePWM         PinMode = 0x03
	PinModeServo       PinMode = 0x04
	PinModeShift       PinMode = 0x05
	PinModeI2C         PinMode = 0x06
	PinModeOneWire     PinMode = 0x07
	PinModeStepper     PinMode = 0x08
	PinModeEncoder     PinMode = 0x09
	PinModeSerial      PinMode = 0x0A
	PinModeInputPullup PinMode = 0x0B
)

func encodePinMode(pin uint8, mode PinMode) []byte {
	return []byte{cmdSetPinMode, pin, uint8(mode)}
}

// encodeDigitalPortWrite emits a DIGITAL_MESSAGE for an 8-bit port mask.
// The mask bit 7 is split into the high data byte per the Firmata 7-bit data encoding.
func encodeDigitalPortWrite(port, mask uint8) []byte {
	return []byte{cmdDigitalMessage | (port & 0x0F), mask & 0x7F, (mask >> 7) & 0x01}
}

func encodeReportDigital(port uint8, enable bool) []byte {
	var b uint8
	if enable {
		b = 1
	}
	return []byte{cmdReportDigital | (port & 0x0F), b}
}
