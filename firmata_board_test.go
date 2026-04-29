package firmataboard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/logging"

	"github.com/viam-devrel/viam-firmata/internal/firmata"
)

func TestConfig_Validate(t *testing.T) {
	t.Run("missing serial_path", func(t *testing.T) {
		c := &Config{}
		_, _, err := c.Validate("components.0")
		if err == nil {
			t.Fatal("expected error for missing serial_path, got nil")
		}
	})

	t.Run("negative baud_rate", func(t *testing.T) {
		c := &Config{SerialPath: "/dev/null", BaudRate: -1}
		_, _, err := c.Validate("components.0")
		if err == nil {
			t.Fatal("expected error for negative baud_rate, got nil")
		}
	})

	t.Run("minimal valid config", func(t *testing.T) {
		c := &Config{SerialPath: "/dev/null"}
		req, opt, err := c.Validate("components.0")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(req) != 0 || len(opt) != 0 {
			t.Fatalf("unexpected deps: req=%v opt=%v", req, opt)
		}
	})

	t.Run("fully-populated valid config", func(t *testing.T) {
		c := &Config{
			SerialPath:       "/dev/ttyACM0",
			BaudRate:         57600,
			AutoResetDelay:   2 * time.Second,
			HandshakeTimeout: 5 * time.Second,
		}
		if _, _, err := c.Validate("components.0"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// testBoard wires a firmataBoard to an in-process pair of pipes that stand
// in for an Arduino: everything the board writes ends up in sentBuf; any
// bytes we hand-craft into arduinoW arrive at the board's read loop.
type testBoard struct {
	b        *firmataBoard
	sentBuf  *bytes.Buffer // everything the board has ever written
	arduinoW io.WriteCloser
	cleanup  func()
}

func newTestBoard(t *testing.T) *testBoard {
	t.Helper()
	// board reads ← arduinoR ... arduinoW (test writes here)
	arduinoR, arduinoW := io.Pipe()
	// board writes → sentBuf (test reads here via sentBuf.Bytes())
	sentBuf := &bytes.Buffer{}
	rw := &rwFake{r: arduinoR, w: sentBuf}

	c := firmata.New(rw)
	name := board.Named("test")
	b := newBoardFromClient(name, c, rw, logging.NewTestLogger(t))

	return &testBoard{
		b:        b,
		sentBuf:  sentBuf,
		arduinoW: arduinoW,
		cleanup: func() {
			// Close the arduino-side write half FIRST so the board's pipe
			// reader unblocks with EOF; otherwise b.Close() blocks forever
			// waiting on the read-loop's drain (rwFake.Close is a no-op).
			_ = arduinoW.Close()
			_ = b.Close(context.Background())
		},
	}
}

// rwFake bolts a reader and a writer together. The writer is a *bytes.Buffer
// that tests can inspect; the reader is an io.Pipe fed by the test.
// Note: bytes.Buffer is not safe for concurrent use, but in these tests
// writes come only from the board's writer goroutine and reads happen after
// we've drained the events channel, so there's a happens-before fence.
type rwFake struct {
	r io.Reader
	w io.Writer
}

func (f *rwFake) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f *rwFake) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f *rwFake) Close() error                { return nil }

func TestGPIOPin_Set_EmitsPinModeOnceThenDigitalWrites(t *testing.T) {
	tb := newTestBoard(t)
	defer tb.cleanup()

	ctx := context.Background()
	pin, err := tb.b.GPIOPinByName("13")
	if err != nil {
		t.Fatalf("GPIOPinByName: %v", err)
	}

	if err := pin.Set(ctx, true, nil); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if err := pin.Set(ctx, false, nil); err != nil {
		t.Fatalf("second Set: %v", err)
	}

	// Expected wire bytes:
	//   SET_PIN_MODE(13, OUTPUT)   = 0xF4, 0x0D, 0x01
	//   DIGITAL_MESSAGE port=1,high = 0x91, 0x20, 0x00   (pin 13 is bit 5 of port 1; 1<<5 = 0x20)
	//   DIGITAL_MESSAGE port=1,low  = 0x91, 0x00, 0x00
	want := []byte{
		0xF4, 0x0D, 0x01,
		0x91, 0x20, 0x00,
		0x91, 0x00, 0x00,
	}
	got := tb.sentBuf.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("wire bytes mismatch:\n got = %x\nwant = %x", got, want)
	}
}

func TestGPIOPin_Get_FirstCallEnablesReportingThenReadsCachedState(t *testing.T) {
	tb := newTestBoard(t)
	defer tb.cleanup()

	ctx := context.Background()
	pin, err := tb.b.GPIOPinByName("2")
	if err != nil {
		t.Fatalf("GPIOPinByName: %v", err)
	}

	// First Get: configures pin mode + enables reporting. Cached input state
	// for port 0 is all-zero, so the returned value is false.
	val, err := pin.Get(ctx, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val {
		t.Fatalf("expected false before any frame, got true")
	}

	want := []byte{
		0xF4, 0x02, 0x0B, // SET_PIN_MODE(2, INPUT_PULLUP)
		0xD0, 0x01, // REPORT_DIGITAL(port=0, enable)
	}
	got := tb.sentBuf.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatalf("wire bytes mismatch:\n got = %x\nwant = %x", got, want)
	}

	// Inject a DIGITAL_MESSAGE from the "Arduino" side: port 0, mask 0x04 (pin 2 HIGH).
	if _, err := tb.arduinoW.Write([]byte{0x90, 0x04, 0x00}); err != nil {
		t.Fatalf("inject frame: %v", err)
	}

	// Wait briefly for the reader goroutine + drain goroutine to process.
	// We can't synchronize on Events() here (the drain goroutine owns it), so
	// poll Get for up to 1s.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if v, _ := pin.Get(ctx, nil); v {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	val, err = pin.Get(ctx, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !val {
		t.Fatalf("expected true after DIGITAL_MESSAGE, got false")
	}

	// A second Get must not resend SET_PIN_MODE or REPORT_DIGITAL.
	sentAfterFirstGet := len(want)
	if extra := tb.sentBuf.Len() - sentAfterFirstGet; extra != 0 {
		t.Fatalf("unexpected %d extra bytes on second Get: %x", extra,
			tb.sentBuf.Bytes()[sentAfterFirstGet:])
	}
}

