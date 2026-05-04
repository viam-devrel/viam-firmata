# viam-firmata Board Module — Analog + PWM Design

**Date:** 2026-04-28
**Status:** Proposed (awaiting spec-reviewer pass + author sign-off)
**Author:** Nick Hehr
**Builds on:** [v1 board module spec](2026-04-21-viam-firmata-board-module-design.md)
**Followed by:** Digital interrupts spec (separate, forthcoming)

## 1. Goal & scope

Extend the `devrel:firmata:board` module to implement the analog-read and PWM portions of the Viam `board` API, leaving digital interrupts for a separate follow-up spec.

**In scope:**

- **PWM:** `GPIOPin.PWM` (cached duty cycle getter) and `GPIOPin.SetPWM` (lazy `SET_PIN_MODE(PWM)` then `ANALOG_MESSAGE` for pin ≤15 / `EXTENDED_ANALOG` sysex for pin ≥16). 8-bit duty cycle resolution.
- **Analog:** `Board.AnalogByName(name)` returns a Firmata-backed analog reader. Lazy enable on first `Read` (`SET_PIN_MODE(ANALOG)` + `REPORT_ANALOG(channel, true)`); subsequent reads return the cached 10-bit value. Optional board-level `sampling_interval_ms` sent once at handshake via the `SAMPLING_INTERVAL` sysex.
- **Capability discovery:** `CAPABILITY_QUERY` (0x6B) + `ANALOG_MAPPING_QUERY` (0x69) issued once at handshake; the responses are decoded into `(pin → supported modes)` and `(digitalPin ↔ analogChannel)` tables, used to validate config and translate pin names.
- **Config schema additions:** `analogs: []AnalogReaderConfig{name, pin, ...}`. Pin form accepts `"A0"`-style aliases or raw digital-pin numbers; both resolve to the same `(digitalPin, analogChannel)` record. Per-pin `samples_per_sec` is accepted-but-warned-and-ignored (Firmata only supports a global rate).
- **Pin-ownership tracking:** validate-time rejection of pins declared in multiple `analogs[]` entries; runtime `GPIOPinByName(...).Set/Get/SetPWM` on a declared analog pin returns a clear error.

**Explicit non-goals:**

