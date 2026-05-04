// Package firmata implements a minimal subset of the Firmata 2.x wire protocol
// sufficient to drive digital GPIO pins on an Arduino running ConfigurableFirmata.
// It is pure Go against io.ReadWriteCloser and has no serial-port dependency.
package firmata

import (
	"bufio"
	"fmt"
)

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

// Sysex sub-command bytes. See https://github.com/firmata/protocol/blob/master/protocol.md
const (
	sysexAnalogMappingQuery    uint8 = 0x69
	sysexAnalogMappingResponse uint8 = 0x6A
	sysexCapabilityQuery       uint8 = 0x6B
	sysexCapabilityResponse    uint8 = 0x6C
	sysexPinStateQuery         uint8 = 0x6D
	sysexPinStateResponse      uint8 = 0x6E
	sysexExtendedAnalog        uint8 = 0x6F
	sysexStringData            uint8 = 0x71
	sysexSamplingInterval      uint8 = 0x7A
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

// encodeAnalogWrite emits an ANALOG_MESSAGE for analog channels 0..15.
// Value is masked to 14 bits and split across two 7-bit data bytes.
func encodeAnalogWrite(channel uint8, value uint16) []byte {
	value &= 0x3FFF
	return []byte{
		cmdAnalogMessage | (channel & 0x0F),
		uint8(value & 0x7F),
		uint8((value >> 7) & 0x7F),
	}
}

// encodeExtendedAnalog emits a sysex EXTENDED_ANALOG for pins/channels >15.
// Value is masked to 14 bits and split across two 7-bit data bytes.
func encodeExtendedAnalog(pin uint8, value uint16) []byte {
	value &= 0x3FFF
	return []byte{
		cmdStartSysex,
		sysexExtendedAnalog,
		pin & 0x7F,
		uint8(value & 0x7F),
		uint8((value >> 7) & 0x7F),
		cmdEndSysex,
	}
}

func encodeReportDigital(port uint8, enable bool) []byte {
	var b uint8
	if enable {
		b = 1
	}
	return []byte{cmdReportDigital | (port & 0x0F), b}
}

func encodeReportAnalog(channel uint8, enable bool) []byte {
	var b uint8
	if enable {
		b = 1
	}
	return []byte{cmdReportAnalog | (channel & 0x0F), b}
}

// encodeSamplingInterval emits a SAMPLING_INTERVAL sysex. Caller is
// responsible for ensuring intervalMs fits in 14 bits (1..16383).
func encodeSamplingInterval(intervalMs uint16) []byte {
	intervalMs &= 0x3FFF
	return []byte{
		cmdStartSysex,
		sysexSamplingInterval,
		uint8(intervalMs & 0x7F),
		uint8((intervalMs >> 7) & 0x7F),
		cmdEndSysex,
	}
}

func encodeCapabilityQuery() []byte {
	return []byte{cmdStartSysex, sysexCapabilityQuery, cmdEndSysex}
}

func encodeAnalogMappingQuery() []byte {
	return []byte{cmdStartSysex, sysexAnalogMappingQuery, cmdEndSysex}
}

func encodePinStateQuery(pin uint8) []byte {
	return []byte{cmdStartSysex, sysexPinStateQuery, pin & 0x7F, cmdEndSysex}
}

// Message is a decoded Firmata frame. Concrete types below.
type Message interface{ isMessage() }

type VersionMessage struct {
	Major, Minor uint8
}

func (VersionMessage) isMessage() {}

type DigitalPortMessage struct {
	Port uint8 // 0..15
	Mask uint8 // bit n = pin (port*8 + n)
}

func (DigitalPortMessage) isMessage() {}

// AnalogMessage carries one ADC sample for a single analog channel.
// Value is 14-bit on the wire (firmware splits into two 7-bit bytes); real
// AVR boards send 10-bit samples (0..1023).
type AnalogMessage struct {
	Channel uint8
	Value   uint16
}

func (AnalogMessage) isMessage() {}

// PinCapabilities maps each supported PinMode to its resolution in bits.
// A pin reports zero or more (mode, resolution) pairs in a CAPABILITY_RESPONSE.
type PinCapabilities map[PinMode]uint8

// CapabilityResponse is the decoded payload of a CAPABILITY_RESPONSE sysex.
// Pins is indexed by digital pin number; entries for pins absent from the
// firmware response are nil.
type CapabilityResponse struct {
	Pins []PinCapabilities
}

func (CapabilityResponse) isMessage() {}

// AnalogMappingResponse is the decoded payload of an ANALOG_MAPPING_RESPONSE
// sysex. ChannelByPin[digitalPin] = analog channel, or 127 (0x7F) if the pin
// is not analog-capable.
type AnalogMappingResponse struct {
	ChannelByPin []uint8
}

func (AnalogMappingResponse) isMessage() {}

// PinStateResponse is the decoded payload of a PIN_STATE_RESPONSE sysex —
// the firmware's current view of one pin's mode and state. State width is
// >= 7 bits, so the bytes are 7-bit packed lsb-first.
type PinStateResponse struct {
	Pin   uint8
	Mode  PinMode
	State uint32
}

func (PinStateResponse) isMessage() {}

// StringDataMessage is the decoded payload of a STRING_DATA sysex (0x71).
// ConfigurableFirmata uses this for diagnostic / error strings: each char is
// sent as two 7-bit bytes (lsb, msb).
type StringDataMessage struct {
	Text string
}

func (StringDataMessage) isMessage() {}

// UnknownMessage is returned for any command we don't explicitly handle,
// including sysex. Callers can ignore it.
type UnknownMessage struct {
	Cmd     uint8
	Payload []byte
}

func (UnknownMessage) isMessage() {}

// decode reads one complete Firmata frame. It resyncs on leading data bytes
// (bit 7 clear) by discarding them until a command byte appears.
func decode(r *bufio.Reader) (Message, error) {
	// Resync: skip bytes until we see one with bit 7 set.
	var cmd byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b&0x80 != 0 {
			cmd = b
			break
		}
	}

	switch {
	case cmd == cmdReportVersion:
		major, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read version major: %w", err)
		}
		minor, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read version minor: %w", err)
		}
		return VersionMessage{Major: major, Minor: minor}, nil

	case cmd&0xF0 == cmdDigitalMessage:
		lsb, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read digital lsb: %w", err)
		}
		msb, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read digital msb: %w", err)
		}
		mask := (lsb & 0x7F) | ((msb & 0x01) << 7)
		return DigitalPortMessage{Port: cmd & 0x0F, Mask: mask}, nil

	case cmd&0xF0 == cmdAnalogMessage:
		lsb, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read analog lsb: %w", err)
		}
		msb, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read analog msb: %w", err)
		}
		return AnalogMessage{
			Channel: cmd & 0x0F,
			Value:   uint16(lsb&0x7F) | (uint16(msb&0x7F) << 7),
		}, nil

	case cmd == cmdStartSysex:
		payload, err := readUntilEndSysex(r)
		if err != nil {
			return nil, fmt.Errorf("read sysex: %w", err)
		}
		if len(payload) == 0 {
			return UnknownMessage{Cmd: cmdStartSysex, Payload: payload}, nil
		}
		switch payload[0] {
		case sysexCapabilityResponse:
			return decodeCapabilityResponse(payload[1:]), nil
		case sysexAnalogMappingResponse:
			return decodeAnalogMappingResponse(payload[1:]), nil
		case sysexPinStateResponse:
			return decodePinStateResponse(payload[1:]), nil
		case sysexStringData:
			return decodeStringData(payload[1:]), nil
		default:
			return UnknownMessage{Cmd: cmdStartSysex, Payload: payload}, nil
		}

	default:
		// Any other command byte in the 0x80..0xEF range is a 3-byte message
		// per the Firmata spec (ANALOG_MESSAGE, etc.) — consume the 2 data bytes.
		// Commands 0xF1..0xFF other than the ones we handle above (e.g. 0xFE
		// PIN_STATE_RESPONSE outside of sysex, or 0xF8 which is a system-command
		// reset in some firmwares) technically have varying lengths; the PoC
		// assumes 2 data bytes since ConfigurableFirmata does not emit those
		// unsolicited during digital-only GPIO traffic. If we ever add analog
		// or sysex feature traffic, replace this with a command-length table.
		b1, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read unknown data1: %w", err)
		}
		b2, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read unknown data2: %w", err)
		}
		return UnknownMessage{Cmd: cmd, Payload: []byte{b1, b2}}, nil
	}
}