func TestUnimplementedMethods_ReturnSentinelError(t *testing.T) {
	tb := newTestBoard(t)
	defer tb.cleanup()
	ctx := context.Background()

	// GPIOPin PWM family.
	pin, _ := tb.b.GPIOPinByName("5")
	if _, err := pin.PWM(ctx, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("PWM: want errUnimplemented, got %v", err)
	}
	if err := pin.SetPWM(ctx, 0.5, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("SetPWM: want errUnimplemented, got %v", err)
	}
	if _, err := pin.PWMFreq(ctx, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("PWMFreq: want errUnimplemented, got %v", err)
	}
	if err := pin.SetPWMFreq(ctx, 1000, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("SetPWMFreq: want errUnimplemented, got %v", err)
	}

	// Board-level.
	if _, err := tb.b.AnalogByName("a0"); !errors.Is(err, errUnimplemented) {
		t.Errorf("AnalogByName: want errUnimplemented, got %v", err)
	}
	if _, err := tb.b.DigitalInterruptByName("d0"); !errors.Is(err, errUnimplemented) {
		t.Errorf("DigitalInterruptByName: want errUnimplemented, got %v", err)
	}
	if err := tb.b.SetPowerMode(ctx, 0, nil, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("SetPowerMode: want errUnimplemented, got %v", err)
	}
	if err := tb.b.StreamTicks(ctx, nil, nil, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("StreamTicks: want errUnimplemented, got %v", err)
	}
}

