package firmata

import (
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
