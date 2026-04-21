# viam-firmata PoC — Design

**Date:** 2026-04-21
**Status:** Approved (pending spec-reviewer pass + author sign-off)
**Author:** Nick Hehr

## 1. Goal & scope

Build a minimal Go proof-of-concept that communicates with an Arduino running ConfigurableFirmata over USB serial to demonstrate bidirectional GPIO control: setting a digital pin as `OUTPUT` and toggling it, while simultaneously reading pin-change events from a digital `INPUT_PULLUP` pin.

**Explicit non-goals for this PoC:**

- No Viam module integration (though the package boundary is drawn to leave that path open).
- No analog read/write, PWM, I2C, SPI, OneWire, stepper, or servo support.
- No `CAPABILITY_QUERY` / `ANALOG_MAPPING_QUERY` parsing.
- No auto-discovery of serial ports — the port path is a required CLI flag.
- No persistent state, reconnection logic, or long-lived daemon behavior.

## 2. Approach

We use a **hybrid** strategy: we implement the small subset of the Firmata wire protocol we need ourselves on top of a plain serial library, rather than adopting a full framework like Gobot.

Rationale:
- The base Firmata messages we need (`REPORT_VERSION`, `SET_PIN_MODE`, `DIGITAL_MESSAGE`, `REPORT_DIGITAL`) are trivially small to encode/decode — roughly seven message types worth of constants and a single-byte-at-a-time reader.
- Owning the codec keeps the dependency graph minimal (one serial lib, no robotics framework) and avoids conforming our API to another project's abstractions, which matters because the likely next step is exposing this as a Viam `board` component.
- It also gives us a pure, hardware-free unit-test surface for the wire protocol.

## 3. Architecture

Two units, one binary.

```
viam-firmata/
├── README.md                 # user-facing setup, including arduino-cli flashing
├── go.mod
├── docs/
│   └── superpowers/specs/    # this design doc lives here
├── cmd/
│   └── firmata-poc/
│       └── main.go           # thin: flags, serial open, Client wiring, demo loop
└── internal/
    └── firmata/
        ├── protocol.go       # wire-level constants + pure encode/decode
        ├── protocol_test.go  # byte-level table tests
        ├── client.go         # Client over io.ReadWriteCloser
        └── client_test.go    # fake-board tests via in-memory pipes
```

Package `internal/firmata` depends only on the Go standard library. The `go.bug.st/serial` dependency lives exclusively in `cmd/firmata-poc`. This keeps the codec hardware-agnostic and trivially reusable.

## 4. Components

### 4.1 `internal/firmata/protocol.go`

Pure functions and constants, no I/O.

**Constants (subset of the Firmata 2.x spec):**

| Name | Value | Purpose |
|---|---|---|
| `cmdDigitalMessage` | `0x90` | Per-port digital I/O (low nibble = port index) |
| `cmdReportDigital`  | `0xD0` | Enable/disable auto-reporting for a port |
| `cmdSetPinMode`     | `0xF4` | Set mode on a single pin |
| `cmdReportVersion`  | `0xF9` | Firmware protocol version (major/minor) |
| `cmdStartSysex`     | `0xF0` | Reserved (not used in PoC, but recognized + skipped) |
| `cmdEndSysex`       | `0xF7` | Reserved |
| `PinModeInput`       | `0x00` | — |
| `PinModeOutput`      | `0x01` | — |
| `PinModeInputPullup` | `0x0B` | — |

**Functions:**

- `encodePinMode(pin uint8, mode PinMode) []byte` — returns `[0xF4, pin, mode]`.
- `encodeDigitalPortWrite(port uint8, mask uint8) []byte` — returns `[0x90|port, mask&0x7F, (mask>>7)&0x01]`.
- `encodeReportDigital(port uint8, enable bool) []byte` — returns `[0xD0|port, enableByte]`.
- `decode(r *bufio.Reader) (Message, error)` — reads one complete frame. Returns a tagged-union type:
  - `VersionMessage{Major, Minor uint8}`
  - `DigitalPortMessage{Port, Mask uint8}`
  - `UnknownMessage{Cmd uint8, Payload []byte}` — sysex and unrecognized commands are consumed and returned as Unknown, not treated as errors.