- `PWMFreq`/`SetPWMFreq` remain `errUnimplemented` — Firmata has no spec for runtime PWM-frequency control. Documented in README.
- `DigitalInterruptByName` and `StreamTicks` continue to return `errUnimplemented` — covered in the next spec.
- No auto-reconnection on serial drop (still v1's `AlwaysRebuild` model).
- No hardware-gated CI; coverage stays pipe-based + codec unit tests.
- No 14-bit PWM resolution; we ship 8-bit because `dutyCyclePct * 255` is what the Viam `GPIOPin.SetPWM` interface implies.

## 2. Approach

Three layers, each kept narrow:

### 2.1 `internal/firmata` (codec + client) gains

- New sysex constants and decoders for `CAPABILITY_RESPONSE` (0x6C), `ANALOG_MAPPING_RESPONSE` (0x6A), and `ANALOG_MESSAGE` (0xE0) input frames.
- New encoders: `EXTENDED_ANALOG` (sysex 0x6F), `REPORT_ANALOG` (0xC0 + channel), `SAMPLING_INTERVAL` (sysex 0x7A), `CAPABILITY_QUERY` (sysex 0x6B), `ANALOG_MAPPING_QUERY` (sysex 0x69).
- New typed message structs: `CapabilityResponse{ Pins []PinCapabilities }` (where `PinCapabilities = map[PinMode]uint8 /* resolution bits */`), `AnalogMappingResponse{ ChannelByPin []uint8 }`, `AnalogMessage{ Channel uint8, Value uint16 }`.
- New cached state on `Client`: `analogState [16]uint16` guarded by `stateMu`, plus `ReadAnalog(channel int) uint16` accessor mirroring `ReadDigital`.
- New blocking handshake helpers: `QueryCapabilities(ctx)` and `QueryAnalogMapping(ctx)` that send the query, wait for the matching sysex response on a per-query channel (same shape as the existing `version` channel), and return the decoded struct or `ctx.Err()`.

### 2.2 `firmata_board.go` (board impl) gains

- `Config.Analogs []board.AnalogReaderConfig`, `Config.SamplingIntervalMs int` (optional). `Validate` does string-level sanity (pin parses, names unique, no duplicate pin literals, sampling-interval in 0..16383); the deeper capability check happens in the constructor right after capability discovery, returning a config error from `NewBoard`.
- `firmataBoard` gains: `capabilities firmata.CapabilityResponse`, `analogMap firmata.AnalogMappingResponse`, `analogs map[string]*firmataAnalog` (built in constructor), `ownedPins map[int]string` (pin → owning analog name), and `pwmDuty map[int]float64` (last duty written per pin).
- `firmataAnalog` is a small struct implementing `board.Analog`. `Read` lazily sends `SET_PIN_MODE(ANALOG)` + `REPORT_ANALOG(channel, true)` once, then returns `client.ReadAnalog(channel)` formatted as `AnalogValue{ Value: int(raw), Min: 0, Max: 1023, StepSize: 5.0/1024 }`. `Write` returns `errUnimplemented` (Firmata has no analog DAC concept on standard boards).
- `firmataGPIOPin.SetPWM`/`PWM`: lazy `SET_PIN_MODE(PWM)` (rejected via the capability map if the pin isn't PWM-capable), then `client.AnalogWrite(pin, byte(duty*255))`. Cached duty cycle on the board-level `pwmDuty` map for the getter.
- Constructor flow: open serial → DTR reset → `Handshake` → `QueryCapabilities` → `QueryAnalogMapping` → optional `SetSamplingInterval` → resolve+validate `analogs[]` against capabilities → build `firmataAnalog`s → return board.

### 2.3 Rationale for the split

The `internal/firmata` package stays Viam-agnostic (a third-party Go consumer can use it without RDK), while all Viam-specific concerns — config schema, pin ownership, validation against capabilities — live in `firmata_board.go`. This mirrors what we did for digital GPIO in v1.

## 3. Components in detail

### 3.1 `internal/firmata/protocol.go` additions

```go
const (
    sysexAnalogMappingQuery    uint8 = 0x69
    sysexAnalogMappingResponse uint8 = 0x6A
    sysexCapabilityQuery       uint8 = 0x6B
    sysexCapabilityResponse    uint8 = 0x6C
    sysexExtendedAnalog        uint8 = 0x6F
    sysexSamplingInterval      uint8 = 0x7A
)

func encodeAnalogWrite(channel uint8, value uint16) []byte // ANALOG_MESSAGE for ch ≤15
func encodeExtendedAnalog(pin uint8, value uint16) []byte  // sysex for pin ≥16
func encodeReportAnalog(channel uint8, enable bool) []byte
func encodeSamplingInterval(intervalMs uint16) []byte      // sysex
func encodeCapabilityQuery() []byte                        // sysex, no args
func encodeAnalogMappingQuery() []byte                     // sysex, no args

type AnalogMessage struct {
    Channel uint8
    Value   uint16 // 14-bit on the wire; real boards send 10-bit ADC samples
}

type PinCapabilities map[PinMode]uint8 // mode → resolution in bits

type CapabilityResponse struct {
    Pins []PinCapabilities // index = pin number; nil for absent pins
}

type AnalogMappingResponse struct {
    // ChannelByPin[digitalPin] = analog channel, or 127 if not analog-capable.
    ChannelByPin []uint8
}
```

`decode` grows two new branches:

- `cmd&0xF0 == cmdAnalogMessage` returns `AnalogMessage` with the same two-7-bit-byte assembly used for digital-port masks.
- The existing `cmdStartSysex` branch is upgraded from "always returns `UnknownMessage`" to switching on the first sysex byte: `sysexCapabilityResponse` → decode into `CapabilityResponse`; `sysexAnalogMappingResponse` → decode into `AnalogMappingResponse`. Anything else still falls through to `UnknownMessage` (the existing fallback path remains as-is in `readLoop`'s switch), so unknown sysex is gracefully ignored.

### 3.2 `internal/firmata/client.go` additions

```go
type Client struct {
    // ... existing fields ...
    analogState  [16]uint16              // last-known value per analog channel
    capabilities chan CapabilityResponse // buffered 1, like version
    analogMap    chan AnalogMappingResponse
}
```

`readLoop` dispatch grows new cases:

- `AnalogMessage` updates `analogState[channel]` under `stateMu`.
- `CapabilityResponse` / `AnalogMappingResponse` are non-blocking-sent to their respective channels (matching the existing `version` pattern).

New public methods:

```go
func (c *Client) AnalogWrite(pin int, value uint16) error
// pin <16 → ANALOG_MESSAGE; pin ≥16 → EXTENDED_ANALOG sysex.
// value masked to the 14-bit Firmata range.

func (c *Client) EnableAnalogReporting(channel int, enable bool) error // parallels existing EnableDigitalReporting(port, enable); arg is an analog channel index, not a port
func (c *Client) SetSamplingInterval(intervalMs int) error              // 1..16383ms; clamped at boundaries
func (c *Client) ReadAnalog(channel int) uint16                         // mirrors ReadDigital

func (c *Client) QueryCapabilities(ctx context.Context) (CapabilityResponse, error)
func (c *Client) QueryAnalogMapping(ctx context.Context) (AnalogMappingResponse, error)
```

The two query methods follow the same shape as `Handshake`: send the query in a goroutine (so `io.Pipe` tests don't deadlock), then `select` on the response channel vs `ctx.Done()`.

### 3.3 `firmata_board.go` — Config additions

```go
type Config struct {
    SerialPath         string                     `json:"serial_path"`
    BaudRate           int                        `json:"baud_rate,omitempty"`
    AutoResetDelay     time.Duration              `json:"auto_reset_delay,omitempty"`
    HandshakeTimeout   time.Duration              `json:"handshake_timeout,omitempty"`
    SamplingIntervalMs int                        `json:"sampling_interval_ms,omitempty"` // 0 = leave firmware default
    Analogs            []board.AnalogReaderConfig `json:"analogs,omitempty"`
}

func (c *Config) Validate(path string) ([]string, []string, error) {
    // existing serial_path / baud_rate checks ...
    if c.SamplingIntervalMs < 0 || c.SamplingIntervalMs > 16383 {
        return nil, nil, resource.NewConfigValidationError(path,
            fmt.Errorf("sampling_interval_ms must be 0..16383"))
    }
    seenName := map[string]bool{}
    seenPin := map[string]bool{}
    for i, a := range c.Analogs {
        sub := fmt.Sprintf("%s.analogs.%d", path, i)
        if err := a.Validate(sub); err != nil {
            return nil, nil, err
        }
        if seenName[a.Name] {
            return nil, nil, resource.NewConfigValidationError(sub,
                fmt.Errorf("duplicate analog name %q", a.Name))
        }
        if seenPin[a.Pin] {
            return nil, nil, resource.NewConfigValidationError(sub,
                fmt.Errorf("pin %q declared in multiple analogs entries", a.Pin))
        }
        if _, _, err := parseAnalogPin(a.Pin); err != nil {
            return nil, nil, resource.NewConfigValidationError(sub, err)
        }
        seenName[a.Name] = true
        seenPin[a.Pin] = true
    }
    return nil, nil, nil
}

// parseAnalogPin accepts "A0".."A15" or a raw digital pin number "0".."127".
// The unknown side of the pair is returned as the sentinel -1 — resolveAnalogPin
// (called in the constructor against the analog-mapping response) fills it in.
// Capability validation against analog support also happens in the constructor.
//
//   "A3"  → (digitalPin: -1, analogChannel: 3)
//   "14"  → (digitalPin: 14, analogChannel: -1)
func parseAnalogPin(s string) (digitalPin int, analogChannel int, err error) { /* ... */ }
```

### 3.4 `firmata_board.go` — `firmataBoard` additions

```go
type firmataBoard struct {
    // ... existing fields ...
    capabilities firmata.CapabilityResponse
    analogMap    firmata.AnalogMappingResponse
    analogs      map[string]*firmataAnalog // by name
    ownedPins    map[int]string            // digital pin → owning analog name
    pwmDuty      map[int]float64           // last duty written per pin (guarded by mu)
}

type firmataAnalog struct {
    board      *firmataBoard
    name       string
    digitalPin int
    channel    uint8
    enableOnce sync.Once
    enableErr  atomic.Pointer[error]
}

func (a *firmataAnalog) Read(ctx context.Context, _ map[string]any) (board.AnalogValue, error) {
    a.enableOnce.Do(func() {
        if err := a.board.client.SetPinMode(a.digitalPin, firmata.PinModeAnalog); err != nil {
            a.enableErr.Store(&err)
            return
        }
        if err := a.board.client.EnableAnalogReporting(int(a.channel), true); err != nil {
            a.enableErr.Store(&err)
            return
        }
        a.board.mu.Lock()
        a.board.pinModes[a.digitalPin] = firmata.PinModeAnalog
        a.board.mu.Unlock()
    })
    // enableErr is set once inside enableOnce.Do and never cleared, so subsequent
    // Reads return the same cached error rather than retrying mid-traffic.
    // (See §5 error table.)
    if errp := a.enableErr.Load(); errp != nil {
        return board.AnalogValue{}, *errp
    }
    return board.AnalogValue{
        Value:    int(a.board.client.ReadAnalog(int(a.channel))),
        Min:      0,
        Max:      1023,
        StepSize: 5.0 / 1024,
    }, nil
}

func (a *firmataAnalog) Write(_ context.Context, _ int, _ map[string]any) error {
    return errUnimplemented
}

func (b *firmataBoard) AnalogByName(name string) (board.Analog, error) {
    a, ok := b.analogs[name]
    if !ok {
        return nil, fmt.Errorf("firmata board: no analog named %q", name)
    }
    return a, nil
}
```

PWM methods on `firmataGPIOPin`:

```go
func (p *firmataGPIOPin) SetPWM(_ context.Context, duty float64, _ map[string]any) error {
    duty, err := board.ValidatePWMDutyCycle(duty)
    if err != nil {
        return err
    }
    if !p.board.pinSupports(p.pin, firmata.PinModePWM) {
        return fmt.Errorf("firmata board: pin %d does not support PWM", p.pin)
    }
    if err := p.ensureMode(firmata.PinModePWM); err != nil {
        return err
    }
    if err := p.board.client.AnalogWrite(p.pin, uint16(duty*255)); err != nil {
        return err
    }
    p.board.mu.Lock()
    p.board.pwmDuty[p.pin] = duty
    p.board.mu.Unlock()
    return nil
}

func (p *firmataGPIOPin) PWM(_ context.Context, _ map[string]any) (float64, error) {
    p.board.mu.Lock()
    defer p.board.mu.Unlock()
    return p.board.pwmDuty[p.pin], nil
}

// PWMFreq / SetPWMFreq remain errUnimplemented — Firmata spec has no
// runtime PWM-frequency control.
```

Existing `Set`/`Get` grow an ownership check at the top:

```go
func (p *firmataGPIOPin) Set(...) error {
    if owner, taken := p.board.ownedPins[p.pin]; taken {
        return fmt.Errorf("firmata board: pin %d is owned by analog %q", p.pin, owner)
    }
    // ... existing body ...
}
```

`pinSupports` is a small helper that consults `b.capabilities.Pins[pin]` and returns whether the requested mode is in the map.

### 3.5 Constructor flow update

```go
// after Handshake succeeds:
caps, err := c.QueryCapabilities(hsCtx)
if err != nil { /* close + return */ }
amap, err := c.QueryAnalogMapping(hsCtx)
if err != nil { /* close + return */ }
if cfg.SamplingIntervalMs > 0 {
    if err := c.SetSamplingInterval(cfg.SamplingIntervalMs); err != nil {
        /* close + return */
    }
}

// Resolve and validate each analogs[] entry against caps/amap.
analogs := map[string]*firmataAnalog{}
owned   := map[int]string{}
for _, ac := range cfg.Analogs {
    digitalPin, channel, err := resolveAnalogPin(ac.Pin, amap)
    if err != nil { /* close + return */ }
    if !caps.Pins[digitalPin].supports(firmata.PinModeAnalog) {
        return nil, fmt.Errorf("analog %q: pin %d does not support analog mode", ac.Name, digitalPin)
    }
    if ac.SamplesPerSecond != 0 {
        logger.Warnf("firmata board: analog %q samples_per_sec ignored — Firmata only supports a global sampling_interval_ms", ac.Name)
    }
    analogs[ac.Name] = &firmataAnalog{
        board: b, name: ac.Name, digitalPin: digitalPin, channel: uint8(channel),
    }
    owned[digitalPin] = ac.Name
}
b.capabilities = caps
b.analogMap    = amap
b.analogs      = analogs
b.ownedPins    = owned
b.pwmDuty      = map[int]float64{}
```

`resolveAnalogPin` takes the parsed form (digital-pin or analog-channel) plus the analog-mapping response and returns both the digital pin number and the analog channel for the entry. If the user supplied `"A0"`, we look up the digital pin via `ChannelByPin`; if they supplied `"14"`, we look up the analog channel via the same map. Either form fails fast if the pin isn't analog-capable.

## 4. Data flow

```
Constructor:
  serial.Open → DTR reset → firmata.New
     ↓
  Handshake (REPORT_VERSION)
     ↓
  QueryCapabilities (sysex 0x6B → CAPABILITY_RESPONSE 0x6C)
     ↓
  QueryAnalogMapping (sysex 0x69 → ANALOG_MAPPING_RESPONSE 0x6A)
     ↓
  optional SetSamplingInterval (sysex 0x7A)
     ↓
  resolve+validate Config.Analogs against caps/amap
     ↓
  build firmataAnalog map, ownedPins map, return *firmataBoard

pin.SetPWM(ctx, 0.6):
  ValidatePWMDutyCycle → ensure caps[pin] supports PWM → ensureMode(PWM) (once)
                      → client.AnalogWrite(pin, 153)
                          ├─ pin ≤15: ANALOG_MESSAGE 0xE0|ch, lsb, msb (3 bytes)
                          └─ pin ≥16: sysex 0xF0 0x6F pin lsb msb 0xF7
                      → cache pwmDuty[pin] = 0.6

analog.Read(ctx):
  enableOnce: SetPinMode(digitalPin, ANALOG) + EnableAnalogReporting(channel, true)
  return AnalogValue{ Value: client.ReadAnalog(channel), Min:0, Max:1023, StepSize:5.0/1024 }

readLoop (background, unchanged in shape):
  AnalogMessage         → analogState[ch] under stateMu (cached for ReadAnalog)
  CapabilityResponse    → non-blocking send to capabilities channel
  AnalogMappingResponse → non-blocking send to analogMap channel
  DigitalPortMessage    → portState (existing) + dispatch to events
```

## 5. Error handling

| Scenario | Behavior |
|---|---|
| `Validate` rejects: bad analog pin string, duplicate analog name, duplicate pin literal, sampling_interval_ms out of range | viam-server rejects config; module never starts. |
| Capability/analog-mapping query times out | Constructor returns wrapped `ctx.DeadlineExceeded` with hint about firmware compatibility ("CAPABILITY_QUERY not answered — is StandardFirmataPlus or ConfigurableFirmata flashed?"); port + client closed. |
| Declared analog pin doesn't support analog mode per capability map | Constructor returns `analog %q: pin %d does not support analog mode`; port + client closed. |
| `SetPWM` on a non-PWM-capable pin | Returns `pin %d does not support PWM` synchronously; no wire I/O. |
| `Set`/`Get`/`SetPWM` on a pin owned by a declared analog | Returns `pin %d is owned by analog %q`; no wire I/O. |
| `AnalogByName` on undeclared name | Returns `no analog named %q`. |
| Analog enable I/O error | First `Read` returns the wrapped error; cached in `enableErr` so subsequent calls return the same error rather than retrying mid-traffic. |
| Per-pin `samples_per_sec` set on an `AnalogReaderConfig` | Logged as warning at constructor time, value ignored. |
| Unknown sysex (anything other than the two query responses) | Decoded as `UnknownMessage` and dropped by `readLoop`, same as today. |

## 6. Testing

### 6.1 `internal/firmata/protocol_test.go` additions

- Encode round-trips: `encodeAnalogWrite(channel, 1023)`, `encodeExtendedAnalog(20, 1023)`, `encodeReportAnalog(0, true)`, `encodeSamplingInterval(100)`, `encodeCapabilityQuery()`, `encodeAnalogMappingQuery()` produce the exact byte sequences from the Firmata spec.
- Decode: feed canonical `CAPABILITY_RESPONSE` / `ANALOG_MAPPING_RESPONSE` byte streams (taken from the spec's examples — Uno-shaped, plus a synthetic ESP32-shaped one with pins ≥16) and assert the decoded structs match.
- Decode: `ANALOG_MESSAGE` (0xE0 | channel) decodes to `AnalogMessage{Channel, Value}` with correct two-7-bit-byte assembly.
- Resync property still holds after the new sysex types (i.e., a stray data byte before `0xE0` is still skipped).

### 6.2 `internal/firmata/client_test.go` additions

- Inject `ANALOG_MESSAGE(channel=0, value=512)` → `client.ReadAnalog(0)` returns 512; `ReadAnalog(1)` returns 0; race-free under `-race`.
- `QueryCapabilities`/`QueryAnalogMapping` return the decoded payload when the fake firmware writes a matching response; return `ctx.DeadlineExceeded` (wrapped) when nothing is sent.
- `AnalogWrite(15, 100)` emits 3 bytes; `AnalogWrite(20, 100)` emits the 6-byte EXTENDED_ANALOG sysex frame.

### 6.3 `firmata_board_test.go` additions (pipe-based)

A test helper builds a `*firmataBoard` directly from a pipe pair *after* the fake firmware has written canonical handshake + capability + analog-mapping responses, so each test case starts from a fully-initialized board.

1. Constructor performs handshake + capability query + analog-mapping query in order; if the fake firmware never answers a query, the constructor returns an error and the port/client are closed.
2. `analogs: [{name: "x", pin: "A0"}]` builds a working `firmataAnalog` that, on first `Read`, emits `SET_PIN_MODE(ANALOG)` + `REPORT_ANALOG(0, 1)`; injecting `ANALOG_MESSAGE(0, 512)` makes the next `Read` return `Value=512`.
3. `pin: "14"` and `pin: "A0"` (on an Uno-shaped capability map where digital 14 == analog channel 0) resolve to the same internal `(digitalPin, channel)` record.
4. Validate rejects: duplicate analog name, duplicate pin literal across two `analogs[]` entries, malformed pin string, out-of-range `sampling_interval_ms`.
5. Constructor rejects an `analogs[]` entry whose pin isn't analog-capable per the (synthetic) capability map.
6. `SetPWM(0.5)` on a PWM-capable pin emits `SET_PIN_MODE(PWM)` + `ANALOG_MESSAGE`; `PWM()` afterward returns 0.5 from cache.
7. `SetPWM` on a non-PWM-capable pin returns `does not support PWM`, no bytes emitted.
8. `SetPWM(0.5)` on pin 20 emits the EXTENDED_ANALOG sysex (covers the ≥16 path).
9. `Set(true)` on a pin declared in `analogs[]` returns `pin %d is owned by analog %q`, no bytes emitted.
10. `SamplingIntervalMs: 50` causes one `SAMPLING_INTERVAL` sysex on the wire during construction.
11. Per-pin `samples_per_sec` field on `AnalogReaderConfig` triggers a warning log (assert via a captured `logger`); does not fail validate.

## 7. Deliverables checklist

1. `internal/firmata/protocol.go` — sysex constants, encoders, new message types, expanded `decode` (analog + sysex responses).
2. `internal/firmata/protocol_test.go` — codec coverage above.
3. `internal/firmata/client.go` — `analogState`, `capabilities`/`analogMap` channels, `AnalogWrite`/`EnableAnalogReporting`/`SetSamplingInterval`/`ReadAnalog`/`QueryCapabilities`/`QueryAnalogMapping`.
4. `internal/firmata/client_test.go` — coverage above.
5. `firmata_board.go` — `Config.Analogs` + `SamplingIntervalMs`, `parseAnalogPin`, `resolveAnalogPin`, `firmataAnalog`, `AnalogByName`, `pwmDuty` cache, ownership check, PWM impl, constructor extension.
6. `firmata_board_test.go` — pipe-based coverage above.
7. `README.md` — new "Analog and PWM" section: example config with `analogs[]`, note on fixed PWM frequency, note that per-pin `samples_per_sec` is ignored.
8. `meta.json` — bump version (suggest minor bump; this is a feature release).

## 8. Open questions

None blocking. Deferred:

- Digital interrupts + `StreamTicks` → next spec (Option C from the brainstorming session).
- Auto-reconnect on serial drop → still v2-future.
- 14-bit PWM resolution via `EXTENDED_ANALOG` (we ship 8-bit because `dutyCyclePct * 255` is what the Viam interface implies).