// maxSysexPayload bounds sysex reads so a firmware that never sends END_SYSEX
// (or a bit-stream that looks like an endless sysex frame) can't exhaust memory.
// 4096 bytes is well above any realistic ConfigurableFirmata sysex payload.
const maxSysexPayload = 4096

func readUntilEndSysex(r *bufio.Reader) ([]byte, error) {
	out := make([]byte, 0, 32)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == cmdEndSysex {
			return out, nil
		}
		if len(out) >= maxSysexPayload {
			return nil, fmt.Errorf("sysex payload exceeds %d bytes without END_SYSEX", maxSysexPayload)
		}
		out = append(out, b)
	}
}

// decodeCapabilityResponse parses a CAPABILITY_RESPONSE payload (already
// stripped of leading 0x6C and trailing 0xF7). Each pin contributes zero or
// more (mode, resolution) pairs followed by a 0x7F terminator.
func decodeCapabilityResponse(p []byte) CapabilityResponse {
	pins := []PinCapabilities{}
	current := PinCapabilities{}
	for i := 0; i < len(p); i++ {
		if p[i] == 0x7F {
			pins = append(pins, current)
			current = PinCapabilities{}
			continue
		}
		// Need at least two bytes for a (mode, resolution) pair.
		if i+1 >= len(p) {
			break
		}
		current[PinMode(p[i])] = p[i+1]
		i++
	}
	return CapabilityResponse{Pins: pins}
}

