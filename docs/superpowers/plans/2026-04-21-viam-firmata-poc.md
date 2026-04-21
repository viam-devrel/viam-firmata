# viam-firmata PoC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a minimal Go program that talks to an Arduino running ConfigurableFirmata over USB serial: toggle a digital OUTPUT pin while streaming pin-change events from a digital INPUT_PULLUP pin.

**Architecture:** Hybrid wire-protocol implementation. A pure, hardware-free `internal/firmata` package encodes/decodes the ~5 Firmata messages we need against an `io.ReadWriteCloser`. A thin `cmd/firmata-poc` binary is the only component that imports a serial library (`go.bug.st/serial`). See the design at `docs/superpowers/specs/2026-04-21-viam-firmata-poc-design.md`.

**Tech Stack:**
- Go 1.22+
- `go.bug.st/serial` for the serial port (main binary only)
- Go stdlib only inside `internal/firmata` (`io`, `bufio`, `sync`, `sync/atomic`, `context`, `errors`)
- `go test` (stdlib testing, table-driven) — no third-party assertion library

**Design anchors (read before starting):**
- Firmata 2.x protocol reference: https://github.com/firmata/protocol (specifically `protocol.md` and `feature-protocol.md`). All byte-layout claims below are from this spec.
- Command bytes have bit 7 set. Data bytes have bit 7 clear. If we read an unexpected data byte mid-frame, we resync by discarding until we next see a byte with bit 7 set.
- `DIGITAL_MESSAGE` is per-**port** (group of 8 pins), not per-pin. `DigitalWrite(pin)` must read-modify-write the cached output mask for `pin/8`.

**Target file layout when plan is complete:**

```
viam-firmata/
├── README.md
├── go.mod
├── go.sum
├── docs/superpowers/{specs,plans}/...        (already written)
├── cmd/firmata-poc/main.go
└── internal/firmata/
    ├── protocol.go
    ├── protocol_test.go
    ├── client.go
    └── client_test.go
```

---

## Task 1: Initialize Go module and install serial dependency

**Files:**
- Create: `go.mod`
- Create: `go.sum`

- [ ] **Step 1: Initialize the module**

From the repo root (`/Users/nick.hehr/src/viam-firmata`):

```bash
go mod init github.com/viam-labs/viam-firmata
```

Expected: creates `go.mod` with `module github.com/viam-labs/viam-firmata` and `go 1.22` (or whatever the installed Go version reports).

- [ ] **Step 2: Add the serial dependency**

```bash
go get go.bug.st/serial@latest
```

Expected: populates `go.sum`, adds a `require go.bug.st/serial vX.Y.Z` line to `go.mod`. No code yet, so `go build` would report "no Go files" — that's fine.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: initialize Go module with go.bug.st/serial dependency"
```

---

## Task 2: Protocol constants, types, and encode functions (TDD)

**Files:**
- Create: `internal/firmata/protocol.go`
- Create: `internal/firmata/protocol_test.go`

- [ ] **Step 1: Write the failing table test for all three encode functions**

Create `internal/firmata/protocol_test.go`:

```go
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
```

- [ ] **Step 2: Run the test and verify it fails**

```bash
go test ./internal/firmata/...
```

Expected: compile error — `undefined: encodePinMode`, `undefined: PinModeOutput`, etc.

- [ ] **Step 3: Implement `protocol.go`**

Create `internal/firmata/protocol.go`:

```go
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
```

- [ ] **Step 4: Run the test and verify it passes**

```bash
go test ./internal/firmata/... -v -run TestEncode
```

Expected: all subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/firmata/protocol.go internal/firmata/protocol_test.go
git commit -m "feat(firmata): add protocol constants and encode functions"
```

---

## Task 3: Protocol decode function (TDD)

**Files:**
- Modify: `internal/firmata/protocol.go`
- Modify: `internal/firmata/protocol_test.go`

- [ ] **Step 1: Add message type declarations and decode test to `protocol_test.go`**

Append to `internal/firmata/protocol_test.go`:

```go
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
```

