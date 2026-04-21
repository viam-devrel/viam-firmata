# viam-firmata Board Module Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wrap the existing `internal/firmata` client as a Viam `board` component module (`devrel:firmata:board`), packaged for the Viam registry and shipped from the same repo as the PoC.

**Architecture:** Additive. `internal/firmata.Client` gains one new accessor (`ReadDigital`) plus an `RWMutex` around `portState` so an external caller can safely read the cached input state. A new repo-root package `firmataboard` (file `firmata_board.go`) owns the Viam resource concerns: config parsing, lifecycle, per-pin mode bookkeeping, and the digital-GPIO-only Viam `board.Board` + `board.GPIOPin` implementations. Unimplemented board/PWM/analog/interrupt methods return a single `errUnimplemented` sentinel. A new `cmd/module/main.go` is the registry binary entrypoint; the existing `cmd/firmata-poc` stays as a hardware smoke test.

See the design at `docs/superpowers/specs/2026-04-21-viam-firmata-board-module-design.md`.

**Tech Stack:**
- Go 1.22+ (go.mod already says 1.26.2; keep it)
- `go.viam.com/rdk` — board/resource/module/logging packages
- `go.viam.com/api/component/board/v1` — protobuf `PowerMode` enum
- `go.bug.st/serial` — already a direct dep; now used by both the PoC and the module binary
- `go test` with `-race` for all new code

**Go module path change:** `github.com/viam-labs/viam-firmata` → `github.com/viam-devrel/viam-firmata` (done in Task 1 before anything else).

**Design anchors (read before starting):**
- Viam `board.Board` interface surface (see the design spec §4.2 and §4.3 for the exact methods). The current v1 target is digital GPIO only; every other board method and every `GPIOPin` PWM method returns `errUnimplemented`.
- `resource.AlwaysRebuild` is an embeddable helper from `go.viam.com/rdk/resource` that causes any config change to tear down and reconstruct the resource. We use it because reopening the serial port is the only sane response to a config change and the pin-mode cache is meaningless across reopens.
- `module.ModularMain(apiModels ...resource.APIModel)` is the correct entrypoint. Models are registered via `resource.RegisterComponent` in an `init()` in `firmata_board.go`.
- Tests drive the board directly over `io.Pipe` fakes, bypassing `serial.Open` / DTR / handshake. A same-package unexported `newBoardFromClient` helper is used by tests; `NewBoard` (exported) is the constructor registered with RDK.

**Target file layout when plan is complete:**

```
viam-firmata/
├── .github/
│   └── workflows/
│       └── deploy.yml                    # new
├── cmd/
│   ├── firmata-poc/main.go               # modified (import path only)
│   └── module/
│       └── main.go                       # new
├── internal/firmata/
│   ├── protocol.go                       # unchanged
│   ├── protocol_test.go                  # unchanged
│   ├── client.go                         # modified (stateMu + ReadDigital)
│   └── client_test.go                    # modified (ReadDigital tests)
├── firmata_board.go                      # new
├── firmata_board_test.go                 # new
├── go.mod                                # modified (module path + rdk dep)
├── go.sum                                # regenerated
├── Makefile                              # new
├── meta.json                             # new
├── build.sh                              # new (exec)
├── setup.sh                              # new (exec)
└── README.md                             # modified (add Viam module section)
```

**A note on unimplemented errors:** There is no standard Viam sentinel for "board method not supported" — the RDK's own non-GPIO boards return plain wrapped errors. We use a single package-level `errUnimplemented = errors.New("firmata board: method not implemented")`. Tests use `errors.Is` to assert on it.

---

## Task 1: Change Go module path and add RDK dependency

**Files:**
- Modify: `go.mod`
- Modify: `cmd/firmata-poc/main.go` (import path)

- [ ] **Step 1: Rewrite go.mod module path**

Edit `go.mod` line 1 from `module github.com/viam-labs/viam-firmata` to `module github.com/viam-devrel/viam-firmata`. Leave `go 1.26.2` alone.

- [ ] **Step 2: Update the PoC import to the new path**

In `cmd/firmata-poc/main.go`, change:

