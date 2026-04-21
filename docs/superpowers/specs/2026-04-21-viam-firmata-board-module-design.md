# viam-firmata Board Module — Design

**Date:** 2026-04-21
**Status:** Proposed (awaiting spec-reviewer pass + author sign-off)
**Author:** Nick Hehr

## 1. Goal & scope

Wrap the existing `internal/firmata` client (built in the PoC) as a Viam `board` component module so that a machine running `viam-server` can drive digital GPIO on an Arduino (or any ConfigurableFirmata-capable device) over USB serial. The module is packaged for the Viam registry and follows the standard `viam module generate` layout.

**In scope for v1:**

- Registry module `devrel:firmata`, exposing one model `devrel:firmata:board`.
- Digital GPIO read/write via the Viam `board` API:
  - `GPIOPinByName(name) (GPIOPin, error)` — returns a firmata-backed pin.
  - `GPIOPin.Set(ctx, high, _)` — writes digital output (lazy `SET_PIN_MODE(OUTPUT)` on first call).
  - `GPIOPin.Get(ctx, _)` — reads cached digital input state (lazy `SET_PIN_MODE(INPUT_PULLUP)` + `REPORT_DIGITAL` on first call).
- Config: implicit pin declaration. Only the serial port and a few transport parameters are in config; pin numbers/modes are determined at call time.
- Cloud-build-ready `meta.json`, `Makefile`, `build.sh`, `setup.sh`, and `.github/workflows/deploy.yml`, mirroring the output of `viam module generate` for a Go module.

**Explicit non-goals for v1:**

- Analog read/write, PWM, digital interrupts, `SetPowerMode`, `StreamTicks` (all return `errUnimplemented`).
- Auto-reconnection to the serial port after a runtime error (the user triggers `Reconfigure` or restart).
- Capability discovery via `CAPABILITY_QUERY` / `ANALOG_MAPPING_QUERY`.
- Named pin aliases in config.
- Multi-board / multi-port support from a single module instance (one module config → one serial port).

Package rename / path change: the Go module path moves from `github.com/viam-labs/viam-firmata` to `github.com/viam-devrel/viam-firmata`. All imports are updated in the same changeset.

## 2. Approach

Reuse `internal/firmata.Client` unchanged in spirit — only one narrow additive change (a `ReadDigital(pin int) bool` accessor backed by an `RWMutex` over the already-cached `portState`). All Viam-facing concerns (config parsing, lifecycle, pin-mode bookkeeping, unimplemented stubs) live in a new `firmata_board.go` at the repo root.

Rationale:

- The wire protocol and I/O loop are already correct and tested in `internal/firmata`. There is no need to duplicate that state machine inside the board implementation.
- Keeping mode bookkeeping (`map[int]firmata.PinMode`) inside the board package (rather than the codec) preserves `internal/firmata` as a general-purpose client that a future non-Viam consumer could reuse without pulling in RDK dependencies.
- The repository-root `firmata_board.go` placement matches what `viam module generate` produces; it keeps the module's public implementation flat and obvious.

## 3. Architecture

```
viam-firmata/
├── .github/
│   └── workflows/
│       └── deploy.yml            # cloud build + registry upload on tag
├── cmd/
│   ├── firmata-poc/
│   │   └── main.go               # existing PoC (import path updated)
│   └── module/
│       └── main.go               # module.ModularMain registering the board model
├── internal/
│   └── firmata/
│       ├── protocol.go           # unchanged
│       ├── protocol_test.go      # unchanged
│       ├── client.go             # + RWMutex around portState, + ReadDigital accessor
│       └── client_test.go        # + test for ReadDigital pre/post dispatch
├── firmata_board.go              # Config, New, firmataBoard, firmataGPIOPin
├── firmata_board_test.go         # pipe-based integration test for the resource
├── go.mod                        # module path → github.com/viam-devrel/viam-firmata
├── go.sum
├── Makefile                      # build, module.tar.gz targets
├── meta.json                     # registry metadata (devrel:firmata)
├── build.sh                      # cloud-build script
├── setup.sh                      # cloud-build setup dependencies
└── README.md                     # PoC docs + "Using as a Viam module" section
```