func TestGPIOPinByName_RejectsInvalid(t *testing.T) {
	tb := newTestBoard(t)
	defer tb.cleanup()
	for _, name := range []string{"", "abc", "-1", "128", "13.0"} {
		if _, err := tb.b.GPIOPinByName(name); err == nil {
			t.Errorf("GPIOPinByName(%q): want error, got nil", name)
		}
	}
}

func TestConfig_Validate_Analogs(t *testing.T) {
	t.Run("accepts well-formed analogs", func(t *testing.T) {
		c := &Config{
			SerialPath: "/dev/null",
			Analogs: []board.AnalogReaderConfig{
				{Name: "joy_x", Pin: "A0"},
				{Name: "joy_y", Pin: "15"},
			},
		}
		if _, _, err := c.Validate("components.0"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects duplicate analog name", func(t *testing.T) {
		c := &Config{
			SerialPath: "/dev/null",
			Analogs: []board.AnalogReaderConfig{
				{Name: "joy", Pin: "A0"},
				{Name: "joy", Pin: "A1"},
			},
		}
		if _, _, err := c.Validate("components.0"); err == nil {
			t.Fatal("expected error for duplicate analog name")
		}
	})

	t.Run("rejects duplicate analog pin literal", func(t *testing.T) {
		c := &Config{
			SerialPath: "/dev/null",
			Analogs: []board.AnalogReaderConfig{
				{Name: "x", Pin: "A0"},
				{Name: "y", Pin: "A0"},
			},
		}
		if _, _, err := c.Validate("components.0"); err == nil {
			t.Fatal("expected error for duplicate analog pin")
		}
	})

	t.Run("rejects malformed pin string", func(t *testing.T) {
		c := &Config{
			SerialPath: "/dev/null",
			Analogs: []board.AnalogReaderConfig{
				{Name: "bad", Pin: "Z9"},
			},
		}
		if _, _, err := c.Validate("components.0"); err == nil {
			t.Fatal("expected error for malformed pin")
		}
	})

	t.Run("rejects empty pin", func(t *testing.T) {
		c := &Config{
			SerialPath: "/dev/null",
			Analogs: []board.AnalogReaderConfig{
				{Name: "bad", Pin: ""},
			},
		}
		if _, _, err := c.Validate("components.0"); err == nil {
			t.Fatal("expected error for empty pin")
		}
	})

	t.Run("rejects sampling_interval_ms out of range", func(t *testing.T) {
		c := &Config{SerialPath: "/dev/null", SamplingIntervalMs: -1}
		if _, _, err := c.Validate("components.0"); err == nil {
			t.Fatal("expected error for negative sampling_interval_ms")
		}
		c = &Config{SerialPath: "/dev/null", SamplingIntervalMs: 16384}
		if _, _, err := c.Validate("components.0"); err == nil {
			t.Fatal("expected error for sampling_interval_ms > 16383")
		}
	})
}

// unoCaps returns a CapabilityResponse shaped like an Arduino Uno running
// StandardFirmataPlus: 14 digital pins (0..13), of which 3, 5, 6, 9, 10, 11
// support PWM, plus 6 analog pins (14..19) that support analog input. Pin 1
// (RX) and pin 0 (TX) are also INPUT/OUTPUT-capable but kept simple here.
func unoCaps() firmata.CapabilityResponse {
	pins := make([]firmata.PinCapabilities, 20)
	pwm := map[int]bool{3: true, 5: true, 6: true, 9: true, 10: true, 11: true}
	for i := 0; i < 14; i++ {
		caps := firmata.PinCapabilities{
			firmata.PinModeInput:       1,
			firmata.PinModeOutput:      1,
			firmata.PinModeInputPullup: 1,
		}
		if pwm[i] {
			caps[firmata.PinModePWM] = 8
		}
		pins[i] = caps
	}
	for i := 14; i < 20; i++ {
		pins[i] = firmata.PinCapabilities{
			firmata.PinModeInput:       1,
			firmata.PinModeOutput:      1,
			firmata.PinModeInputPullup: 1,
			firmata.PinModeAnalog:      10,
		}
	}
	return firmata.CapabilityResponse{Pins: pins}
}

// unoAnalogMap returns the analog mapping table for an Uno: digital pins
// 0..13 are not analog (0x7F); pins 14..19 map to channels 0..5.
func unoAnalogMap() firmata.AnalogMappingResponse {
	channels := make([]uint8, 20)
	for i := 0; i < 14; i++ {
		channels[i] = 0x7F
	}
	for i := 14; i < 20; i++ {
		channels[i] = uint8(i - 14)
	}
	return firmata.AnalogMappingResponse{ChannelByPin: channels}
}

// newTestBoardWithCaps builds a testBoard with the supplied capability +
// analog-mapping data already injected.
//
// IMPORTANT: until Task 13 lands a real resolveAnalogPin, callers MUST pass
// an empty (or nil) analogs slice. The Task-12 stub of resolveAnalogPin
// always errors, and the helper t.Fatalfs on that error. No test in Task 12
// references this helper with non-empty analogs; tests that need declared
// analogs are introduced in Task 13.
func newTestBoardWithCaps(
	t *testing.T,
	caps firmata.CapabilityResponse,
	amap firmata.AnalogMappingResponse,
	analogs []board.AnalogReaderConfig,
) *testBoard {
	t.Helper()
	tb := newTestBoard(t)
	tb.b.capabilities = caps
	tb.b.analogMap = amap
	for _, a := range analogs {
		dpin, ch, err := parseAnalogPin(a.Pin)
		if err != nil {
			t.Fatalf("parseAnalogPin(%q): %v", a.Pin, err)
		}
		dpin, ch, err = resolveAnalogPin(dpin, ch, amap)
		if err != nil {
			t.Fatalf("resolveAnalogPin(%q): %v", a.Pin, err)
		}
		tb.b.analogs[a.Name] = &firmataAnalog{
			board:      tb.b,
			name:       a.Name,
			digitalPin: dpin,
			channel:    uint8(ch),
		}
		tb.b.ownedPins[dpin] = a.Name
	}
	return tb
}

func TestResolveAnalogPin(t *testing.T) {
	amap := unoAnalogMap()

	t.Run("A0 maps to digital pin 14", func(t *testing.T) {
		dpin, ch, err := resolveAnalogPin(-1, 0, amap)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if dpin != 14 || ch != 0 {
			t.Errorf("got (%d, %d), want (14, 0)", dpin, ch)
		}
	})

	t.Run("digital 14 maps to channel 0", func(t *testing.T) {
		dpin, ch, err := resolveAnalogPin(14, -1, amap)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if dpin != 14 || ch != 0 {
			t.Errorf("got (%d, %d), want (14, 0)", dpin, ch)
		}
	})

	t.Run("non-analog digital pin fails", func(t *testing.T) {
		if _, _, err := resolveAnalogPin(13, -1, amap); err == nil {
			t.Errorf("expected error for non-analog pin")
		}
	})

	t.Run("unknown channel fails", func(t *testing.T) {
		if _, _, err := resolveAnalogPin(-1, 9, amap); err == nil {
			t.Errorf("expected error for unmapped channel")
		}
	})
}

func TestAnalogRead_FirstCallEmitsModeAndReporting(t *testing.T) {
	tb := newTestBoardWithCaps(t, unoCaps(), unoAnalogMap(),
		[]board.AnalogReaderConfig{{Name: "x", Pin: "A0"}})
	defer tb.cleanup()

	a, err := tb.b.AnalogByName("x")
	if err != nil {
		t.Fatalf("AnalogByName: %v", err)
	}

	val, err := a.Read(context.Background(), nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if val.Value != 0 || val.Min != 0 || val.Max != 1023 {
		t.Errorf("first Read = %+v, want value=0 min=0 max=1023", val)
	}

	want := []byte{
		0xF4, 14, 0x02, // SET_PIN_MODE(14, ANALOG)
		0xC0, 0x01, // REPORT_ANALOG(0, on)
	}
	if got := tb.sentBuf.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("wire bytes:\n got = % X\nwant = % X", got, want)
	}

	// Inject ANALOG_MESSAGE channel 0, value 512.
	if _, err := tb.arduinoW.Write([]byte{0xE0, 0x00, 0x04}); err != nil {
		t.Fatalf("inject frame: %v", err)
	}

	// Poll for cached value.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, _ := a.Read(context.Background(), nil)
		if v.Value == 512 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	val, _ = a.Read(context.Background(), nil)
	if val.Value != 512 {
		t.Fatalf("Read after frame = %d, want 512", val.Value)
	}

	// A second Read after enableOnce must not re-emit SET_PIN_MODE/REPORT_ANALOG.
	if got := tb.sentBuf.Len(); got != len(want) {
		t.Errorf("extra bytes after subsequent Read: got %d, want %d", got, len(want))
	}
}

func TestAnalogRead_UnknownName(t *testing.T) {
	tb := newTestBoardWithCaps(t, unoCaps(), unoAnalogMap(), nil)
	defer tb.cleanup()
	if _, err := tb.b.AnalogByName("nope"); err == nil {
		t.Errorf("AnalogByName(nope): want error")
	}
}

func TestAnalogWrite_OnAnalogReaderReturnsUnimplemented(t *testing.T) {
	tb := newTestBoardWithCaps(t, unoCaps(), unoAnalogMap(),
		[]board.AnalogReaderConfig{{Name: "x", Pin: "A0"}})
	defer tb.cleanup()
	a, _ := tb.b.AnalogByName("x")
	if err := a.Write(context.Background(), 100, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("Write: want errUnimplemented, got %v", err)
	}
}

func TestGPIOPin_Set_RejectedWhenOwnedByAnalog(t *testing.T) {
	tb := newTestBoardWithCaps(t, unoCaps(), unoAnalogMap(),
		[]board.AnalogReaderConfig{{Name: "joy", Pin: "A0"}})
	defer tb.cleanup()

	pin, err := tb.b.GPIOPinByName("14") // A0 == digital 14
	if err != nil {
		t.Fatalf("GPIOPinByName: %v", err)
	}
	err = pin.Set(context.Background(), true, nil)
	if err == nil {
		t.Fatal("Set on owned pin: want error, got nil")
	}
	if got := tb.sentBuf.Len(); got != 0 {
		t.Errorf("Set on owned pin emitted %d bytes; want zero-IO refusal", got)
	}
}

func TestGPIOPin_Get_RejectedWhenOwnedByAnalog(t *testing.T) {
	tb := newTestBoardWithCaps(t, unoCaps(), unoAnalogMap(),
		[]board.AnalogReaderConfig{{Name: "joy", Pin: "A0"}})
	defer tb.cleanup()

	pin, _ := tb.b.GPIOPinByName("14")
	if _, err := pin.Get(context.Background(), nil); err == nil {
		t.Fatal("Get on owned pin: want error, got nil")
	}
}

func TestGPIOPin_Set_UnaffectedWhenAnotherPinIsOwned(t *testing.T) {
	tb := newTestBoardWithCaps(t, unoCaps(), unoAnalogMap(),
		[]board.AnalogReaderConfig{{Name: "joy", Pin: "A0"}})
	defer tb.cleanup()

	pin, _ := tb.b.GPIOPinByName("13")
	if err := pin.Set(context.Background(), true, nil); err != nil {
		t.Fatalf("Set on unowned pin: %v", err)
	}
}

func TestSetPWM_OnPWMCapablePin_EmitsModeAndAnalogMessage(t *testing.T) {
	tb := newTestBoardWithCaps(t, unoCaps(), unoAnalogMap(), nil)
	defer tb.cleanup()

	pin, err := tb.b.GPIOPinByName("9") // PWM-capable on Uno
	if err != nil {
		t.Fatalf("GPIOPinByName: %v", err)
	}
	if err := pin.SetPWM(context.Background(), 0.5, nil); err != nil {
		t.Fatalf("SetPWM: %v", err)
	}

	// SET_PIN_MODE(9, PWM=0x03) + ANALOG_MESSAGE channel 9, value 127.
	// 0.5 * 255 = 127.5 -> 127 after uint16 truncation.
	want := []byte{
		0xF4, 0x09, 0x03,
		0xE9, 0x7F, 0x00,
	}
	if got := tb.sentBuf.Bytes(); !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}

	// PWM() returns the cached duty.
	got, err := pin.PWM(context.Background(), nil)
	if err != nil {
		t.Fatalf("PWM: %v", err)
	}
	if got != 0.5 {
		t.Errorf("PWM cached = %v, want 0.5", got)
	}
}