```go
"github.com/viam-labs/viam-firmata/internal/firmata"
```

to:

```go
"github.com/viam-devrel/viam-firmata/internal/firmata"
```

- [ ] **Step 3: Add the RDK dependency**

```bash
go get go.viam.com/rdk@latest
go mod tidy
```

Expected: `go.mod` now has a `require go.viam.com/rdk vX.Y.Z` line and a large indirect-dependency block. `go.sum` updated.

- [ ] **Step 4: Verify everything still builds**

```bash
go build ./...
go test ./...
```

Expected: both succeed. No functional changes yet.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum cmd/firmata-poc/main.go
git commit -m "chore: rename go module to github.com/viam-devrel/viam-firmata and add rdk"
```

---

## Task 2: Add `ReadDigital` accessor to the firmata client (TDD)

**Files:**
- Modify: `internal/firmata/client.go`
- Modify: `internal/firmata/client_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/firmata/client_test.go` (below the existing tests, keep the existing imports):

```go
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
```

If `rwAdapter` does not already exist in `client_test.go`, add this helper at the bottom of the file:

```go
type rwAdapter struct {
    r io.Reader
    w io.WriteCloser
}

func (a *rwAdapter) Read(p []byte) (int, error)  { return a.r.Read(p) }
func (a *rwAdapter) Write(p []byte) (int, error) { return a.w.Write(p) }
func (a *rwAdapter) Close() error                { return a.w.Close() }
```

Before adding, grep the existing test file:

```bash
grep -n "rwAdapter\|type.*io.ReadWriteCloser\|func.*Read.*p \[\]byte" internal/firmata/client_test.go
```

If an equivalent helper already exists under a different name, use that name in the test above instead.

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/firmata/ -run TestReadDigital -v
```

Expected: FAIL with `c.ReadDigital undefined`.

- [ ] **Step 3: Add the `stateMu` field and the `ReadDigital` method**

In `internal/firmata/client.go`:

a) Add a new field to the `Client` struct (alongside `writeMu`):

```go
stateMu   sync.RWMutex       // guards portState for external readers
```

b) In `dispatchDigital`, wrap the read-modify-write of `c.portState` in a write lock. Replace the current body with:

```go
func (c *Client) dispatchDigital(m DigitalPortMessage) {
    c.stateMu.Lock()
    prev := c.portState[m.Port]
    c.portState[m.Port] = m.Mask
    c.stateMu.Unlock()

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
}
```

c) Add a new method at the end of the file:

```go
// ReadDigital returns the cached input level of a digital pin.
// Returns false if the pin is out of range [0, 127] or if no DIGITAL_MESSAGE
// has yet been received for the pin's port (i.e. reporting was never enabled
// or nothing has changed since the port was enabled).
func (c *Client) ReadDigital(pin int) bool {
    if pin < 0 || pin > 127 {
        return false
    }
    port := uint8(pin / 8)
    bit := uint8(pin % 8)
    c.stateMu.RLock()
    defer c.stateMu.RUnlock()
    return c.portState[port]&(1<<bit) != 0
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/firmata/ -run TestReadDigital -v
```

Expected: PASS.

- [ ] **Step 5: Run the full package under `-race`**

```bash
go test -race ./internal/firmata/
```

Expected: PASS. If the race detector complains, revisit the `dispatchDigital` locking.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/client.go internal/firmata/client_test.go
git commit -m "feat(firmata): add ReadDigital accessor guarded by RWMutex"
```

---

## Task 3: Scaffold the `firmataboard` package with Config and Validate (TDD)

**Files:**
- Create: `firmata_board.go`
- Create: `firmata_board_test.go`

- [ ] **Step 1: Verify the `resource.AlwaysRebuild` and `resource.NewConfigValidationFieldRequiredError` symbols exist in the installed RDK**

```bash
go doc go.viam.com/rdk/resource.AlwaysRebuild
go doc go.viam.com/rdk/resource.NewConfigValidationFieldRequiredError
```

Expected: both print non-empty docs. If either is missing, the RDK version is too old — rerun `go get go.viam.com/rdk@latest` and re-verify. If `AlwaysRebuild` is absent even on latest, substitute a method: `func (b *firmataBoard) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error { return resource.ErrReconfigureNotImplemented /* or equivalent rebuild-signaling sentinel */ }` — but `AlwaysRebuild` is the preferred form.

- [ ] **Step 2: Write the failing test for Config.Validate**

Create `firmata_board_test.go`:

```go
package firmataboard

