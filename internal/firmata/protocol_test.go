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