Dependencies added: `go.viam.com/rdk` (for `board`, `resource`, `module`, `logging`), `go.viam.com/api` (transitive, pinned by RDK). The module binary in `cmd/module` additionally depends on `go.bug.st/serial` for opening the port.

`internal/firmata` gains no new external dependencies.

## 4. Components

### 4.1 `firmata_board.go` — Config

```go
package firmataboard

import "time"

var Model = resource.NewModel("devrel", "firmata", "board")

type Config struct {
    SerialPath       string        `json:"serial_path"`
    BaudRate         int           `json:"baud_rate,omitempty"`         // default 57600
    AutoResetDelay   time.Duration `json:"auto_reset_delay,omitempty"`  // default 2s
    HandshakeTimeout time.Duration `json:"handshake_timeout,omitempty"` // default 5s
}

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

`time.Duration` fields decode from JSON strings like `"2s"` via `resource.TransformAttributeMap`, which honors `mapstructure`'s duration hook. No custom decoder needed.

No dependencies on other resources — a firmata board stands alone.

### 4.2 `firmata_board.go` — `firmataBoard`

```go
type firmataBoard struct {
    resource.Named
    resource.AlwaysRebuild   // Reconfigure → Close + Constructor; state is small
    // ^ Rationale: we have to reopen the serial port on any config change,
    //   and the pin-mode cache is meaningless across reopens anyway.

    logger logging.Logger

    port   serial.Port           // go.bug.st/serial
    client *firmata.Client
    drain  chan struct{}         // closes when the events-drain goroutine exits

    mu       sync.Mutex
    pinModes map[int]firmata.PinMode // last mode we sent to each pin
}
```

**Constructor:**

```go
func newBoard(ctx context.Context, _ resource.Dependencies,
    conf resource.Config, logger logging.Logger) (board.Board, error) {

    cfg, err := resource.NativeConfig[*Config](conf)
    if err != nil { return nil, err }
    // apply defaults
    baud := cfg.BaudRate;           if baud == 0           { baud = 57600 }
    resetDelay := cfg.AutoResetDelay; if resetDelay == 0    { resetDelay = 2 * time.Second }
    hsTimeout := cfg.HandshakeTimeout; if hsTimeout == 0   { hsTimeout = 5 * time.Second }

    sp, err := serial.Open(cfg.SerialPath, &serial.Mode{BaudRate: baud})
    if err != nil { return nil, fmt.Errorf("open %s: %w", cfg.SerialPath, err) }
    _ = sp.SetDTR(false); time.Sleep(100*time.Millisecond); _ = sp.SetDTR(true)
    logger.Infof("firmata: waiting %s for auto-reset on %s", resetDelay, cfg.SerialPath)
    time.Sleep(resetDelay)

    c := firmata.New(sp)
    hsCtx, cancel := context.WithTimeout(ctx, hsTimeout)
    major, minor, err := c.Handshake(hsCtx); cancel()
    if err != nil { _ = c.Close(); _ = sp.Close(); return nil, fmt.Errorf("handshake: %w", err) }
    logger.Infof("firmata: connected — firmware v%d.%d", major, minor)

    b := &firmataBoard{
        Named:    conf.ResourceName().AsNamed(),
        logger:   logger, port: sp, client: c,
        pinModes: make(map[int]firmata.PinMode),
        drain:    make(chan struct{}),
    }
    go b.drainEvents()
    return b, nil
}
```

**Event drain:** the firmata client's `Events()` channel is buffered (16); leaving it unread would eventually back-pressure the reader goroutine. We don't need events at the board level because `Get` pulls from the cached port state, so we discard them:

```go
func (b *firmataBoard) drainEvents() {
    defer close(b.drain)
    for range b.client.Events() {}
}
```

**Close:**

```go
func (b *firmataBoard) Close(_ context.Context) error {
    err := b.client.Close() // unblocks the reader → closes Events → drain exits
    <-b.drain
    if perr := b.port.Close(); err == nil { err = perr }
    return err
}
```

**Board API methods:**

```go
func (b *firmataBoard) GPIOPinByName(name string) (board.GPIOPin, error) {
    pin, err := strconv.Atoi(name)
    if err != nil || pin < 0 || pin > 127 {
        return nil, fmt.Errorf("firmata: invalid pin name %q (want decimal 0-127)", name)
    }
    return &firmataGPIOPin{board: b, pin: pin}, nil
}