import (
    "errors"
    "testing"
    "time"
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
```

- [ ] **Step 3: Run to verify it fails**

```bash
go test . -run TestConfig_Validate -v
```

Expected: FAIL — `firmata_board.go` does not yet exist.

- [ ] **Step 4: Create `firmata_board.go` with package, Model, Config, Validate**

```go
// Package firmataboard provides a Viam board component backed by a device
// running ConfigurableFirmata over USB serial. See
// docs/superpowers/specs/2026-04-21-viam-firmata-board-module-design.md.
package firmataboard

import (
    "errors"
    "fmt"
    "time"

    "go.viam.com/rdk/resource"
)

// Model identifies this board implementation in the Viam registry.
var Model = resource.NewModel("devrel", "firmata", "board")

// errUnimplemented is returned by every board/GPIOPin method that is outside
// the v1 digital-GPIO-only scope. Tests use errors.Is to assert on it.
var errUnimplemented = errors.New("firmata board: method not implemented")

// Config is the attributes block for a devrel:firmata:board component.
type Config struct {
    SerialPath       string        `json:"serial_path"`
    BaudRate         int           `json:"baud_rate,omitempty"`
    AutoResetDelay   time.Duration `json:"auto_reset_delay,omitempty"`
    HandshakeTimeout time.Duration `json:"handshake_timeout,omitempty"`
}

// Validate checks required fields and reports that this board has no resource
// dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
    if c.SerialPath == "" {
        return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "serial_path")
    }
    if c.BaudRate < 0 {
        return nil, nil, resource.NewConfigValidationError(path,
            fmt.Errorf("baud_rate must be >= 0"))
    }
    return nil, nil, nil
}
```

- [ ] **Step 5: Run the test again**

```bash
go test . -run TestConfig_Validate -v
```

Expected: PASS on all four subtests.

- [ ] **Step 6: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "feat(board): add firmataboard package with Config + Validate"
```

---

## Task 4: Add a test-only `newBoardFromClient` helper and `firmataBoard` skeleton

**Files:**
- Modify: `firmata_board.go`

- [ ] **Step 1: Add struct, drain goroutine, and Close skeleton**

Append to `firmata_board.go`:

```go
import (
    // ... existing imports ...
    "context"
    "io"
    "sync"

    "go.viam.com/rdk/logging"

    "github.com/viam-devrel/viam-firmata/internal/firmata"
)

type firmataBoard struct {
    resource.Named
    resource.AlwaysRebuild

    logger logging.Logger

    closer io.Closer       // owns the underlying transport (real: serial port; tests: pipe)
    client *firmata.Client

    drainDone chan struct{}

    mu       sync.Mutex
    pinModes map[int]firmata.PinMode
}

// newBoardFromClient wires a *firmata.Client (and the io.Closer that owns the
// underlying transport) into a firmataBoard. Exposed at package scope so that
// firmata_board_test.go can drive the board over io.Pipe fakes without
// touching a real serial port.
func newBoardFromClient(name resource.Name, c *firmata.Client, closer io.Closer, logger logging.Logger) *firmataBoard {
    b := &firmataBoard{
        Named:     name.AsNamed(),
        logger:    logger,
        closer:    closer,
        client:    c,
        drainDone: make(chan struct{}),
        pinModes:  make(map[int]firmata.PinMode),
    }
    go b.drainEvents()
    return b
}

// drainEvents consumes and discards Client.Events() so the reader goroutine
// inside firmata.Client is never back-pressured. v1 does not expose events to
// Viam callers — all digital reads go through ReadDigital on the cached state.
func (b *firmataBoard) drainEvents() {
    defer close(b.drainDone)
    for range b.client.Events() {
    }
}

// Close tears down the client and the underlying transport and waits for the
// drain goroutine to exit. Idempotent.
func (b *firmataBoard) Close(_ context.Context) error {
    err := b.client.Close() // unblocks reader → closes events → drain exits
    <-b.drainDone
    if cerr := b.closer.Close(); err == nil {
        err = cerr
    }
    return err
}
```