**Resync policy:** if the decoder reads a byte that is not a recognized command (high bit set) or a plausible continuation byte in the middle of a frame, it discards bytes until it sees a byte with bit 7 set, then restarts frame decoding. This is logged to stderr at debug level but does not return an error, because Arduino serial streams on connect often contain bootloader noise.

### 4.2 `internal/firmata/client.go`

```go
type PinChange struct {
    Pin  int
    High bool
}

type Client struct {
    rw         io.ReadWriteCloser
    portState  [16]uint8          // last-known mask per port, for diffing
    reported   [16]uint8          // which pins are we reporting on (for diffing)
    events     chan PinChange
    version    chan struct{ Major, Minor uint8 }
    readerDone chan struct{}
    writeMu    sync.Mutex
}

func New(rw io.ReadWriteCloser) *Client
func (c *Client) Handshake(ctx context.Context) (major, minor uint8, err error)
func (c *Client) SetPinMode(pin int, mode PinMode) error
func (c *Client) DigitalWrite(pin int, high bool) error
func (c *Client) EnableDigitalReporting(port int, enable bool) error
func (c *Client) Events() <-chan PinChange
func (c *Client) Close() error
```

**Concurrency model:** `New` starts exactly one background reader goroutine that owns all reads from `rw`. It decodes frames and dispatches:
- `VersionMessage` → non-blocking send on `version` channel (Handshake consumes it).
- `DigitalPortMessage` → diff against `portState`, emit one `PinChange` per changed bit on `events`, then update `portState`.

Writes are serialized by `writeMu` so concurrent callers cannot interleave bytes.

`DigitalWrite` internally maps `pin` to `(port = pin/8, bit = pin%8)`, updates an output-side port mask, and sends `encodeDigitalPortWrite`. (The output mask is tracked separately from `portState`, which is input-side.)

`Close` closes `rw`, which unblocks the reader; the reader closes `events` on exit.

### 4.3 `cmd/firmata-poc/main.go`

CLI flags:

| Flag | Default | Purpose |
|---|---|---|
| `-port` | _(required)_ | Serial device path, e.g. `/dev/tty.usbmodem14201` |
| `-baud` | `57600` | ConfigurableFirmata default |
| `-out-pin` | `13` | Pin driven HIGH/LOW, typically onboard LED |
| `-in-pin` | `2` | Pin read as `INPUT_PULLUP` |
| `-duration` | `10s` | Total run time |
| `-toggle-interval` | `500ms` | How often to flip the output |

Execution sequence:
1. Parse flags; fail fast if `-port` is empty.
2. Open the serial port via `go.bug.st/serial.Open` with the given baud, 8N1.
3. Toggle DTR to trigger the Arduino auto-reset, then sleep 2s.
4. `client := firmata.New(port)`; `client.Handshake(ctx5s)`; log `firmware version M.m`.
5. `SetPinMode(outPin, OUTPUT)`; `SetPinMode(inPin, INPUT_PULLUP)`; `EnableDigitalReporting(inPin/8, true)`.
6. Start a `time.Ticker(toggleInterval)` that flips an `outHigh` bool and calls `DigitalWrite`.
7. Concurrently, `for ev := range client.Events() { log.Printf(...) }`.
8. When `-duration` elapses (via `context.WithTimeout`), stop the ticker, call `client.Close()`, wait for the events goroutine to exit, print a clean summary, exit 0.

## 5. Data flow

```
  Arduino
     │   (USB CDC serial)
     ▼
  go.bug.st/serial port  (io.ReadWriteCloser)
     │
     ▼
  firmata.Client
     ├── write path: SetPinMode / DigitalWrite / EnableDigitalReporting
     │       └── writeMu-guarded Write of encoded bytes
     │
     └── read path (goroutine): decode loop
             ├── VersionMessage   → version chan → Handshake
             ├── DigitalPortMessage → diff → PinChange → events chan
             └── UnknownMessage   → debug log, discard
                                                       │
                                                       ▼
                                                 main.go prints
```

