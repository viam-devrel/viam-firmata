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

	_ = errors.New // keep import used if we add error-type assertions later
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