Note on duplicate imports: if the package already has `import ( ... )` from Task 3, merge the new imports into the existing block rather than adding a second `import` statement.

- [ ] **Step 2: Verify it compiles**

```bash
go build ./...
```

Expected: success. No tests yet for this new helper — next task covers it.

- [ ] **Step 3: Commit**

```bash
git add firmata_board.go
git commit -m "feat(board): add firmataBoard struct and drain/Close lifecycle"
```

---

## Task 5: Implement `firmataGPIOPin.Set` with pin-mode caching (TDD)

**Files:**
- Modify: `firmata_board.go`
- Modify: `firmata_board_test.go`

- [ ] **Step 1: Write the failing test**

Append to `firmata_board_test.go`:

```go
import (
    // keep prior imports
    "bytes"
    "context"
    "io"
    "testing"
    "time"

    "go.viam.com/rdk/board"
    "go.viam.com/rdk/logging"
    "go.viam.com/rdk/resource"

    "github.com/viam-devrel/viam-firmata/internal/firmata"
)

// testBoard wires a firmataBoard to an in-process pair of pipes that stand
// in for an Arduino: everything the board writes ends up in sentBytes; any
// bytes we hand-craft into arduinoW arrive at the board's read loop.
type testBoard struct {
    b          *firmataBoard
    sentBuf    *bytes.Buffer // everything the board has ever written
    arduinoW   io.WriteCloser
    cleanup    func()
}

func newTestBoard(t *testing.T) *testBoard {
    t.Helper()
    // board reads ← arduinoR ... arduinoW (test writes here)
    arduinoR, arduinoW := io.Pipe()
    // board writes → sentBuf (test reads here via sentBuf.Bytes())
    sentBuf := &bytes.Buffer{}
    rw := &rwFake{r: arduinoR, w: sentBuf}

    c := firmata.New(rw)
    name := board.Named("test").AsNamed().Name()
    b := newBoardFromClient(name, c, rw, logging.NewTestLogger(t))

    return &testBoard{
        b:        b,
        sentBuf:  sentBuf,
        arduinoW: arduinoW,
        cleanup: func() {
            _ = b.Close(context.Background())
            _ = arduinoW.Close()
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
func (f *rwFake) Close() error                 { return nil }

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
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test . -run TestGPIOPin_Set -v
```

Expected: FAIL — `GPIOPinByName` undefined on `firmataBoard`.

- [ ] **Step 3: Implement `firmataGPIOPin` and `GPIOPinByName` for Set**

Append to `firmata_board.go`:

```go
import (
    // add:
    "strconv"
)

type firmataGPIOPin struct {
    board *firmataBoard
    pin   int
}

func (b *firmataBoard) GPIOPinByName(name string) (board.GPIOPin, error) {
    pin, err := strconv.Atoi(name)
    if err != nil || pin < 0 || pin > 127 {
        return nil, fmt.Errorf("firmata: invalid pin name %q (want decimal 0-127)", name)
    }
    return &firmataGPIOPin{board: b, pin: pin}, nil
}

// ensureMode sends SET_PIN_MODE only when the pin's cached mode differs from
// the requested one. For INPUT/INPUT_PULLUP it also enables per-port
// reporting on first configuration.
func (p *firmataGPIOPin) ensureMode(mode firmata.PinMode) error {
    p.board.mu.Lock()
    defer p.board.mu.Unlock()
    if current, ok := p.board.pinModes[p.pin]; ok && current == mode {
        return nil
    }
    if err := p.board.client.SetPinMode(p.pin, mode); err != nil {
        return err
    }
    p.board.pinModes[p.pin] = mode
    if mode == firmata.PinModeInput || mode == firmata.PinModeInputPullup {
        if err := p.board.client.EnableDigitalReporting(p.pin/8, true); err != nil {
            return err
        }
    }
    return nil
}

func (p *firmataGPIOPin) Set(_ context.Context, high bool, _ map[string]any) error {
    if err := p.ensureMode(firmata.PinModeOutput); err != nil {
        return err
    }
    return p.board.client.DigitalWrite(p.pin, high)
}
```