func TestSetPWM_OnNonPWMPin_RejectsWithoutWire(t *testing.T) {
	tb := newTestBoardWithCaps(t, unoCaps(), unoAnalogMap(), nil)
	defer tb.cleanup()

	pin, _ := tb.b.GPIOPinByName("2") // NOT PWM-capable on Uno
	err := pin.SetPWM(context.Background(), 0.5, nil)
	if err == nil {
		t.Fatal("SetPWM on non-PWM pin: want error, got nil")
	}
	if got := tb.sentBuf.Len(); got != 0 {
		t.Errorf("emitted %d bytes; want zero", got)
	}
}

func TestSetPWM_HighPin_EmitsExtendedAnalog(t *testing.T) {
	// Synthetic ESP32-shape capability: pin 20 is PWM-capable.
	caps := firmata.CapabilityResponse{Pins: make([]firmata.PinCapabilities, 32)}
	caps.Pins[20] = firmata.PinCapabilities{
		firmata.PinModeOutput: 1,
		firmata.PinModePWM:    8,
	}
	amap := firmata.AnalogMappingResponse{ChannelByPin: make([]uint8, 32)}
	for i := range amap.ChannelByPin {
		amap.ChannelByPin[i] = 0x7F
	}

	tb := newTestBoardWithCaps(t, caps, amap, nil)
	defer tb.cleanup()

	pin, _ := tb.b.GPIOPinByName("20")
	if err := pin.SetPWM(context.Background(), 0.5, nil); err != nil {
		t.Fatalf("SetPWM: %v", err)
	}

	// SET_PIN_MODE(20, PWM) + EXTENDED_ANALOG sysex.
	want := []byte{
		0xF4, 20, 0x03,
		0xF0, 0x6F, 20, 0x7F, 0x00, 0xF7, // value 127 = 0x7F lsb, 0x00 msb
	}
	if got := tb.sentBuf.Bytes(); !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestPWM_DefaultsToZero(t *testing.T) {
	tb := newTestBoardWithCaps(t, unoCaps(), unoAnalogMap(), nil)
	defer tb.cleanup()

	pin, _ := tb.b.GPIOPinByName("9")
	got, err := pin.PWM(context.Background(), nil)
	if err != nil {
		t.Fatalf("PWM: %v", err)
	}
	if got != 0 {
		t.Errorf("PWM before any SetPWM = %v, want 0", got)
	}
}

func TestSet_AfterStreamClose_ReturnsError(t *testing.T) {
	tb := newTestBoard(t)
	defer tb.cleanup()

	// Close the "Arduino" side: the Client's reader will observe io.EOF and
	// store the error; the next write from the board must surface it.
	if err := tb.arduinoW.Close(); err != nil {
		t.Fatalf("close arduinoW: %v", err)
	}

	// Give the reader goroutine a beat to propagate the error.
	time.Sleep(50 * time.Millisecond)

	pin, _ := tb.b.GPIOPinByName("13")
	// Note: depending on timing, first Set may still succeed (SET_PIN_MODE is
	// write-only, readErr is only surfaced when it has been set). Poll for up
	// to 1s for the error to materialize.
	deadline := time.Now().Add(time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = pin.Set(context.Background(), true, nil)
		if lastErr != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr == nil {
		t.Fatalf("expected an error after stream close, got nil")
	}
}
