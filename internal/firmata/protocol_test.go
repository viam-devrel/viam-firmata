package firmata

import (
	"bufio"
	"bytes"
	"testing"
)

func TestEncode(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		want []byte
	}{
		{
			// Firmata spec: SET_PIN_MODE = 0xF4, pin, mode. Pin 13 -> OUTPUT.
			name: "SetPinMode pin 13 OUTPUT",
			got:  encodePinMode(13, PinModeOutput),
			want: []byte{0xF4, 0x0D, 0x01},
		},
		{
			// Pin 2 -> INPUT_PULLUP (mode 0x0B)
			name: "SetPinMode pin 2 INPUT_PULLUP",
			got:  encodePinMode(2, PinModeInputPullup),
			want: []byte{0xF4, 0x02, 0x0B},
		},
		{
			// DIGITAL_MESSAGE port 0, mask 0x01 (pin 0 high).
			// Byte layout: 0x90|port, lsb7, msb1. mask=0x01 -> lsb=0x01, msb=0x00.
			name: "DigitalPortWrite port 0 mask 0x01",
			got:  encodeDigitalPortWrite(0, 0x01),
			want: []byte{0x90, 0x01, 0x00},
		},
		{
			// DIGITAL_MESSAGE port 1, mask 0x20 (pin 13 high -> port 1, bit 5).
			name: "DigitalPortWrite port 1 mask 0x20",
			got:  encodeDigitalPortWrite(1, 0x20),
			want: []byte{0x91, 0x20, 0x00},
		},
		{
			// Mask with bit 7 set must split across lsb/msb bytes.
			// mask=0x80 -> lsb=0x00, msb=0x01.
			name: "DigitalPortWrite port 0 mask 0x80 (bit 7 split)",
			got:  encodeDigitalPortWrite(0, 0x80),
			want: []byte{0x90, 0x00, 0x01},
		},
		{
			// REPORT_DIGITAL port 0 enable -> 0xD0, 0x01
			name: "EnableDigitalReporting port 0 on",
			got:  encodeReportDigital(0, true),
			want: []byte{0xD0, 0x01},
		},
		{
			name: "EnableDigitalReporting port 1 off",
			got:  encodeReportDigital(1, false),
			want: []byte{0xD1, 0x00},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Equal(tc.got, tc.want) {
				t.Errorf("got % X, want % X", tc.got, tc.want)
			}
		})
	}
}

func TestDecode_Version(t *testing.T) {
	// REPORT_VERSION (0xF9) major=2 minor=5
	r := bufio.NewReader(bytes.NewReader([]byte{0xF9, 0x02, 0x05}))
	msg, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	v, ok := msg.(VersionMessage)
	if !ok {
		t.Fatalf("wanted VersionMessage, got %T", msg)
	}
	if v.Major != 2 || v.Minor != 5 {
		t.Errorf("got %d.%d, want 2.5", v.Major, v.Minor)
	}
}

func TestDecode_DigitalPort(t *testing.T) {
	// DIGITAL_MESSAGE port 1, mask 0x20 -> bytes 0x91, 0x20, 0x00
	r := bufio.NewReader(bytes.NewReader([]byte{0x91, 0x20, 0x00}))
	msg, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d, ok := msg.(DigitalPortMessage)
	if !ok {
		t.Fatalf("wanted DigitalPortMessage, got %T", msg)
	}
	if d.Port != 1 || d.Mask != 0x20 {
		t.Errorf("got port=%d mask=%#x, want port=1 mask=0x20", d.Port, d.Mask)
	}
}

func TestDecode_DigitalPort_Bit7Split(t *testing.T) {
	// mask 0x80 is split: lsb=0x00, msb=0x01.
	r := bufio.NewReader(bytes.NewReader([]byte{0x90, 0x00, 0x01}))
	msg, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d := msg.(DigitalPortMessage)
	if d.Mask != 0x80 {
		t.Errorf("got mask=%#x, want 0x80", d.Mask)
	}
}

func TestDecode_SysexIsSkipped(t *testing.T) {
	// A sysex frame: 0xF0 ... 0xF7, then a real version message.
	r := bufio.NewReader(bytes.NewReader([]byte{0xF0, 0x79, 0x02, 0x05, 0xF7, 0xF9, 0x02, 0x05}))
	// First decode call consumes the sysex as UnknownMessage.
	first, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error decoding sysex: %v", err)
	}
	if _, ok := first.(UnknownMessage); !ok {
		t.Fatalf("wanted UnknownMessage for sysex, got %T", first)
	}
	// Second decode call must find the real version frame.
	second, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error after sysex: %v", err)
	}
	if _, ok := second.(VersionMessage); !ok {
		t.Fatalf("wanted VersionMessage after sysex, got %T", second)
	}
}

func TestDecode_ResyncOnLeadingNoise(t *testing.T) {
	// Stray data bytes (bit 7 clear) followed by a valid version frame.
	// Decoder should discard noise bytes until it finds a command byte.
	r := bufio.NewReader(bytes.NewReader([]byte{0x00, 0x7F, 0x42, 0xF9, 0x02, 0x05}))
	msg, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := msg.(VersionMessage); !ok {
		t.Fatalf("wanted VersionMessage after resync, got %T", msg)
	}
}