Don't forget to add `"go.viam.com/rdk/components/board"` to the import block. (The correct package path is `components/board`, not `board`.)

**Important:** The test above imports `"go.viam.com/rdk/board"` for brevity in the snippet but the actual RDK package path is `go.viam.com/rdk/components/board`. Verify and adjust:

```bash
go doc go.viam.com/rdk/components/board.Named
```

If that prints docs, use `"go.viam.com/rdk/components/board"` in both the test and the implementation. Update the test import accordingly before running.

- [ ] **Step 4: Run the test**

```bash
go test . -run TestGPIOPin_Set -v
```

Expected: PASS. If the wire bytes differ, re-check the pin 13 port/bit math: pin 13 = port 1, bit 5, mask `1<<5 = 0x20`.

- [ ] **Step 5: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "feat(board): implement GPIOPinByName + Set with pin-mode cache"
```

---

## Task 6: Implement `firmataGPIOPin.Get` (TDD)

**Files:**
- Modify: `firmata_board.go`
- Modify: `firmata_board_test.go`

- [ ] **Step 1: Write the failing test**

Append to `firmata_board_test.go`:

```go
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
        0xD0, 0x01,       // REPORT_DIGITAL(port=0, enable)
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
    // poll ReadDigital for up to 1s.
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
```

- [ ] **Step 2: Run to verify it fails**

```bash
go test . -run TestGPIOPin_Get -v
```

Expected: FAIL — `Get` method does not exist on `*firmataGPIOPin`.

- [ ] **Step 3: Implement `Get`**

Append to `firmata_board.go` below the `Set` method:

```go
func (p *firmataGPIOPin) Get(_ context.Context, _ map[string]any) (bool, error) {
    if err := p.ensureMode(firmata.PinModeInputPullup); err != nil {
        return false, err
    }
    return p.board.client.ReadDigital(p.pin), nil
}
```

- [ ] **Step 4: Run the test**

```bash
go test . -run TestGPIOPin_Get -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "feat(board): implement GPIOPin.Get reading cached digital state"
```

---

## Task 7: Stub unimplemented GPIOPin and board methods (TDD)

**Files:**
- Modify: `firmata_board.go`
- Modify: `firmata_board_test.go`

- [ ] **Step 1: Write the failing test**

Append to `firmata_board_test.go`:

```go
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
```

Add `"errors"` to the test file's imports if it's not already there.

- [ ] **Step 2: Run to verify it fails**

```bash
go test . -run "TestUnimplementedMethods|TestGPIOPinByName_RejectsInvalid" -v
```

Expected: FAIL — `PWM`, `AnalogByName`, `DigitalInterruptByName` etc. are undefined.

- [ ] **Step 3: Implement the stubs**

Append to `firmata_board.go`:

```go
// --- GPIOPin PWM family: unimplemented in v1 ---

func (p *firmataGPIOPin) PWM(context.Context, map[string]any) (float64, error) {
    return 0, errUnimplemented
}
func (p *firmataGPIOPin) SetPWM(context.Context, float64, map[string]any) error {
    return errUnimplemented
}
func (p *firmataGPIOPin) PWMFreq(context.Context, map[string]any) (uint, error) {
    return 0, errUnimplemented
}
func (p *firmataGPIOPin) SetPWMFreq(context.Context, uint, map[string]any) error {
    return errUnimplemented
}

// --- Board-level methods outside the v1 digital-GPIO scope ---

func (b *firmataBoard) AnalogByName(string) (board.Analog, error) {
    return nil, errUnimplemented
}

func (b *firmataBoard) DigitalInterruptByName(string) (board.DigitalInterrupt, error) {
    return nil, errUnimplemented
}
```

For `SetPowerMode` and `StreamTicks` we need the `boardpb` import for the `PowerMode` enum type.

Verify the protobuf package path in the installed RDK:

```bash
go doc go.viam.com/api/component/board/v1.PowerMode
```

Expected: non-empty docs. Then add to the imports:

```go
import (
    // ...
    pb "go.viam.com/api/component/board/v1"
)
```

Add:

```go
func (b *firmataBoard) SetPowerMode(_ context.Context, _ pb.PowerMode, _ *time.Duration, _ map[string]any) error {
    return errUnimplemented
}