Also add the missing `bufio` import at the top of the file (`"bufio"` alongside `"bytes"` and `"testing"`).

- [ ] **Step 2: Run the tests and verify they fail**

```bash
go test ./internal/firmata/... -v -run TestDecode
```

Expected: compile error — `undefined: decode`, `undefined: VersionMessage`, `undefined: DigitalPortMessage`, `undefined: UnknownMessage`.

- [ ] **Step 3: Implement decode in `protocol.go`**

Append to `internal/firmata/protocol.go`:

```go
import (
	"bufio"
	"fmt"
	"io"
)

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

	case cmd == cmdStartSysex:
		// Consume until END_SYSEX. First byte after 0xF0 is the sysex command id.
		payload, err := readUntilEndSysex(r)
		if err != nil {
			return nil, fmt.Errorf("read sysex: %w", err)
		}
		return UnknownMessage{Cmd: cmdStartSysex, Payload: payload}, nil

	default:
		// Any other command byte in the 0x80..0xEF range is a 3-byte message
		// per the Firmata spec (ANALOG_MESSAGE, etc.) — consume the 2 data bytes.
		// Commands 0xF1..0xFF other than the ones we handle above are rare enough
		// we treat them identically.
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

func readUntilEndSysex(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == cmdEndSysex {
			return out, nil
		}
		out = append(out, b)
	}
}

// io is reserved for client.go; silence the unused import if the linter complains.
var _ io.Reader = (*bufio.Reader)(nil)
```

Note: the `import` block above must be merged with the existing top-of-file imports, not a second block. The stub `var _ io.Reader` line exists only so that `io` is tracked as a used package once `client.go` lands; delete it after `client.go` is written if `goimports` complains.

- [ ] **Step 4: Run the tests and verify they pass**

```bash
go test ./internal/firmata/... -v
```

Expected: all `TestEncode` + `TestDecode_*` subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/firmata/protocol.go internal/firmata/protocol_test.go
git commit -m "feat(firmata): add wire-protocol decoder with sysex skip and resync"
```

---

## Task 4: Client skeleton — New, Close, fake-board test harness

**Files:**
- Create: `internal/firmata/client.go`
- Create: `internal/firmata/client_test.go`

- [ ] **Step 1: Write a failing test that constructs a Client and calls Close**

Create `internal/firmata/client_test.go`:

```go
package firmata

import (
	"io"
	"testing"
	"time"
)

// pipePair gives the test both ends of a bidirectional in-memory byte stream.
// host side is what the Client reads/writes. board side is what the "fake Arduino"
// in the test reads/writes.
type pipePair struct {
	hostIn, hostOut *io.PipeWriter
	host            io.ReadWriteCloser
	board           io.ReadWriteCloser
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
	return &pipePair{hostIn: boardW, hostOut: hostW, host: host, board: board}
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
```

- [ ] **Step 2: Run the test and verify it fails**

```bash
go test ./internal/firmata/... -v -run TestClientCloseStopsReader
```

Expected: compile error — `undefined: New`, `undefined: Client`, method `.Close()` / `.Events()` missing.

- [ ] **Step 3: Implement the skeleton in `client.go`**

Create `internal/firmata/client.go`:

```go
package firmata

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

type PinChange struct {
	Pin  int
	High bool
}

type Client struct {
	rw         io.ReadWriteCloser
	br         *bufio.Reader
	portState  [16]uint8 // last-known input mask per port
	outState   [16]uint8 // last-written output mask per port
	events     chan PinChange
	version    chan VersionMessage
	readErr    atomic.Pointer[error]
	readerDone chan struct{}
	writeMu    sync.Mutex
	closed     atomic.Bool
}

func New(rw io.ReadWriteCloser) *Client {
	c := &Client{
		rw:         rw,
		br:         bufio.NewReader(rw),
		events:     make(chan PinChange, 16),
		version:    make(chan VersionMessage, 1),
		readerDone: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *Client) Events() <-chan PinChange { return c.events }

func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	err := c.rw.Close()
	<-c.readerDone
	return err
}

// readLoop decodes frames until the underlying stream errors out.
// It dispatches recognized frames to the appropriate channels and exits cleanly,
// closing events and storing any error in readErr for the next writer to surface.
func (c *Client) readLoop() {
	defer close(c.readerDone)
	defer close(c.events)
	for {
		msg, err := decode(c.br)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				c.readErr.Store(&err)
			}
			return
		}
		switch m := msg.(type) {
		case VersionMessage:
			// Non-blocking send: if nobody is waiting we drop it.
			select {
			case c.version <- m:
			default:
			}
		case DigitalPortMessage:
			c.dispatchDigital(m)
		case UnknownMessage:
			// Ignore — sysex, analog reports we didn't subscribe to, etc.
		}
	}
}