func (b *firmataBoard) AnalogByName(string) (board.Analog, error) {
    return nil, errUnimplemented
}
func (b *firmataBoard) DigitalInterruptByName(string) (board.DigitalInterrupt, error) {
    return nil, errUnimplemented
}
func (b *firmataBoard) SetPowerMode(context.Context, boardpb.PowerMode, *time.Duration, map[string]any) error {
    return errUnimplemented
}
func (b *firmataBoard) StreamTicks(context.Context, []board.DigitalInterrupt,
    chan board.Tick, map[string]any) error {
    return errUnimplemented
}
```

Names-returning helpers (`AnalogNames`, `DigitalInterruptNames`) are not part of the current `board.Board` interface per the reference, so no method is needed. (If RDK adds them back, they return empty slices.)

### 4.3 `firmata_board.go` — `firmataGPIOPin`

```go
type firmataGPIOPin struct {
    board *firmataBoard
    pin   int
}

func (p *firmataGPIOPin) ensureMode(mode firmata.PinMode) error {
    p.board.mu.Lock()
    defer p.board.mu.Unlock()
    if p.board.pinModes[p.pin] == mode {
        return nil
    }
    if err := p.board.client.SetPinMode(p.pin, mode); err != nil {
        return err
    }
    p.board.pinModes[p.pin] = mode
    // If we just configured an input, also enable reporting for its port once.
    if mode == firmata.PinModeInput || mode == firmata.PinModeInputPullup {
        if err := p.board.client.EnableDigitalReporting(p.pin/8, true); err != nil {
            return err
        }
    }
    return nil
}

func (p *firmataGPIOPin) Set(_ context.Context, high bool, _ map[string]any) error {
    if err := p.ensureMode(firmata.PinModeOutput); err != nil { return err }
    return p.board.client.DigitalWrite(p.pin, high)
}

func (p *firmataGPIOPin) Get(_ context.Context, _ map[string]any) (bool, error) {
    if err := p.ensureMode(firmata.PinModeInputPullup); err != nil { return false, err }
    return p.board.client.ReadDigital(p.pin), nil
}

func (p *firmataGPIOPin) PWM(context.Context, map[string]any) (float64, error)             { return 0, errUnimplemented }
func (p *firmataGPIOPin) SetPWM(context.Context, float64, map[string]any) error            { return errUnimplemented }
func (p *firmataGPIOPin) PWMFreq(context.Context, map[string]any) (uint, error)            { return 0, errUnimplemented }
func (p *firmataGPIOPin) SetPWMFreq(context.Context, uint, map[string]any) error           { return errUnimplemented }
```

**Default `Get` mode is `INPUT_PULLUP`**, not plain `INPUT`. Rationale: Firmata-over-Arduino has no floating-pin protection; `INPUT_PULLUP` is safer by default and matches the PoC. Callers who explicitly want plain `INPUT` can extend via `extra` in a future iteration.

**`errUnimplemented`:** single package-level sentinel wrapping `resource.ErrDoCommandUnimplemented` style:

```go
var errUnimplemented = errors.New("firmata board: method not implemented")
```

### 4.4 `internal/firmata/client.go` changes

One narrow additive change:

```go
type Client struct {
    // ... existing fields ...
    stateMu   sync.RWMutex       // guards portState for external reads
}

// In dispatchDigital, take stateMu.Lock before updating portState.
// (Prior dispatchDigital wrote portState without a lock because only the
//  reader goroutine touched it; we now allow concurrent external reads.)