func (b *firmataBoard) StreamTicks(_ context.Context, _ []board.DigitalInterrupt, _ chan board.Tick, _ map[string]any) error {
    return errUnimplemented
}
```

- [ ] **Step 4: Verify the compiler confirms interface satisfaction**

Add this package-level assertion at the bottom of `firmata_board.go`:

```go
var _ board.Board = (*firmataBoard)(nil)
var _ board.GPIOPin = (*firmataGPIOPin)(nil)
```

Then:

```bash
go build ./...
```

Expected: success. If it fails with "missing method" errors, the surface has drifted from what the design spec documents — add the missing stubs returning `errUnimplemented` (or zero values + nil for *Names methods that expect slices).

- [ ] **Step 5: Run the tests**

```bash
go test . -run "TestUnimplementedMethods|TestGPIOPinByName_RejectsInvalid" -v
go test -race .
```

Expected: PASS on both.

- [ ] **Step 6: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "feat(board): stub unimplemented board + GPIOPin methods"
```

---

## Task 8: Implement the exported `NewBoard` constructor

**Files:**
- Modify: `firmata_board.go`

- [ ] **Step 1: Write the constructor**

Append to `firmata_board.go`:

```go
import (
    // add:
    "go.bug.st/serial"
)

// NewBoard is the constructor registered with the Viam resource manager.
// It opens the serial port, toggles DTR to reset the Arduino, waits for the
// auto-reset window, runs the Firmata handshake, and returns a live board.
func NewBoard(ctx context.Context, _ resource.Dependencies, conf resource.Config, logger logging.Logger) (board.Board, error) {
    cfg, err := resource.NativeConfig[*Config](conf)
    if err != nil {
        return nil, err
    }

    baud := cfg.BaudRate
    if baud == 0 {
        baud = 57600
    }
    resetDelay := cfg.AutoResetDelay
    if resetDelay == 0 {
        resetDelay = 2 * time.Second
    }
    hsTimeout := cfg.HandshakeTimeout
    if hsTimeout == 0 {
        hsTimeout = 5 * time.Second
    }

    sp, err := serial.Open(cfg.SerialPath, &serial.Mode{BaudRate: baud})
    if err != nil {
        return nil, fmt.Errorf("open %s: %w", cfg.SerialPath, err)
    }

    _ = sp.SetDTR(false)
    time.Sleep(100 * time.Millisecond)
    _ = sp.SetDTR(true)
    logger.Infof("firmata: waiting %s for auto-reset on %s", resetDelay, cfg.SerialPath)
    select {
    case <-time.After(resetDelay):
    case <-ctx.Done():
        _ = sp.Close()
        return nil, ctx.Err()
    }

    c := firmata.New(sp)

    hsCtx, cancel := context.WithTimeout(ctx, hsTimeout)
    major, minor, err := c.Handshake(hsCtx)
    cancel()
    if err != nil {
        _ = c.Close()
        _ = sp.Close()
        return nil, fmt.Errorf("handshake on %s: %w", cfg.SerialPath, err)
    }
    logger.Infof("firmata: connected to %s — firmware v%d.%d", cfg.SerialPath, major, minor)

    return newBoardFromClient(conf.ResourceName(), c, sp, logger), nil
}
```

- [ ] **Step 2: Register the component in `init()`**

Append to `firmata_board.go`:

```go
func init() {
    resource.RegisterComponent(
        board.API, Model,
        resource.Registration[board.Board, *Config]{
            Constructor: NewBoard,
        },
    )
}
```

- [ ] **Step 3: Verify it compiles and all tests still pass**

```bash
go build ./...
go test -race ./...
```

Expected: PASS. The constructor has no unit test — exercising `serial.Open` + DTR + sleep would require hardware or a very elaborate fake. It's covered by the manual smoke test documented in the README.

- [ ] **Step 4: Commit**