// decodePinStateResponse parses a PIN_STATE_RESPONSE payload (already stripped
// of the leading 0x6E sub-command and trailing 0xF7). Layout: pin, mode, then
// 0..N 7-bit state bytes (lsb first).
func decodePinStateResponse(p []byte) PinStateResponse {
	if len(p) < 2 {
		return PinStateResponse{}
	}
	resp := PinStateResponse{Pin: p[0], Mode: PinMode(p[1])}
	var state uint32
	for i, b := range p[2:] {
		state |= uint32(b&0x7F) << (7 * uint(i))
	}
	resp.State = state
	return resp
}

// decodeStringData parses a STRING_DATA payload (already stripped of the
// leading 0x71 sub-command and trailing 0xF7). Each character is two 7-bit
// bytes (lsb, msb); we collapse pairs and strip the trailing NUL the
// firmware tends to append.
func decodeStringData(p []byte) StringDataMessage {
	out := make([]byte, 0, len(p)/2)
	for i := 0; i+1 < len(p); i += 2 {
		ch := byte((p[i] & 0x7F) | ((p[i+1] & 0x01) << 7))
		if ch == 0 {
			break
		}
		out = append(out, ch)
	}
	return StringDataMessage{Text: string(out)}
}

// decodeAnalogMappingResponse parses an ANALOG_MAPPING_RESPONSE payload
// (already stripped of leading 0x6A and trailing 0xF7). Each byte is the
// analog channel for the matching digital pin, or 0x7F if not analog.
func decodeAnalogMappingResponse(p []byte) AnalogMappingResponse {
	out := make([]uint8, len(p))
	copy(out, p)
	return AnalogMappingResponse{ChannelByPin: out}
}
