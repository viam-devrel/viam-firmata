package firmata

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// pipePair gives the test both ends of a bidirectional in-memory byte stream.
// host side is what the Client reads/writes. board side is what the "fake Arduino"
// in the test reads/writes.
type pipePair struct {
	host  io.ReadWriteCloser
	board io.ReadWriteCloser
}

type rwcWrapper struct {
	io.Reader
	io.Writer
	close func() error
}

func (w rwcWrapper) Close() error { return w.close() }

func newPipePair() *pipePair {
	boardR, hostW := io.Pipe() // host writes -> board reads
	hostR, boardW := io.Pipe() // board writes -> host reads
	host := rwcWrapper{
		Reader: hostR,
		Writer: hostW,
		close:  func() error { _ = hostW.Close(); _ = hostR.Close(); return nil },
	}
	board := rwcWrapper{
		Reader: boardR,
		Writer: boardW,
		close:  func() error { _ = boardW.Close(); _ = boardR.Close(); return nil },
	}
	return &pipePair{host: host, board: board}
}

func TestClientCloseStopsReader(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	// Closing the host side must unblock the reader and close Events().
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-c.Events():
		if ok {
			t.Fatal("expected Events channel to be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("reader goroutine did not exit within 1s")
	}
}

func TestHandshakeSucceeds(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	// Fake board sends a REPORT_VERSION 2.5 frame.
	go func() {
		_, _ = pp.board.Write([]byte{0xF9, 0x02, 0x05})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	major, minor, err := c.Handshake(ctx)
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if major != 2 || minor != 5 {
		t.Errorf("got %d.%d, want 2.5", major, minor)
	}
}

func TestHandshakeTimesOut(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := c.Handshake(ctx)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// readN reads exactly n bytes or fails the test.
func readN(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	if err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	return buf
}

func TestSetPinModeWritesBytes(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	go func() { _ = c.SetPinMode(13, PinModeOutput) }()
	got := readN(t, pp.board, 3)
	want := []byte{0xF4, 0x0D, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestEnableDigitalReportingWritesBytes(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	go func() { _ = c.EnableDigitalReporting(0, true) }()
	got := readN(t, pp.board, 2)
	want := []byte{0xD0, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestDigitalWriteTracksPortMask(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	// Drive pin 13 HIGH. pin 13 = port 1, bit 5 -> mask 0x20.
	// Expected frame: 0x91 0x20 0x00
	go func() { _ = c.DigitalWrite(13, true) }()
	got := readN(t, pp.board, 3)
	if want := []byte{0x91, 0x20, 0x00}; !bytes.Equal(got, want) {
		t.Fatalf("first write: got % X, want % X", got, want)
	}

	// Drive pin 12 HIGH (same port, bit 4). Mask must now be 0x30.
	go func() { _ = c.DigitalWrite(12, true) }()
	got = readN(t, pp.board, 3)
	if want := []byte{0x91, 0x30, 0x00}; !bytes.Equal(got, want) {
		t.Fatalf("second write: got % X, want % X", got, want)
	}

	// Drive pin 13 LOW again — mask goes back to 0x10.
	go func() { _ = c.DigitalWrite(13, false) }()
	got = readN(t, pp.board, 3)
	if want := []byte{0x91, 0x10, 0x00}; !bytes.Equal(got, want) {
		t.Fatalf("third write: got % X, want % X", got, want)
	}
}

func TestEventsEmittedPerChangedBit(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	// Board reports port 0 mask 0x05 (pins 0 and 2 high). Starting mask is 0
	// so both pin 0 and pin 2 should emit a HIGH event.
	_, _ = pp.board.Write([]byte{0x90, 0x05, 0x00})

	got := collect(t, c.Events(), 2, time.Second)
	wantSet := map[PinChange]bool{
		{Pin: 0, High: true}: true,
		{Pin: 2, High: true}: true,
	}
	for _, ev := range got {
		if !wantSet[ev] {
			t.Errorf("unexpected event %+v", ev)
		}
		delete(wantSet, ev)
	}
	if len(wantSet) != 0 {
		t.Errorf("missing events: %+v", wantSet)
	}

	// Now board reports mask 0x01 — pin 2 went LOW, pin 0 unchanged.
	_, _ = pp.board.Write([]byte{0x90, 0x01, 0x00})
	got = collect(t, c.Events(), 1, time.Second)
	if len(got) != 1 || got[0] != (PinChange{Pin: 2, High: false}) {
		t.Errorf("got %+v, want [{Pin:2 High:false}]", got)
	}
}

func collect(t *testing.T, ch <-chan PinChange, n int, timeout time.Duration) []PinChange {
	t.Helper()
	deadline := time.After(timeout)
	out := make([]PinChange, 0, n)
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("events channel closed after %d of %d events", len(out), n)
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for events; got %d of %d: %+v", len(out), n, out)
		}
	}
	return out
}

func TestReadDigital_BeforeAndAfterDispatch(t *testing.T) {
	arduinoR, clientW := io.Pipe()
	clientR, arduinoW := io.Pipe()
	defer arduinoR.Close()
	defer arduinoW.Close()

	// Wrap the two pipes as an io.ReadWriteCloser for the Client.
	rw := &rwAdapter{r: clientR, w: clientW}
	c := New(rw)
	defer c.Close()

	// Before any DIGITAL_MESSAGE has arrived, all pins read false.
	if c.ReadDigital(2) {
		t.Fatalf("ReadDigital(2) before any frame: got true, want false")
	}

	// Push a DIGITAL_MESSAGE for port 0 with bit 2 set (pin 2 = HIGH).
	// Encoding: 0x90 | 0 = 0x90, data1 = 0x04, data2 = 0x00.
	_, err := arduinoW.Write([]byte{0x90, 0x04, 0x00})
	if err != nil {
		t.Fatalf("write frame: %v", err)
	}

	// Wait for the reader goroutine to process the frame.
	// The existing Client exposes Events(); drain one and then read.
	select {
	case ev := <-c.Events():
		if ev.Pin != 2 || !ev.High {
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatalf("no event received")
	}

	if !c.ReadDigital(2) {
		t.Fatalf("ReadDigital(2) after dispatch: got false, want true")
	}
	if c.ReadDigital(1) {
		t.Fatalf("ReadDigital(1) unrelated bit: got true, want false")
	}
	if c.ReadDigital(-1) || c.ReadDigital(128) {
		t.Fatalf("out-of-range pins should return false")
	}
}

func TestReadAnalog_BeforeAndAfterDispatch(t *testing.T) {
	arduinoR, clientW := io.Pipe()
	clientR, arduinoW := io.Pipe()
	defer arduinoR.Close()
	defer arduinoW.Close()

	rw := &rwAdapter{r: clientR, w: clientW}
	c := New(rw)
	defer c.Close()

	// Before any ANALOG_MESSAGE arrives, all channels read 0.
	if v := c.ReadAnalog(0); v != 0 {
		t.Fatalf("ReadAnalog(0) before any frame: got %d, want 0", v)
	}

	// Push ANALOG_MESSAGE for channel 0, value 512: 0xE0 0x00 0x04.
	if _, err := arduinoW.Write([]byte{0xE0, 0x00, 0x04}); err != nil {
		t.Fatalf("write frame: %v", err)
	}

	// Poll for the dispatch (no events channel for analog; rely on cached state).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if c.ReadAnalog(0) == 512 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v := c.ReadAnalog(0); v != 512 {
		t.Fatalf("ReadAnalog(0) after dispatch: got %d, want 512", v)
	}
	if v := c.ReadAnalog(1); v != 0 {
		t.Fatalf("ReadAnalog(1) unrelated: got %d, want 0", v)
	}

	// Out-of-range pins return 0.
	if c.ReadAnalog(-1) != 0 || c.ReadAnalog(16) != 0 {
		t.Fatalf("out-of-range channels should return 0")
	}
}

func TestAnalogWrite_LowChannel_EmitsAnalogMessage(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	go func() { _ = c.AnalogWrite(6, 200) }()
	got := readN(t, pp.board, 3)
	want := []byte{0xE6, 0x48, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestAnalogWrite_HighPin_EmitsExtendedAnalog(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	go func() { _ = c.AnalogWrite(20, 200) }()
	got := readN(t, pp.board, 6)
	want := []byte{0xF0, 0x6F, 20, 0x48, 0x01, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestAnalogWrite_OutOfRange(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()
	if err := c.AnalogWrite(-1, 0); err == nil {
		t.Errorf("AnalogWrite(-1): want error, got nil")
	}
	if err := c.AnalogWrite(128, 0); err == nil {
		t.Errorf("AnalogWrite(128): want error, got nil")
	}
}

func TestEnableAnalogReportingWritesBytes(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	go func() { _ = c.EnableAnalogReporting(0, true) }()
	got := readN(t, pp.board, 2)
	want := []byte{0xC0, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestEnableAnalogReporting_OutOfRange(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()
	if err := c.EnableAnalogReporting(-1, true); err == nil {
		t.Errorf("EnableAnalogReporting(-1): want error")
	}
	if err := c.EnableAnalogReporting(16, true); err == nil {
		t.Errorf("EnableAnalogReporting(16): want error")
	}
}

func TestSetSamplingIntervalWritesBytes(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	go func() { _ = c.SetSamplingInterval(100) }()
	got := readN(t, pp.board, 5)
	want := []byte{0xF0, 0x7A, 0x64, 0x00, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestSetSamplingInterval_OutOfRange(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()
	if err := c.SetSamplingInterval(0); err == nil {
		t.Errorf("SetSamplingInterval(0): want error")
	}
	if err := c.SetSamplingInterval(16384); err == nil {
		t.Errorf("SetSamplingInterval(16384): want error")
	}
}

func TestQueryCapabilities_Succeeds(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	// Fake board responds with a 2-pin capability payload.
	go func() {
		_, _ = pp.board.Write([]byte{
			0xF0, 0x6C,
			0x00, 0x01, 0x01, 0x01, 0x7F, // pin 0: INPUT(1), OUTPUT(1)
			0x03, 0x08, 0x7F, // pin 1: PWM(8)
			0xF7,
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cr, err := c.QueryCapabilities(ctx)
	if err != nil {
		t.Fatalf("QueryCapabilities: %v", err)
	}
	if len(cr.Pins) != 2 {
		t.Fatalf("len(Pins) = %d, want 2", len(cr.Pins))
	}
	if cr.Pins[1][PinModePWM] != 8 {
		t.Errorf("pin 1 PWM resolution = %d, want 8", cr.Pins[1][PinModePWM])
	}
}

func TestQueryCapabilities_TimesOut(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	// Drain the outbound query bytes so the writer doesn't block forever.
	go func() {
		buf := make([]byte, 64)
		_, _ = pp.board.Read(buf)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.QueryCapabilities(ctx); err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestQueryAnalogMapping_Succeeds(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	go func() {
		_, _ = pp.board.Write([]byte{0xF0, 0x6A, 0x7F, 0x7F, 0x00, 0x01, 0xF7})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	am, err := c.QueryAnalogMapping(ctx)
	if err != nil {
		t.Fatalf("QueryAnalogMapping: %v", err)
	}
	want := []uint8{0x7F, 0x7F, 0x00, 0x01}
	if !bytes.Equal(am.ChannelByPin, want) {
		t.Errorf("got % X, want % X", am.ChannelByPin, want)
	}
}

type rwAdapter struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (a *rwAdapter) Read(p []byte) (int, error)  { return a.r.Read(p) }
func (a *rwAdapter) Write(p []byte) (int, error) { return a.w.Write(p) }
func (a *rwAdapter) Close() error {
	// Close both ends so the Client's reader goroutine, which is blocked
	// on the read side, unblocks and lets Close() return.
	_ = a.r.Close()
	return a.w.Close()
}