```bash
git add firmata_board.go
git commit -m "feat(board): add NewBoard constructor and register the model"
```

---

## Task 9: Stream-error surfacing test (TDD for an already-present property)

**Files:**
- Modify: `firmata_board_test.go`

- [ ] **Step 1: Write the test**

Append to `firmata_board_test.go`:

```go
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
```

- [ ] **Step 2: Run it**

```bash
go test . -run TestSet_AfterStreamClose -v -race
```

Expected: PASS. If it hangs or always gets nil, the reader goroutine in `firmata.Client` may not be surfacing `io.EOF` — revisit `readLoop` in `internal/firmata/client.go`.

- [ ] **Step 3: Commit**

```bash
git add firmata_board_test.go
git commit -m "test(board): surface stream-close errors via Set"
```

---

## Task 10: Module binary entrypoint

**Files:**
- Create: `cmd/module/main.go`

- [ ] **Step 1: Create the entrypoint**

```go
// Command viam-firmata is the module binary for the devrel:firmata registry
// module. It registers the devrel:firmata:board model via the side-effect
// import of the firmataboard package and hands control to module.ModularMain.
package main

import (
    "go.viam.com/rdk/components/board"
    "go.viam.com/rdk/module"
    "go.viam.com/rdk/resource"

    firmataboard "github.com/viam-devrel/viam-firmata"
)

func main() {
    module.ModularMain(resource.APIModel{API: board.API, Model: firmataboard.Model})
}
```

- [ ] **Step 2: Verify it builds**

```bash
go build ./cmd/module/
```

Expected: produces a `module` binary in the cwd (or no output with no error — `go build` by default only checks).

- [ ] **Step 3: Commit**

```bash
git add cmd/module/main.go
git commit -m "feat(module): add cmd/module entrypoint registering devrel:firmata:board"
```

---

## Task 11: Module packaging — Makefile, build.sh, setup.sh, meta.json

**Files:**
- Create: `Makefile`
- Create: `build.sh`
- Create: `setup.sh`
- Create: `meta.json`

- [ ] **Step 1: Write the Makefile**

```makefile
GO ?= go
BINARY := bin/viam-firmata
ENTRYPOINT := ./cmd/module

.PHONY: all build module.tar.gz test clean

all: build

build:
	mkdir -p bin
	$(GO) build -o $(BINARY) $(ENTRYPOINT)

module.tar.gz: build
	tar -czf module.tar.gz $(BINARY)

test:
	$(GO) test -race ./...

clean:
	rm -rf bin module.tar.gz
```

- [ ] **Step 2: Write `build.sh`**

```bash
#!/usr/bin/env bash
set -euo pipefail
make module.tar.gz
```

Then make it executable:

```bash
chmod +x build.sh
```

- [ ] **Step 3: Write `setup.sh`**

```bash
#!/usr/bin/env bash
# No native dependencies — pure Go build.
# Kept as a no-op for forward compatibility and cloud-build plumbing.
set -euo pipefail
exit 0
```

Then:

```bash
chmod +x setup.sh
```

- [ ] **Step 4: Write `meta.json`**

```json
{
  "$schema": "https://dl.viam.dev/module.schema.json",
  "module_id": "devrel:firmata",
  "visibility": "public",
  "url": "https://github.com/viam-devrel/viam-firmata",
  "description": "Firmata-over-serial board component for Arduino / ConfigurableFirmata devices.",
  "models": [
    {
      "api": "rdk:component:board",
      "model": "devrel:firmata:board",
      "short_description": "Digital GPIO via ConfigurableFirmata over USB serial."
    }
  ],
  "entrypoint": "bin/viam-firmata",
  "build": {
    "setup": "./setup.sh",
    "build": "./build.sh",
    "path": "module.tar.gz",
    "arch": ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  },
  "markdown_link": "README.md"
}
```

- [ ] **Step 5: Build locally to verify**

```bash
make clean
make module.tar.gz
tar -tzf module.tar.gz
```

Expected: the tarball contains exactly `bin/viam-firmata`.

- [ ] **Step 6: Add `bin/` and `module.tar.gz` to `.gitignore`**

Open `.gitignore` and append:

```
bin/
module.tar.gz
```