## 6. Error handling

- **Wrong port / not Firmata:** `Handshake` blocks until either a version frame arrives or ctx expires; on timeout it returns an error wrapping the port path and a hint about `-baud` / wrong device.
- **Serial read errors:** reader goroutine exits, closes `events`. Next client write returns the error via the shared error field; main logs and exits non-zero.
- **Malformed frames:** resync policy above. Never fatal.
- **Short writes:** treated as errors from the caller's perspective; serial library is expected to return `n, err` semantics per `io.Writer`.
- **Context cancellation in main:** `Close()` is idempotent; the ticker and events loop both exit on channel close.

## 7. Testing

**Unit (no hardware):**
- `protocol_test.go` — table-driven tests for every `encode*` function and every `decode` branch, using hex literals straight from the Firmata protocol spec. E.g., `SET_PIN_MODE(13, OUTPUT)` must emit exactly `F4 0D 01`.
- `client_test.go` — wires a `Client` to a pair of `io.Pipe`s (fake Arduino on one side, client on the other). Covers:
  - Handshake succeeds when we push a version frame.
  - Handshake times out when we push nothing.
  - `EnableDigitalReporting` + a `DIGITAL_MESSAGE` produces the expected `PinChange`s (both edges).
  - Resync: a leading byte of noise followed by a valid frame is decoded correctly.
  - `Close` unblocks the reader and closes `events`.

**Manual (hardware):**
- One real Arduino flashed with ConfigurableFirmata.
- LED + resistor on the output pin, tactile button to GND on the input pin (pull-up internal).
- Expected: LED blinks at `-toggle-interval`, button presses print `pin 2 = LOW` / `pin 2 = HIGH` lines.

## 8. README.md (deliverable)

The repo ships a `README.md` at the project root that covers, in order:

1. **What this is** — one paragraph.
2. **Hardware prerequisites** — Arduino Uno (or any AVR board; Uno is the tested target), USB cable, optional LED + button for the full demo.
3. **Software prerequisites** — Go 1.22+, `arduino-cli` installed (`brew install arduino-cli` on macOS).
4. **Flashing ConfigurableFirmata via `arduino-cli`**, as an explicit command sequence:

   ```sh
   # One-time setup
   arduino-cli core update-index
   arduino-cli core install arduino:avr
   arduino-cli lib install ConfigurableFirmata

   # Find your board
   arduino-cli board list
   # → note the port (e.g. /dev/tty.usbmodem14201) and FQBN (e.g. arduino:avr:uno)

   # Compile and upload the stock ConfigurableFirmata example
   SKETCH="$HOME/Documents/Arduino/libraries/ConfigurableFirmata/examples/ConfigurableFirmata"
   arduino-cli compile --fqbn arduino:avr:uno "$SKETCH"
   arduino-cli upload  --fqbn arduino:avr:uno --port /dev/tty.usbmodem14201 "$SKETCH"
   ```

   A note will call out that the example sketch path differs on Linux (`~/Arduino/libraries/...`) and Windows (`Documents\Arduino\libraries\...`), and that `arduino-cli config init` may be needed on first use.

5. **Running the Go PoC:**

   ```sh
   go run ./cmd/firmata-poc -port /dev/tty.usbmodem14201
   # or with custom pins:
   go run ./cmd/firmata-poc -port /dev/tty.usbmodem14201 -out-pin 13 -in-pin 2 -duration 15s
   ```

6. **Expected output** — a short annotated example transcript showing the version line and a few `PinChange` log lines.
7. **Troubleshooting** — "handshake timeout" → wrong port or wrong baud; `permission denied` on `/dev/tty.*` → user not in `dialout`/`uucp` group on Linux; LED doesn't blink → board needs auto-reset, try unplugging/replugging.

## 9. Open questions

None blocking. The PoC can proceed. Things deliberately deferred:

- Whether `internal/firmata` should later move to `pkg/firmata` for external reuse (deferred until there's a second consumer).
- Whether to add a `-list-ports` convenience flag (deferred; `arduino-cli board list` already does this well enough for the PoC).