// ReadDigital returns the cached input state of a pin. Returns false if the
// pin is out of range or no DIGITAL_MESSAGE for its port has been received yet.
func (c *Client) ReadDigital(pin int) bool {
    if pin < 0 || pin > 127 { return false }
    port := uint8(pin / 8)
    bit := uint8(pin % 8)
    c.stateMu.RLock()
    defer c.stateMu.RUnlock()
    return c.portState[port] & (1 << bit) != 0
}
```

Existing call sites of `portState` in `dispatchDigital` are updated to take `stateMu.Lock()` around the read-modify-write. No new public API beyond `ReadDigital`.

### 4.5 `cmd/module/main.go`

```go
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

func init() {
    resource.RegisterComponent(board.API, firmataboard.Model,
        resource.Registration[board.Board, *firmataboard.Config]{
            Constructor: firmataboard.NewBoard,
        })
}
```

(The `RegisterComponent` call ends up in `firmata_board.go`'s `init()`; `main.go` only imports the package for its side effects and dispatches to `module.ModularMain`. The snippet above flattens both for illustration.)

### 4.6 `meta.json`

```json
{
  "$schema": "https://dl.viam.dev/module.schema.json",
  "module_id": "devrel:firmata",
  "visibility": "public",
  "url": "https://github.com/viam-devrel/viam-firmata",
  "description": "Firmata-over-serial board component for Arduino / ConfigurableFirmata devices",
  "models": [
    {
      "api": "rdk:component:board",
      "model": "devrel:firmata:board",
      "short_description": "Digital GPIO via ConfigurableFirmata over USB serial"
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

### 4.7 Makefile / build.sh / setup.sh

- **Makefile** targets: `build` (go build → `bin/viam-firmata`), `module.tar.gz` (tars the entrypoint), `test` (passes through), `clean`.
- **build.sh**: invoked by cloud build. Runs `make module.tar.gz`.
- **setup.sh**: no-op (`exit 0`) for now — pure Go, no native deps. Kept for forward compatibility.

### 4.8 `.github/workflows/deploy.yml`

On tag push (`v*`), use `viamrobotics/build-action@v1` to run `viam module build start`, wait for completion, and auto-upload to the registry. Requires repo secrets `VIAM_KEY_ID` + `VIAM_KEY`.

## 5. Data flow

```
  Machine config JSON
     │
     ▼  (viam-server starts module binary via cmd/module)
  module.ModularMain  ←──  RegisterComponent(board.API, devrel:firmata:board)
     │
     ▼
  Board Constructor:
     ├── serial.Open(serial_path, baud)
     ├── DTR toggle + sleep(auto_reset_delay)
     ├── firmata.New(port)
     ├── Handshake(handshake_timeout)
     ├── go drainEvents()    ← keeps client.Events() draining
     └── return *firmataBoard

  Caller (motion service, user SDK, web UI, etc.)
     │
     ▼
  board.GPIOPinByName("13")  → *firmataGPIOPin{pin:13}
     │
     ▼
  pin.Set(ctx, true)
     ├── ensureMode(OUTPUT)
     │       └── firmata.Client.SetPinMode(13, OUTPUT) (once)
     └── firmata.Client.DigitalWrite(13, true)
             └── writeMu-guarded write of encoded DIGITAL_MESSAGE

  pin.Get(ctx)
     ├── ensureMode(INPUT_PULLUP)
     │       ├── SetPinMode(pin, INPUT_PULLUP) (once)
     │       └── EnableDigitalReporting(pin/8, true) (once)
     └── firmata.Client.ReadDigital(pin)   ← cached, non-blocking
```

## 6. Error handling

| Scenario | Behavior |
|---|---|
| `Validate` fails (missing `serial_path`) | viam-server rejects the config; module never starts. |
| `serial.Open` fails (wrong path, perms) | Constructor returns error; module marks resource as failed; user sees it in logs. |
| `Handshake` times out | Constructor closes port + client, returns error. Includes the serial path and a "wrong port or wrong baud?" hint. |
| Runtime write error (cable unplugged mid-run) | `firmata.Client` caches the reader-side error; the next `Set`/`Get` call returns it wrapped. The board does **not** auto-reconnect in v1. User-triggered `Reconfigure` rebuilds the resource via `AlwaysRebuild`. |
| Caller requests unimplemented method (PWM, Analog, Interrupt, StreamTicks, SetPowerMode) | Returns `errUnimplemented`. No side effects. |
| Invalid pin name in `GPIOPinByName` | Returns an explanatory error (`invalid pin name "foo"`). |

## 7. Testing

### 7.1 `internal/firmata/client_test.go` (additive)

One new table entry in the existing dispatch test:

- After injecting a `DIGITAL_MESSAGE(port=0, mask=0x04)`, `client.ReadDigital(2)` returns `true` and `client.ReadDigital(1)` returns `false`.
- Before any frame is received, `client.ReadDigital(anything)` returns `false`.
- Concurrent `ReadDigital` callers and an active reader goroutine do not race under `go test -race`.

### 7.2 `firmata_board_test.go`

Uses the same `io.Pipe` pattern as the existing `client_test.go`:

```go
// Fake Arduino side writes to arduinoWrite (board reads it),
// Board side reads from arduinoRead (Arduino side writes to it).
```

A tiny helper constructs a `*firmataBoard` directly from a pair of pipes (bypassing `serial.Open` / DTR / handshake) so the tests don't depend on `go.bug.st/serial` or real hardware. This means the package exposes a test-only constructor, e.g.:

```go
// In firmata_board.go, behind a comment noting it's for tests:
func newFromClient(name resource.Name, c *firmata.Client, port io.Closer, logger logging.Logger) *firmataBoard { ... }
```

Covered cases:

1. **`Set(true)` on a new pin emits `SET_PIN_MODE(OUTPUT)` once**, then `DIGITAL_MESSAGE`. Second `Set(false)` emits only `DIGITAL_MESSAGE` (mode cached).
2. **`Get` on a new pin emits `SET_PIN_MODE(INPUT_PULLUP)` + `REPORT_DIGITAL(port, 1)`** once. Injecting a `DIGITAL_MESSAGE(port=0, mask=0x04)` from the fake Arduino makes a subsequent `Get` for pin 2 return `true`.
3. **Unimplemented methods return `errUnimplemented`** and emit zero bytes on the wire.
4. **Invalid pin names** are rejected by `GPIOPinByName`.
5. **`Close` unblocks the drain goroutine** and returns `nil` on a clean close.
6. **Stream-error surfacing:** closing the fake Arduino pipe mid-test causes the next `Set` to return a wrapped read error.

### 7.3 Manual hardware smoke-test

Documented in README: run `cmd/firmata-poc` first to confirm the wire is good, then start a local `viam-server` with a test config pointing at the freshly built module binary and call `GPIOPinByName("13").Set(...)` from the Viam app's control UI or via the Go SDK.

## 8. Deliverables checklist

1. `go.mod` — module path changed; `go.viam.com/rdk` added.
2. `cmd/firmata-poc/main.go` — import path updated.
3. `internal/firmata/client.go` — `stateMu` added, `ReadDigital` added.
4. `internal/firmata/client_test.go` — `ReadDigital` coverage, race-safe.
5. `firmata_board.go` — Config, `NewBoard`, `firmataBoard`, `firmataGPIOPin`, `init()` registration.
6. `firmata_board_test.go` — pipe-based integration tests.
7. `cmd/module/main.go` — `module.ModularMain` entry.
8. `meta.json`, `Makefile`, `build.sh`, `setup.sh` — module packaging.
9. `.github/workflows/deploy.yml` — CI.
10. `README.md` — new "Using as a Viam module" section with a sample machine config JSON and a note that the Arduino needs ConfigurableFirmata flashed (cross-link to the existing PoC flashing section).

## 9. Open questions

None blocking. Deliberately deferred:

- **PWM / analog / interrupts:** follow-up once the initial module is merged and the firmata client grows matching methods.
- **Auto-reconnect on serial drop:** deferred to v2; for now `AlwaysRebuild` + user-triggered reconfigure is sufficient.
- **Named pin aliases in config:** reconsider if users find the raw-integer pin names cumbersome.
- **Multi-port support:** each module instance owns one port; multiple Arduinos = multiple board resources in the same machine config.