// dispatchDigital diffs the new port mask against the last-known one
// and emits one PinChange per changed bit.
func (c *Client) dispatchDigital(m DigitalPortMessage) {
	prev := c.portState[m.Port]
	changed := prev ^ m.Mask
	for bit := uint8(0); bit < 8; bit++ {
		if changed&(1<<bit) == 0 {
			continue
		}
		c.events <- PinChange{
			Pin:  int(m.Port)*8 + int(bit),
			High: m.Mask&(1<<bit) != 0,
		}
	}
	c.portState[m.Port] = m.Mask
}

// writeFrame sends an already-encoded frame, honoring writeMu and surfacing
// any prior reader-side error.
func (c *Client) writeFrame(frame []byte) error {
	if errp := c.readErr.Load(); errp != nil {
		return fmt.Errorf("firmata: stream closed: %w", *errp)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.rw.Write(frame)
	return err
}
```

- [ ] **Step 4: Run the test and verify it passes**

```bash
go test ./internal/firmata/... -v -run TestClientCloseStopsReader
```

Expected: PASS within 1s.

- [ ] **Step 5: Remove the temporary `var _ io.Reader` line from `protocol.go`**

Now that `client.go` uses `io`, delete the stub from the end of `protocol.go`. Re-run `go test ./...` — still green.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/client.go internal/firmata/client_test.go internal/firmata/protocol.go
git commit -m "feat(firmata): add Client skeleton with reader goroutine and Close"
```

---

## Task 5: Handshake (TDD)

**Files:**
- Modify: `internal/firmata/client.go`
- Modify: `internal/firmata/client_test.go`

- [ ] **Step 1: Write the failing handshake tests**

Append to `internal/firmata/client_test.go`:

```go
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
```

Also add `"context"` to the test file's import list.

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/firmata/... -v -run TestHandshake
```

Expected: `undefined: c.Handshake`.

- [ ] **Step 3: Implement Handshake**

Append to `client.go`:

```go
// Handshake blocks until the firmware sends a REPORT_VERSION frame or ctx expires.
// It also proactively sends a REPORT_VERSION query, because some firmwares only
// send the version on request (not on reset).
func (c *Client) Handshake(ctx context.Context) (major, minor uint8, err error) {
	// Ask, in case the auto-emit was missed or the firmware doesn't send one on reset.
	_ = c.writeFrame([]byte{cmdReportVersion})
	select {
	case v := <-c.version:
		return v.Major, v.Minor, nil
	case <-ctx.Done():
		return 0, 0, fmt.Errorf("handshake: %w (no REPORT_VERSION received — wrong port or baud?)", ctx.Err())
	}
}
```

- [ ] **Step 4: Run and confirm passing**

```bash
go test ./internal/firmata/... -v -run TestHandshake
```

Expected: both tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/firmata/client.go internal/firmata/client_test.go
git commit -m "feat(firmata): implement handshake with REPORT_VERSION query and timeout"
```

---

## Task 6: SetPinMode and EnableDigitalReporting (TDD)

**Files:**
- Modify: `internal/firmata/client.go`
- Modify: `internal/firmata/client_test.go`

- [ ] **Step 1: Write the failing tests that assert on bytes written to the fake board**

Append to `client_test.go`:

```go
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
```

Add `"bytes"` to the test file imports if not already there.

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/firmata/... -v -run 'TestSetPinMode|TestEnableDigitalReporting'
```

Expected: `undefined: SetPinMode`, `undefined: EnableDigitalReporting`.

- [ ] **Step 3: Implement both methods**

Append to `client.go`:

```go
func (c *Client) SetPinMode(pin int, mode PinMode) error {
	if pin < 0 || pin > 127 {
		return fmt.Errorf("pin %d out of range", pin)
	}
	return c.writeFrame(encodePinMode(uint8(pin), mode))
}

// EnableDigitalReporting enables or disables auto-reporting for a port.
// NOTE: port is a port index (pin/8), not a pin number.
func (c *Client) EnableDigitalReporting(port int, enable bool) error {
	if port < 0 || port > 15 {
		return fmt.Errorf("port %d out of range", port)
	}
	return c.writeFrame(encodeReportDigital(uint8(port), enable))
}
```

- [ ] **Step 4: Run and confirm passing**

```bash
go test ./internal/firmata/... -v
```

Expected: all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/firmata/client.go internal/firmata/client_test.go
git commit -m "feat(firmata): add SetPinMode and EnableDigitalReporting"
```

---

## Task 7: DigitalWrite with output-mask tracking (TDD)

**Files:**
- Modify: `internal/firmata/client.go`
- Modify: `internal/firmata/client_test.go`

- [ ] **Step 1: Write the failing test**

Append to `client_test.go`:

```go
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
```

- [ ] **Step 2: Run and confirm failure**

```bash
go test ./internal/firmata/... -v -run TestDigitalWrite
```

Expected: `undefined: DigitalWrite`.

- [ ] **Step 3: Implement DigitalWrite**

Append to `client.go`:

```go
// DigitalWrite sets a single pin HIGH or LOW. It read-modify-writes the cached
// output mask for the pin's port, so multiple pins on the same port coexist.
func (c *Client) DigitalWrite(pin int, high bool) error {
	if pin < 0 || pin > 127 {
		return fmt.Errorf("pin %d out of range", pin)
	}
	port := uint8(pin / 8)
	bit := uint8(pin % 8)

	c.writeMu.Lock()
	mask := c.outState[port]
	if high {
		mask |= 1 << bit
	} else {
		mask &^= 1 << bit
	}
	c.outState[port] = mask
	c.writeMu.Unlock()

	return c.writeFrame(encodeDigitalPortWrite(port, mask))
}
```

(Note: we hold `writeMu` briefly to read-modify the state array, then release it so `writeFrame` can take it again for the actual write. An alternative is a separate `stateMu`; for this PoC we keep one mutex.)

Actually, simpler — `writeFrame` already takes `writeMu`. Nesting is a deadlock. Restructure `DigitalWrite` to inline the write under a single lock acquisition:

```go
func (c *Client) DigitalWrite(pin int, high bool) error {
	if pin < 0 || pin > 127 {
		return fmt.Errorf("pin %d out of range", pin)
	}
	port := uint8(pin / 8)
	bit := uint8(pin % 8)

	if errp := c.readErr.Load(); errp != nil {
		return fmt.Errorf("firmata: stream closed: %w", *errp)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if high {
		c.outState[port] |= 1 << bit
	} else {
		c.outState[port] &^= 1 << bit
	}
	_, err := c.rw.Write(encodeDigitalPortWrite(port, c.outState[port]))
	return err
}
```

Use this second form. Remove any lingering version of the first.

- [ ] **Step 4: Run and confirm passing**

```bash
go test ./internal/firmata/... -v
```

Expected: all tests PASS, no deadlocks, no races when run with `-race`.

```bash
go test -race ./internal/firmata/...
```

Expected: PASS with no data-race warnings.

- [ ] **Step 5: Commit**

```bash
git add internal/firmata/client.go internal/firmata/client_test.go
git commit -m "feat(firmata): add DigitalWrite with per-port output-mask tracking"
```

---

## Task 8: PinChange event dispatch end-to-end (TDD)

**Files:**
- Modify: `internal/firmata/client_test.go`

- [ ] **Step 1: Write the failing test that pushes a digital frame and consumes the resulting events**

Append to `client_test.go`:

```go
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
```

- [ ] **Step 2: Run the test and verify it passes without code changes**

The reader goroutine + `dispatchDigital` were already implemented in Task 4. This test is a correctness check, not a new feature.

```bash
go test ./internal/firmata/... -v -run TestEventsEmittedPerChangedBit
go test -race ./internal/firmata/...
```

Expected: PASS (both the targeted test and the full suite under `-race`).

- [ ] **Step 3: If it fails, fix `dispatchDigital`**

If the assertion order-sensitivity bites, debug the diff logic. Likely issue: forgetting to update `portState` after emitting events, or off-by-one on `bit`.

- [ ] **Step 4: Commit**

```bash
git add internal/firmata/client_test.go
git commit -m "test(firmata): cover PinChange diff dispatch across two port updates"
```

---

## Task 9: `cmd/firmata-poc` CLI main

**Files:**
- Create: `cmd/firmata-poc/main.go`

- [ ] **Step 1: Write `main.go`**

Create `cmd/firmata-poc/main.go`:

```go
// Command firmata-poc is a minimal proof-of-concept that drives a digital
// OUTPUT pin and streams pin-change events from a digital INPUT_PULLUP pin
// on an Arduino running ConfigurableFirmata, over USB serial.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"go.bug.st/serial"

	"github.com/viam-labs/viam-firmata/internal/firmata"
)

func main() {
	port := flag.String("port", "", "serial device path (e.g. /dev/tty.usbmodem14201) — required")
	baud := flag.Int("baud", 57600, "serial baud rate (ConfigurableFirmata default is 57600)")
	outPin := flag.Int("out-pin", 13, "digital pin to drive as OUTPUT (typically onboard LED)")
	inPin := flag.Int("in-pin", 2, "digital pin to read as INPUT_PULLUP")
	duration := flag.Duration("duration", 10*time.Second, "total run time")
	toggleInterval := flag.Duration("toggle-interval", 500*time.Millisecond, "how often to flip the output pin")
	flag.Parse()

	if *port == "" {
		fmt.Fprintln(os.Stderr, "error: -port is required")
		flag.Usage()
		os.Exit(2)
	}

	if err := run(*port, *baud, *outPin, *inPin, *duration, *toggleInterval); err != nil {
		log.Fatalf("firmata-poc: %v", err)
	}
}

func run(portPath string, baud, outPin, inPin int, duration, toggleInterval time.Duration) error {
	mode := &serial.Mode{BaudRate: baud}
	sp, err := serial.Open(portPath, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", portPath, err)
	}
	defer sp.Close()

	// Toggle DTR to trigger the Arduino bootloader reset, then wait for the
	// sketch to come up. The ~2s delay is intentionally hardcoded — the
	// Arduino bootloader's wait window is ~1.6s and this is not worth a flag.
	_ = sp.SetDTR(false)
	time.Sleep(100 * time.Millisecond)
	_ = sp.SetDTR(true)
	log.Println("waiting 2s for Arduino auto-reset...")
	time.Sleep(2 * time.Second)

	c := firmata.New(sp)
	defer c.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	hsCtx, hsCancel := context.WithTimeout(ctx, 5*time.Second)
	major, minor, err := c.Handshake(hsCtx)
	hsCancel()
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	log.Printf("connected — firmware Firmata v%d.%d", major, minor)

	if err := c.SetPinMode(outPin, firmata.PinModeOutput); err != nil {
		return fmt.Errorf("SetPinMode out: %w", err)
	}
	if err := c.SetPinMode(inPin, firmata.PinModeInputPullup); err != nil {
		return fmt.Errorf("SetPinMode in: %w", err)
	}
	if err := c.EnableDigitalReporting(inPin/8, true); err != nil {
		return fmt.Errorf("EnableDigitalReporting: %w", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, duration)
	defer runCancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ev := range c.Events() {
			level := "LOW"
			if ev.High {
				level = "HIGH"
			}
			log.Printf("pin %d -> %s", ev.Pin, level)
		}
	}()

	ticker := time.NewTicker(toggleInterval)
	defer ticker.Stop()
	var high bool
	log.Printf("driving pin %d every %s for %s (press ctrl-c to stop early)",
		outPin, toggleInterval, duration)
	for {
		select {
		case <-runCtx.Done():
			log.Println("run complete")
			_ = c.DigitalWrite(outPin, false) // leave the LED off
			// Close the client so the events loop exits.
			_ = c.Close()
			wg.Wait()
			return nil
		case <-ticker.C:
			high = !high
			if err := c.DigitalWrite(outPin, high); err != nil {
				return fmt.Errorf("DigitalWrite: %w", err)
			}
		}
	}
}
```

- [ ] **Step 2: Verify it builds**

```bash
go build ./cmd/firmata-poc
```

Expected: produces a `firmata-poc` binary in the working directory, no compile errors.

Remove the binary before committing:

```bash
rm firmata-poc
```

- [ ] **Step 3: Verify `-h` output works without hardware**

```bash
go run ./cmd/firmata-poc -h
```

Expected: flag usage printed, exit code 2 (standard for `-h`).

- [ ] **Step 4: Commit**

```bash
git add cmd/firmata-poc/main.go
git commit -m "feat(cli): add firmata-poc binary with flag-driven GPIO demo loop"
```

---

## Task 10: README.md with `arduino-cli` flashing instructions

**Files:**
- Create: `README.md`

- [ ] **Step 1: Write the README**

Create `README.md`:

````markdown
# viam-firmata

A minimal Go proof-of-concept that talks to an Arduino running
[ConfigurableFirmata](https://github.com/firmata/ConfigurableFirmata) over USB
serial. It toggles a digital OUTPUT pin and streams pin-change events from a
digital INPUT_PULLUP pin.

This is intentionally small and dependency-light: one serial library, a
hand-rolled codec for the ~5 Firmata messages we need, and one CLI binary.

## Hardware prerequisites

- An Arduino Uno (or any AVR board — Uno is the tested target).
- A USB cable.
- For the full demo: an LED + ~330Ω resistor on pin 13 (or use the onboard LED),
  and a momentary pushbutton wired from pin 2 to GND (internal pull-up is used).

## Software prerequisites

- Go 1.22 or newer.
- `arduino-cli` installed. On macOS: `brew install arduino-cli`. On Linux/Windows
  see [arduino-cli install docs](https://arduino.github.io/arduino-cli/latest/installation/).

## Flashing ConfigurableFirmata

Run the following once to set up `arduino-cli` and install the
ConfigurableFirmata library, then compile and upload the stock example sketch
to your board.

```sh
# First-time only: create the arduino-cli config file.
arduino-cli config init

# Install the AVR core and the ConfigurableFirmata library.
arduino-cli core update-index
arduino-cli core install arduino:avr
arduino-cli lib install ConfigurableFirmata

# Find your board's port and FQBN.
arduino-cli board list
# → note the port (e.g. /dev/tty.usbmodem14201 on macOS, /dev/ttyACM0 on Linux,
#   or COM3 on Windows) and FQBN (e.g. arduino:avr:uno).

# Locate the example sketch. Path depends on OS:
#   macOS:   ~/Documents/Arduino/libraries/ConfigurableFirmata/examples/ConfigurableFirmata
#   Linux:   ~/Arduino/libraries/ConfigurableFirmata/examples/ConfigurableFirmata
#   Windows: %USERPROFILE%\Documents\Arduino\libraries\ConfigurableFirmata\examples\ConfigurableFirmata
SKETCH="$HOME/Documents/Arduino/libraries/ConfigurableFirmata/examples/ConfigurableFirmata"

# Compile + upload.
arduino-cli compile --fqbn arduino:avr:uno "$SKETCH"
arduino-cli upload  --fqbn arduino:avr:uno --port /dev/tty.usbmodem14201 "$SKETCH"
```

## Running the Go PoC

```sh
# Default: blink pin 13, listen for input on pin 2, run for 10s.
go run ./cmd/firmata-poc -port /dev/tty.usbmodem14201

# Customize:
go run ./cmd/firmata-poc \
    -port /dev/tty.usbmodem14201 \
    -out-pin 13 \
    -in-pin 2 \
    -duration 15s \
    -toggle-interval 250ms
```

Expected output:

```
waiting 2s for Arduino auto-reset...
connected — firmware Firmata v2.10
driving pin 13 every 500ms for 10s (press ctrl-c to stop early)
pin 2 -> LOW        # ← button pressed
pin 2 -> HIGH       # ← button released
...
run complete
```

## Troubleshooting

- **`handshake: ... no REPORT_VERSION received`** — wrong serial port path or
  wrong baud rate. Re-run `arduino-cli board list` to confirm the port, and
  make sure the sketch you uploaded was ConfigurableFirmata (not StandardFirmata
  or a blank sketch).
- **`permission denied` on `/dev/tty*`** — on Linux, add your user to the
  `dialout` (Debian/Ubuntu) or `uucp` (Arch) group and log out/in.
- **Pin 13 doesn't blink** — the Arduino needs a fresh auto-reset on each run;
  unplug and replug the USB cable, or press the onboard RESET button.
- **Garbage bytes in the first second** — normal. The decoder skips non-command
  bytes until it sees a valid Firmata frame.

## Running the tests

The `internal/firmata` package is hardware-free and fully unit-tested:

```sh
go test ./...
go test -race ./...
```

## Design docs

- Spec: [`docs/superpowers/specs/2026-04-21-viam-firmata-poc-design.md`](docs/superpowers/specs/2026-04-21-viam-firmata-poc-design.md)
- Plan: [`docs/superpowers/plans/2026-04-21-viam-firmata-poc.md`](docs/superpowers/plans/2026-04-21-viam-firmata-poc.md)
````

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: add README with arduino-cli flashing and run instructions"
```

---

## Task 11: Final verification

- [ ] **Step 1: Full suite runs green with race detector**

```bash
go test -race ./...
```

Expected: all packages PASS, no race warnings.

- [ ] **Step 2: `go vet` is clean**

```bash
go vet ./...
```

Expected: no output.

- [ ] **Step 3: Binary builds**

```bash
go build ./cmd/firmata-poc && rm firmata-poc
```

- [ ] **Step 4: Tree matches spec**

```bash
find . -type f -not -path './.git/*' | sort
```

Expected (roughly):

```
./README.md
./cmd/firmata-poc/main.go
./docs/superpowers/plans/2026-04-21-viam-firmata-poc.md
./docs/superpowers/specs/2026-04-21-viam-firmata-poc-design.md
./go.mod
./go.sum
./internal/firmata/client.go
./internal/firmata/client_test.go
./internal/firmata/protocol.go
./internal/firmata/protocol_test.go
```

- [ ] **Step 5: End-to-end hardware test**

With a real Arduino flashed per the README:

```bash
go run ./cmd/firmata-poc -port /dev/tty.usbmodem14201
```

Watch the LED blink, press the button, confirm `pin N -> LOW/HIGH` lines in the
log. If the run completes cleanly in 10s with no error, the PoC is done.

- [ ] **Step 6: Final commit marking the PoC complete**

If no files changed in Step 1–5, skip. Otherwise:

```bash
git add -A
git commit -m "chore: finalize PoC — lint clean, tests green with -race"
```

---

## Post-plan notes

- The `internal/firmata` package is deliberately **internal**. If a second consumer appears (likely: a Viam board module), move it to `pkg/firmata` and promote the types (lowercase `encodeX` helpers can stay internal; `Client`, `PinMode`, `PinChange`, `Handshake`, `SetPinMode`, `DigitalWrite`, `EnableDigitalReporting`, `Events`, `Close` are the candidate public surface).
- Adding analog support later is one new command byte (`ANALOG_MESSAGE` 0xE0-0xEF) and a symmetric `AnalogChange` event — the decoder `default:` branch already consumes the right number of bytes.