- [ ] **Step 7: Commit**

```bash
git add Makefile build.sh setup.sh meta.json .gitignore
git commit -m "build(module): add meta.json, Makefile, and build/setup scripts"
```

---

## Task 12: GitHub Actions cloud-build workflow

**Files:**
- Create: `.github/workflows/deploy.yml`

- [ ] **Step 1: Write the workflow**

```yaml
name: Module cloud build and upload

on:
  push:
    tags:
      - "v*"

jobs:
  upload:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: viamrobotics/build-action@v1
        with:
          version: ${{ github.ref_name }}
          ref: ${{ github.sha }}
          key-id: ${{ secrets.VIAM_KEY_ID }}
          key-value: ${{ secrets.VIAM_KEY }}
          token: ${{ secrets.GITHUB_TOKEN }}
```

- [ ] **Step 2: Verify the YAML parses**

```bash
python3 -c 'import yaml,sys; yaml.safe_load(open(".github/workflows/deploy.yml"))' \
  && echo "yaml ok"
```

Expected: `yaml ok`.

- [ ] **Step 3: Commit**

```bash
git add .github/workflows/deploy.yml
git commit -m "ci: add cloud-build workflow for tagged releases"
```

---

## Task 13: README — "Using as a Viam module" section

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Append a new section after "Running the tests"**

Between `## Running the tests` and `## Design docs`, insert:

```markdown
## Using as a Viam module

This repo also ships a Viam `board` component module that lets a machine
running `viam-server` drive digital GPIO over the same Firmata connection.

**Prerequisite:** flash ConfigurableFirmata as described above. The same
hardware that runs `firmata-poc` is what `viam-server` talks to.

**Build the module binary:**

```sh
make build
# produces ./bin/viam-firmata
```

**Local machine config snippet:** (replace the executable path with your
absolute path, and `serial_path` with the port from `arduino-cli board list`)

```json
{
  "modules": [
    {
      "name": "firmata",
      "type": "local",
      "executable_path": "/absolute/path/to/viam-firmata/bin/viam-firmata"
    }
  ],
  "components": [
    {
      "name": "my-firmata-board",
      "api": "rdk:component:board",
      "model": "devrel:firmata:board",
      "attributes": {
        "serial_path": "/dev/tty.usbmodem14201",
        "baud_rate": 57600
      }
    }
  ]
}
```

**Scope (v1):** digital pins only — `GPIOPinByName(name).Set/Get`. PWM,
analog, and digital-interrupt methods return an "unimplemented" error.

**Registry install** (after the first tagged release is cloud-built and
uploaded) — replace the local module stanza with:

```json
{
  "modules": [
    {
      "name": "firmata",
      "type": "registry",
      "module_id": "devrel:firmata"
    }
  ]
}
```
```

- [ ] **Step 2: Preview the rendered section**

```bash
sed -n '/^## Using as a Viam module/,/^## /p' README.md
```

Expected: the section prints cleanly, ending at the next `##` heading.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document using viam-firmata as a Viam module"
```

---

## Task 14: Final verification sweep

**Files:** none modified; pure verification.

- [ ] **Step 1: Build everything one more time**

```bash
go build ./...
```

Expected: success.

- [ ] **Step 2: Run the full test suite with race detection**

```bash
go test -race ./...
```

Expected: all tests pass.

- [ ] **Step 3: Lint/vet**

```bash
go vet ./...
```

Expected: no output (no warnings).

- [ ] **Step 4: Confirm the module tarball still builds end-to-end**

```bash
make clean
make module.tar.gz
tar -tzf module.tar.gz
```

Expected: `bin/viam-firmata` is the sole entry.

- [ ] **Step 5: Confirm the PoC still runs (hardware optional)**

If hardware is plugged in:

```bash
go run ./cmd/firmata-poc -port /dev/tty.usbmodem14201 -duration 3s
```

Expected: prints `connected — firmware Firmata vX.Y` and, if a button is wired, pin-change lines.

If no hardware: just `go build ./cmd/firmata-poc` to confirm it still compiles against the new import path.

- [ ] **Step 6: No commit for this task.** Push the branch and open a PR.