func TestDecode_SysexOverflowErrors(t *testing.T) {
	// A sysex frame with no END_SYSEX byte and more than maxSysexPayload
	// payload bytes must fail with an error rather than allocating unboundedly.
	buf := make([]byte, 0, maxSysexPayload+8)
	buf = append(buf, cmdStartSysex)
	for range maxSysexPayload + 1 {
		buf = append(buf, 0x00)
	}
	_, err := decode(bufio.NewReader(bytes.NewReader(buf)))
	if err == nil {
		t.Fatal("expected error for unbounded sysex, got nil")
	}
}

func TestEncodeAnalogWrite(t *testing.T) {
	// ANALOG_MESSAGE for channel 6, value 200:
	//   cmd = 0xE0 | 6 = 0xE6
	//   lsb = 200 & 0x7F = 0x48
	//   msb = (200 >> 7) & 0x7F = 0x01
	got := encodeAnalogWrite(6, 200)
	want := []byte{0xE6, 0x48, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeAnalogWrite(6, 200): got % X, want % X", got, want)
	}
}

func TestEncodeExtendedAnalog(t *testing.T) {
	// EXTENDED_ANALOG for pin 20, value 200:
	//   0xF0 0x6F pin lsb msb 0xF7
	got := encodeExtendedAnalog(20, 200)
	want := []byte{0xF0, 0x6F, 20, 0x48, 0x01, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeExtendedAnalog(20, 200): got % X, want % X", got, want)
	}
}

func TestEncodeExtendedAnalog_HighResolution(t *testing.T) {
	// 14-bit value 0x3FFF should serialize to two 7-bit bytes 0x7F, 0x7F.
	got := encodeExtendedAnalog(20, 0x3FFF)
	want := []byte{0xF0, 0x6F, 20, 0x7F, 0x7F, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeExtendedAnalog(20, 0x3FFF): got % X, want % X", got, want)
	}
}

func TestEncodeReportAnalog(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"channel 0 enable", encodeReportAnalog(0, true), []byte{0xC0, 0x01}},
		{"channel 5 enable", encodeReportAnalog(5, true), []byte{0xC5, 0x01}},
		{"channel 0 disable", encodeReportAnalog(0, false), []byte{0xC0, 0x00}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Equal(tc.got, tc.want) {
				t.Errorf("got % X, want % X", tc.got, tc.want)
			}
		})
	}
}

func TestEncodeSamplingInterval(t *testing.T) {
	// 100ms -> lsb=0x64, msb=0x00. Frame: F0 7A 64 00 F7.
	got := encodeSamplingInterval(100)
	want := []byte{0xF0, 0x7A, 0x64, 0x00, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeSamplingInterval(100): got % X, want % X", got, want)
	}

	// 1000ms -> 0x03E8. lsb=0x68, msb=0x07.
	got = encodeSamplingInterval(1000)
	want = []byte{0xF0, 0x7A, 0x68, 0x07, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeSamplingInterval(1000): got % X, want % X", got, want)
	}
}

func TestEncodeCapabilityQuery(t *testing.T) {
	got := encodeCapabilityQuery()
	want := []byte{0xF0, 0x6B, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestEncodeAnalogMappingQuery(t *testing.T) {
	got := encodeAnalogMappingQuery()
	want := []byte{0xF0, 0x69, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestDecode_AnalogMessage(t *testing.T) {
	// ANALOG_MESSAGE channel 0, value 512: 0xE0 0x00 0x04
	r := bufio.NewReader(bytes.NewReader([]byte{0xE0, 0x00, 0x04}))
	msg, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a, ok := msg.(AnalogMessage)
	if !ok {
		t.Fatalf("wanted AnalogMessage, got %T", msg)
	}
	if a.Channel != 0 || a.Value != 512 {
		t.Errorf("got channel=%d value=%d, want channel=0 value=512", a.Channel, a.Value)
	}
}

func TestDecode_AnalogMessage_HighChannel(t *testing.T) {
	// ANALOG_MESSAGE channel 7, value 1023: 0xE7 0x7F 0x07
	r := bufio.NewReader(bytes.NewReader([]byte{0xE7, 0x7F, 0x07}))
	msg, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a := msg.(AnalogMessage)
	if a.Channel != 7 || a.Value != 1023 {
		t.Errorf("got channel=%d value=%d, want channel=7 value=1023", a.Channel, a.Value)
	}
}

func TestNewMessageTypes_ZeroValues(t *testing.T) {
	var am AnalogMessage
	if am.Channel != 0 || am.Value != 0 {
		t.Errorf("AnalogMessage zero value: %+v", am)
	}

	var cr CapabilityResponse
	if cr.Pins != nil {
		t.Errorf("CapabilityResponse.Pins zero value: %v", cr.Pins)
	}

	var ar AnalogMappingResponse
	if ar.ChannelByPin != nil {
		t.Errorf("AnalogMappingResponse.ChannelByPin zero value: %v", ar.ChannelByPin)
	}

	// Ensure they all satisfy the Message interface (compile-time check via assertion).
	var _ Message = AnalogMessage{}
	var _ Message = CapabilityResponse{}
	var _ Message = AnalogMappingResponse{}
}
