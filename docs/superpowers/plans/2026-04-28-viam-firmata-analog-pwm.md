# viam-firmata Analog + PWM Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend `devrel:firmata:board` from digital-GPIO-only to fully implement analog reads and PWM via Firmata's `ANALOG_MESSAGE` / `EXTENDED_ANALOG` / `REPORT_ANALOG` / `SAMPLING_INTERVAL` / `CAPABILITY_QUERY` / `ANALOG_MAPPING_QUERY` wire features. Digital interrupts remain `errUnimplemented` and ship in a separate spec/plan.

**Architecture:** Codec layer (`internal/firmata/protocol.go`) gains new sysex constants/encoders/decoders and three new message types (`AnalogMessage`, `CapabilityResponse`, `AnalogMappingResponse`). Client layer (`internal/firmata/client.go`) gains `analogState`, query channels, and new public methods (`AnalogWrite`, `EnableAnalogReporting`, `SetSamplingInterval`, `ReadAnalog`, `QueryCapabilities`, `QueryAnalogMapping`). Board layer (`firmata_board.go`) extends `Config` with `Analogs[]` and `SamplingIntervalMs`, runs capability + analog-mapping queries during construction, builds a `firmataAnalog` per declared analog, enforces strict pin ownership, and implements `SetPWM`/`PWM` on `firmataGPIOPin`.

**Tech Stack:** Go 1.23+, `go.viam.com/rdk@v0.123.0` (`board`/`resource`/`logging`), `go.bug.st/serial`, standard-library `bufio`/`io`/`sync`. Tests use the existing `io.Pipe`-based pattern (no hardware required).

**Spec:** [`docs/superpowers/specs/2026-04-28-viam-firmata-analog-pwm-design.md`](../specs/2026-04-28-viam-firmata-analog-pwm-design.md)

**Discipline:** TDD per task — failing test first, run it, minimal implementation, run it green, commit. Existing tests must stay green throughout (`go test -race ./...`).

---

## File map

| File | Change | Purpose |
|---|---|---|
| `internal/firmata/protocol.go` | Modify | Add sysex constants, encoders for analog/sampling/queries, `AnalogMessage`/`CapabilityResponse`/`AnalogMappingResponse` types; extend `decode()` with `ANALOG_MESSAGE` and sysex-subcommand branches. |
| `internal/firmata/protocol_test.go` | Modify | Codec tests for the new encoders and decoders. |
| `internal/firmata/client.go` | Modify | Add `analogState`, query channels, new public methods (`AnalogWrite`, `EnableAnalogReporting`, `SetSamplingInterval`, `ReadAnalog`, `QueryCapabilities`, `QueryAnalogMapping`); dispatch the new message types in `readLoop`. |
| `internal/firmata/client_test.go` | Modify | Pipe-based tests for the new client methods. |
| `firmata_board.go` | Modify | Extend `Config` (`Analogs`, `SamplingIntervalMs`) + `Validate`. Add `parseAnalogPin`/`resolveAnalogPin` helpers. Add `firmataAnalog` struct. Add `pinSupports` helper, `ownedPins` map, `pwmDuty` cache. Implement `AnalogByName`, `firmataGPIOPin.SetPWM`/`PWM`, ownership checks on `Set`/`Get`. Extend `NewBoard` with capability + analog-mapping queries and analog construction. |
| `firmata_board_test.go` | Modify | Replace/extend the test helper to inject canonical handshake + capability + analog-mapping responses. Add coverage for analog Read, PWM, ownership, validation, sampling interval, and capability rejection. Update `TestUnimplementedMethods_ReturnSentinelError` for the methods that are no longer unimplemented. |
| `README.md` | Modify | Update v1 scope section, add "Analog and PWM" section with example config, document the fixed-PWM-frequency caveat and the ignored per-pin `samples_per_sec` field. |

No new files are created. No file is deleted. No package-rename or module-path change.

---

## Task 1: Codec — sysex constants and message types

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/protocol.go`

This task only adds compile-time additions (constants and types) — no runtime behavior change yet. We'll write a placeholder test that proves the new types exist and zero-value correctly.

- [ ] **Step 1: Write the failing test**

Add to `internal/firmata/protocol_test.go`:

```go
func TestNewMessageTypes_ZeroValues(t *testing.T) {
	var am AnalogMessage
	if am.Channel != 0 || am.Value != 0 {
		t.Errorf("AnalogMessage zero value: %+v", am)
	}

	var cr CapabilityResponse
	if cr.Pins != nil {
		t.Errorf("CapabilityResponse.Pins zero value: %v", cr.Pins)
	}

	var ar AnalogMappingResponse
	if ar.ChannelByPin != nil {
		t.Errorf("AnalogMappingResponse.ChannelByPin zero value: %v", ar.ChannelByPin)
	}

	// Ensure they all satisfy the Message interface (compile-time check via assertion).
	var _ Message = AnalogMessage{}
	var _ Message = CapabilityResponse{}
	var _ Message = AnalogMappingResponse{}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/firmata/ -run TestNewMessageTypes_ZeroValues`
Expected: compile error — types undefined.

- [ ] **Step 3: Add constants and types in `internal/firmata/protocol.go`**

Add the new sysex constants alongside the existing `cmd*` block:

```go
const (
	sysexAnalogMappingQuery    uint8 = 0x69
	sysexAnalogMappingResponse uint8 = 0x6A
	sysexCapabilityQuery       uint8 = 0x6B
	sysexCapabilityResponse    uint8 = 0x6C
	sysexExtendedAnalog        uint8 = 0x6F
	sysexSamplingInterval      uint8 = 0x7A
)
```

Add the new types below the existing `DigitalPortMessage`:

```go
// AnalogMessage carries one ADC sample for a single analog channel.
// Value is 14-bit on the wire (firmware splits into two 7-bit bytes); real
// AVR boards send 10-bit samples (0..1023).
type AnalogMessage struct {
	Channel uint8
	Value   uint16
}

func (AnalogMessage) isMessage() {}

// PinCapabilities maps each supported PinMode to its resolution in bits.
// A pin reports zero or more (mode, resolution) pairs in a CAPABILITY_RESPONSE.
type PinCapabilities map[PinMode]uint8

// CapabilityResponse is the decoded payload of a CAPABILITY_RESPONSE sysex.
// Pins is indexed by digital pin number; entries for pins absent from the
// firmware response are nil.
type CapabilityResponse struct {
	Pins []PinCapabilities
}

func (CapabilityResponse) isMessage() {}

// AnalogMappingResponse is the decoded payload of an ANALOG_MAPPING_RESPONSE
// sysex. ChannelByPin[digitalPin] = analog channel, or 127 (0x7F) if the pin
// is not analog-capable.
type AnalogMappingResponse struct {
	ChannelByPin []uint8
}

func (AnalogMappingResponse) isMessage() {}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/firmata/ -run TestNewMessageTypes_ZeroValues`
Expected: PASS.

- [ ] **Step 5: Run the whole suite to make sure nothing regressed**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/protocol.go internal/firmata/protocol_test.go
git commit -m "$(cat <<'EOF'
feat(firmata): add analog/sysex message types and constants

Adds AnalogMessage, CapabilityResponse, AnalogMappingResponse, plus
PinCapabilities map type. Adds sysex command constants (CAPABILITY_QUERY,
ANALOG_MAPPING_QUERY, EXTENDED_ANALOG, SAMPLING_INTERVAL, and the matching
response IDs). No encoder/decoder wiring yet.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Codec — encoder for ANALOG_MESSAGE and EXTENDED_ANALOG

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/protocol.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/protocol_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/firmata/protocol_test.go`:

```go
func TestEncodeAnalogWrite(t *testing.T) {
	// ANALOG_MESSAGE for channel 6, value 200:
	//   cmd = 0xE0 | 6 = 0xE6
	//   lsb = 200 & 0x7F = 0x48
	//   msb = (200 >> 7) & 0x7F = 0x01
	got := encodeAnalogWrite(6, 200)
	want := []byte{0xE6, 0x48, 0x01}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeAnalogWrite(6, 200): got % X, want % X", got, want)
	}
}

func TestEncodeExtendedAnalog(t *testing.T) {
	// EXTENDED_ANALOG for pin 20, value 200:
	//   0xF0 0x6F pin lsb msb 0xF7
	got := encodeExtendedAnalog(20, 200)
	want := []byte{0xF0, 0x6F, 20, 0x48, 0x01, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeExtendedAnalog(20, 200): got % X, want % X", got, want)
	}
}

func TestEncodeExtendedAnalog_HighResolution(t *testing.T) {
	// 14-bit value 0x3FFF should serialize to two 7-bit bytes 0x7F, 0x7F.
	got := encodeExtendedAnalog(20, 0x3FFF)
	want := []byte{0xF0, 0x6F, 20, 0x7F, 0x7F, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeExtendedAnalog(20, 0x3FFF): got % X, want % X", got, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/firmata/ -run "TestEncodeAnalogWrite|TestEncodeExtendedAnalog"`
Expected: compile error — `encodeAnalogWrite` / `encodeExtendedAnalog` undefined.

- [ ] **Step 3: Implement the encoders in `protocol.go`**

```go
// encodeAnalogWrite emits an ANALOG_MESSAGE for analog channels 0..15.
// Value is masked to 14 bits and split across two 7-bit data bytes.
func encodeAnalogWrite(channel uint8, value uint16) []byte {
	value &= 0x3FFF
	return []byte{
		cmdAnalogMessage | (channel & 0x0F),
		uint8(value & 0x7F),
		uint8((value >> 7) & 0x7F),
	}
}

// encodeExtendedAnalog emits a sysex EXTENDED_ANALOG for pins/channels >15.
// Value is masked to 14 bits and split across two 7-bit data bytes.
func encodeExtendedAnalog(pin uint8, value uint16) []byte {
	value &= 0x3FFF
	return []byte{
		cmdStartSysex,
		sysexExtendedAnalog,
		pin & 0x7F,
		uint8(value & 0x7F),
		uint8((value >> 7) & 0x7F),
		cmdEndSysex,
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/firmata/ -run "TestEncodeAnalogWrite|TestEncodeExtendedAnalog"`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/protocol.go internal/firmata/protocol_test.go
git commit -m "$(cat <<'EOF'
feat(firmata): encode ANALOG_MESSAGE and EXTENDED_ANALOG frames

Adds encodeAnalogWrite (channel <=15, 3 bytes) and encodeExtendedAnalog
(pin or channel >=16, sysex-wrapped 6 bytes) per the Firmata 2.x spec.
Both encoders mask the value to 14 bits and split it across two 7-bit
data bytes.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Codec — encoders for REPORT_ANALOG, SAMPLING_INTERVAL, queries

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/protocol.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/protocol_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/firmata/protocol_test.go`:

```go
func TestEncodeReportAnalog(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"channel 0 enable", encodeReportAnalog(0, true), []byte{0xC0, 0x01}},
		{"channel 5 enable", encodeReportAnalog(5, true), []byte{0xC5, 0x01}},
		{"channel 0 disable", encodeReportAnalog(0, false), []byte{0xC0, 0x00}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Equal(tc.got, tc.want) {
				t.Errorf("got % X, want % X", tc.got, tc.want)
			}
		})
	}
}

func TestEncodeSamplingInterval(t *testing.T) {
	// 100ms -> lsb=0x64, msb=0x00. Frame: F0 7A 64 00 F7.
	got := encodeSamplingInterval(100)
	want := []byte{0xF0, 0x7A, 0x64, 0x00, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeSamplingInterval(100): got % X, want % X", got, want)
	}

	// 1000ms -> 0x03E8. lsb=0x68, msb=0x07.
	got = encodeSamplingInterval(1000)
	want = []byte{0xF0, 0x7A, 0x68, 0x07, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("encodeSamplingInterval(1000): got % X, want % X", got, want)
	}
}

func TestEncodeCapabilityQuery(t *testing.T) {
	got := encodeCapabilityQuery()
	want := []byte{0xF0, 0x6B, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}

func TestEncodeAnalogMappingQuery(t *testing.T) {
	got := encodeAnalogMappingQuery()
	want := []byte{0xF0, 0x69, 0xF7}
	if !bytes.Equal(got, want) {
		t.Errorf("got % X, want % X", got, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/firmata/ -run "TestEncodeReportAnalog|TestEncodeSamplingInterval|TestEncodeCapabilityQuery|TestEncodeAnalogMappingQuery"`
Expected: compile error — encoder names undefined.

- [ ] **Step 3: Implement in `protocol.go`**

```go
func encodeReportAnalog(channel uint8, enable bool) []byte {
	var b uint8
	if enable {
		b = 1
	}
	return []byte{cmdReportAnalog | (channel & 0x0F), b}
}

// encodeSamplingInterval emits a SAMPLING_INTERVAL sysex. Caller is
// responsible for ensuring intervalMs fits in 14 bits (1..16383).
func encodeSamplingInterval(intervalMs uint16) []byte {
	intervalMs &= 0x3FFF
	return []byte{
		cmdStartSysex,
		sysexSamplingInterval,
		uint8(intervalMs & 0x7F),
		uint8((intervalMs >> 7) & 0x7F),
		cmdEndSysex,
	}
}

func encodeCapabilityQuery() []byte {
	return []byte{cmdStartSysex, sysexCapabilityQuery, cmdEndSysex}
}

func encodeAnalogMappingQuery() []byte {
	return []byte{cmdStartSysex, sysexAnalogMappingQuery, cmdEndSysex}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/firmata/ -run "TestEncodeReportAnalog|TestEncodeSamplingInterval|TestEncodeCapabilityQuery|TestEncodeAnalogMappingQuery"`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/protocol.go internal/firmata/protocol_test.go
git commit -m "$(cat <<'EOF'
feat(firmata): encode REPORT_ANALOG, SAMPLING_INTERVAL, CAPABILITY_QUERY, ANALOG_MAPPING_QUERY

Adds the four query/control encoders needed to negotiate analog-capable
pins and the firmware sampling rate at handshake time. SAMPLING_INTERVAL
and the two queries are sysex frames; REPORT_ANALOG is a 2-byte command.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Codec — decode ANALOG_MESSAGE

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/protocol.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/protocol_test.go`

- [ ] **Step 1: Write the failing test**

Add to `protocol_test.go`:

```go
func TestDecode_AnalogMessage(t *testing.T) {
	// ANALOG_MESSAGE channel 0, value 512: 0xE0 0x00 0x04
	r := bufio.NewReader(bytes.NewReader([]byte{0xE0, 0x00, 0x04}))
	msg, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a, ok := msg.(AnalogMessage)
	if !ok {
		t.Fatalf("wanted AnalogMessage, got %T", msg)
	}
	if a.Channel != 0 || a.Value != 512 {
		t.Errorf("got channel=%d value=%d, want channel=0 value=512", a.Channel, a.Value)
	}
}

func TestDecode_AnalogMessage_HighChannel(t *testing.T) {
	// ANALOG_MESSAGE channel 7, value 1023: 0xE7 0x7F 0x07
	r := bufio.NewReader(bytes.NewReader([]byte{0xE7, 0x7F, 0x07}))
	msg, err := decode(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	a := msg.(AnalogMessage)
	if a.Channel != 7 || a.Value != 1023 {
		t.Errorf("got channel=%d value=%d, want channel=7 value=1023", a.Channel, a.Value)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/firmata/ -run TestDecode_AnalogMessage`
Expected: FAIL — current `decode()` returns `UnknownMessage` for any non-version, non-digital, non-sysex command, including 0xE0.

- [ ] **Step 3: Add an `ANALOG_MESSAGE` case to `decode()`**

In `protocol.go`, *insert* a new case after the existing `cmd&0xF0 == cmdDigitalMessage` branch and before the `cmd == cmdStartSysex` branch:

```go
case cmd&0xF0 == cmdAnalogMessage:
    lsb, err := r.ReadByte()
    if err != nil {
        return nil, fmt.Errorf("read analog lsb: %w", err)
    }
    msb, err := r.ReadByte()
    if err != nil {
        return nil, fmt.Errorf("read analog msb: %w", err)
    }
    return AnalogMessage{
        Channel: cmd & 0x0F,
        Value:   uint16(lsb&0x7F) | (uint16(msb&0x7F) << 7),
    }, nil
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/firmata/ -run TestDecode_AnalogMessage`
Expected: PASS.

- [ ] **Step 5: Run the whole suite (regression check)**

Run: `go test -race ./...`
Expected: PASS — including `TestDecode_ResyncOnLeadingNoise` and `TestDecode_SysexIsSkipped`.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/protocol.go internal/firmata/protocol_test.go
git commit -m "$(cat <<'EOF'
feat(firmata): decode ANALOG_MESSAGE frames

Recognizes 0xE0|channel as an ANALOG_MESSAGE; consumes the two 7-bit
data bytes and reassembles the 14-bit value. Previously these frames
fell through to the catch-all 3-byte UnknownMessage path.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Codec — decode CAPABILITY_RESPONSE and ANALOG_MAPPING_RESPONSE

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/protocol.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/protocol_test.go`

The existing `decode()` returns `UnknownMessage{Cmd: cmdStartSysex, Payload: ...}` for *every* sysex frame. We're going to look at `Payload[0]` to dispatch the two responses we care about, and fall through to `UnknownMessage` for everything else.

Background — wire formats per the Firmata spec:

```
CAPABILITY_RESPONSE:
  0xF0 0x6C
  for each pin:
    [(mode, resolution), ...]   # repeated mode/resolution pairs
    0x7F                        # terminator for this pin
  0xF7

ANALOG_MAPPING_RESPONSE:
  0xF0 0x6A
  for each pin (in pin order):
    channel | 0x7F              # 0x7F means "not analog"
  0xF7
```

- [ ] **Step 1: Write the failing test**

Add to `protocol_test.go`:

```go
func TestDecode_CapabilityResponse(t *testing.T) {
	// Two pins worth of payload:
	//   pin 0: INPUT (res 1), OUTPUT (res 1), terminator 0x7F
	//   pin 1: PWM (res 8), terminator 0x7F
	frame := []byte{
		0xF0, 0x6C,
		0x00, 0x01, // INPUT, 1 bit
		0x01, 0x01, // OUTPUT, 1 bit
		0x7F,
		0x03, 0x08, // PWM, 8 bits
		0x7F,
		0xF7,
	}
	msg, err := decode(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cr, ok := msg.(CapabilityResponse)
	if !ok {
		t.Fatalf("wanted CapabilityResponse, got %T", msg)
	}
	if len(cr.Pins) != 2 {
		t.Fatalf("len(Pins) = %d, want 2", len(cr.Pins))
	}
	if cr.Pins[0][PinModeInput] != 1 || cr.Pins[0][PinModeOutput] != 1 {
		t.Errorf("pin 0 caps = %v", cr.Pins[0])
	}
	if cr.Pins[1][PinModePWM] != 8 {
		t.Errorf("pin 1 caps = %v", cr.Pins[1])
	}
}

func TestDecode_AnalogMappingResponse(t *testing.T) {
	// 4-pin map: pin 0 -> not analog (0x7F), pin 1 -> not analog,
	//            pin 2 -> ch 0, pin 3 -> ch 1
	frame := []byte{0xF0, 0x6A, 0x7F, 0x7F, 0x00, 0x01, 0xF7}
	msg, err := decode(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	am, ok := msg.(AnalogMappingResponse)
	if !ok {
		t.Fatalf("wanted AnalogMappingResponse, got %T", msg)
	}
	want := []uint8{0x7F, 0x7F, 0x00, 0x01}
	if !bytes.Equal(am.ChannelByPin, want) {
		t.Errorf("got % X, want % X", am.ChannelByPin, want)
	}
}

func TestDecode_UnknownSysexStillFallsThrough(t *testing.T) {
	// REPORT_FIRMWARE-ish sysex (0x79) is not in our switch — must remain UnknownMessage.
	frame := []byte{0xF0, 0x79, 0x02, 0x05, 0xF7}
	msg, err := decode(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := msg.(UnknownMessage); !ok {
		t.Fatalf("wanted UnknownMessage, got %T", msg)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/firmata/ -run "TestDecode_CapabilityResponse|TestDecode_AnalogMappingResponse|TestDecode_UnknownSysexStillFallsThrough"`
Expected: First two FAIL (decoded as `UnknownMessage`); the third PASSES already (current behavior).

- [ ] **Step 3: Refactor the sysex branch in `decode()`**

In `protocol.go`, replace the `case cmd == cmdStartSysex:` block with:

```go
case cmd == cmdStartSysex:
    payload, err := readUntilEndSysex(r)
    if err != nil {
        return nil, fmt.Errorf("read sysex: %w", err)
    }
    if len(payload) == 0 {
        return UnknownMessage{Cmd: cmdStartSysex, Payload: payload}, nil
    }
    switch payload[0] {
    case sysexCapabilityResponse:
        return decodeCapabilityResponse(payload[1:]), nil
    case sysexAnalogMappingResponse:
        return decodeAnalogMappingResponse(payload[1:]), nil
    default:
        return UnknownMessage{Cmd: cmdStartSysex, Payload: payload}, nil
    }
```

Then add the two helpers below `readUntilEndSysex`:

```go
// decodeCapabilityResponse parses a CAPABILITY_RESPONSE payload (already
// stripped of leading 0x6C and trailing 0xF7). Each pin contributes zero or
// more (mode, resolution) pairs followed by a 0x7F terminator.
func decodeCapabilityResponse(p []byte) CapabilityResponse {
	pins := []PinCapabilities{}
	current := PinCapabilities{}
	for i := 0; i < len(p); i++ {
		if p[i] == 0x7F {
			pins = append(pins, current)
			current = PinCapabilities{}
			continue
		}
		// Need at least two bytes for a (mode, resolution) pair.
		if i+1 >= len(p) {
			break
		}
		current[PinMode(p[i])] = p[i+1]
		i++
	}
	return CapabilityResponse{Pins: pins}
}

// decodeAnalogMappingResponse parses an ANALOG_MAPPING_RESPONSE payload
// (already stripped of leading 0x6A and trailing 0xF7). Each byte is the
// analog channel for the matching digital pin, or 0x7F if not analog.
func decodeAnalogMappingResponse(p []byte) AnalogMappingResponse {
	out := make([]uint8, len(p))
	copy(out, p)
	return AnalogMappingResponse{ChannelByPin: out}
}
```

- [ ] **Step 4: Run all decode tests**

Run: `go test ./internal/firmata/ -run TestDecode`
Expected: PASS — including the unchanged `TestDecode_SysexIsSkipped` (which uses sysex 0x79 — falls through correctly).

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/protocol.go internal/firmata/protocol_test.go
git commit -m "$(cat <<'EOF'
feat(firmata): decode CAPABILITY_RESPONSE and ANALOG_MAPPING_RESPONSE

Promotes the sysex branch in decode() from "always UnknownMessage" to
dispatching on the first sysex byte. CAPABILITY_RESPONSE (0x6C) and
ANALOG_MAPPING_RESPONSE (0x6A) decode to their structured forms; every
other sysex still falls through to UnknownMessage so unknown frames
remain harmless.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Client — analogState + ReadAnalog

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/client.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/client_test.go`

- [ ] **Step 1: Write the failing test**

Add to `client_test.go` (uses the existing `rwAdapter` helper from `TestReadDigital_BeforeAndAfterDispatch`):

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/firmata/ -run TestReadAnalog_BeforeAndAfterDispatch`
Expected: compile error — `ReadAnalog` undefined.

- [ ] **Step 3: Add `analogState` field, dispatch in `readLoop`, and `ReadAnalog` accessor**

In `client.go`, add to the `Client` struct (after `portState`/`outState`):

```go
analogState [16]uint16 // last-known value per analog channel
```

In `readLoop`, add a case to the switch right after `case DigitalPortMessage:`:

```go
case AnalogMessage:
    c.dispatchAnalog(m)
```

And add the dispatch helper near `dispatchDigital`:

```go
// dispatchAnalog stores the latest analog reading for the channel under
// stateMu so ReadAnalog can read concurrently with the reader goroutine.
func (c *Client) dispatchAnalog(m AnalogMessage) {
	if int(m.Channel) >= len(c.analogState) {
		return
	}
	c.stateMu.Lock()
	c.analogState[m.Channel] = m.Value
	c.stateMu.Unlock()
}
```

Add the public accessor at the bottom of the file:

```go
// ReadAnalog returns the cached last value for the given analog channel.
// Returns 0 if the channel is out of range [0, 15] or no ANALOG_MESSAGE has
// arrived for that channel yet.
func (c *Client) ReadAnalog(channel int) uint16 {
	if channel < 0 || channel >= len(c.analogState) {
		return 0
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.analogState[channel]
}
```

- [ ] **Step 4: Run the new test**

Run: `go test ./internal/firmata/ -run TestReadAnalog_BeforeAndAfterDispatch -race`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/client.go internal/firmata/client_test.go
git commit -m "$(cat <<'EOF'
feat(firmata): cache analog samples in Client + ReadAnalog accessor

readLoop now dispatches AnalogMessage frames into a [16]uint16
analogState array guarded by the existing stateMu. ReadAnalog mirrors
ReadDigital: cached, non-blocking, returns 0 for out-of-range channels
or when no frame has arrived yet.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Client — AnalogWrite (with ≤15 vs ≥16 split)

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/client.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/client_test.go`

- [ ] **Step 1: Write the failing test**

Add to `client_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/firmata/ -run TestAnalogWrite`
Expected: compile error — `AnalogWrite` undefined.

- [ ] **Step 3: Implement `AnalogWrite`**

In `client.go`, add a new method:

```go
// AnalogWrite writes an analog value (e.g. PWM duty 0..255 or DAC value).
// Pins 0..15 use ANALOG_MESSAGE (0xE0|channel, 3 bytes); pins 16..127 use
// EXTENDED_ANALOG sysex (6 bytes). The pin number passed here is the
// digital-pin index — for ANALOG_MESSAGE it doubles as the channel because
// firmware accepts the pin number in the low nibble of the command byte.
func (c *Client) AnalogWrite(pin int, value uint16) error {
	if pin < 0 || pin > 127 {
		return fmt.Errorf("pin %d out of range", pin)
	}
	if pin <= 15 {
		return c.writeFrame(encodeAnalogWrite(uint8(pin), value))
	}
	return c.writeFrame(encodeExtendedAnalog(uint8(pin), value))
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/firmata/ -run TestAnalogWrite`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/client.go internal/firmata/client_test.go
git commit -m "$(cat <<'EOF'
feat(firmata): Client.AnalogWrite for PWM / analog output

Sends ANALOG_MESSAGE for pin <=15 and EXTENDED_ANALOG sysex for pin >=16,
matching the Firmata 2.x spec. Used by the board's SetPWM (and future
non-PWM analog outputs).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Client — EnableAnalogReporting + SetSamplingInterval

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/client.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/client_test.go`

- [ ] **Step 1: Write the failing test**

Add to `client_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/firmata/ -run "TestEnableAnalogReporting|TestSetSamplingInterval"`
Expected: compile error.

- [ ] **Step 3: Implement the methods**

In `client.go`:

```go
// EnableAnalogReporting enables or disables auto-reporting for an analog
// channel. The argument is an analog channel index (0..15), parallel to
// EnableDigitalReporting which takes a port index.
func (c *Client) EnableAnalogReporting(channel int, enable bool) error {
	if channel < 0 || channel > 15 {
		return fmt.Errorf("analog channel %d out of range", channel)
	}
	return c.writeFrame(encodeReportAnalog(uint8(channel), enable))
}

// SetSamplingInterval sets the firmware-side sample interval (in ms) for
// all enabled analog reports. Must be in 1..16383.
func (c *Client) SetSamplingInterval(intervalMs int) error {
	if intervalMs < 1 || intervalMs > 16383 {
		return fmt.Errorf("sampling interval %dms out of range (1..16383)", intervalMs)
	}
	return c.writeFrame(encodeSamplingInterval(uint16(intervalMs)))
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/firmata/ -run "TestEnableAnalogReporting|TestSetSamplingInterval"`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/firmata/client.go internal/firmata/client_test.go
git commit -m "$(cat <<'EOF'
feat(firmata): EnableAnalogReporting and SetSamplingInterval

EnableAnalogReporting parallels EnableDigitalReporting but takes an
analog channel index (0..15). SetSamplingInterval sends the global
SAMPLING_INTERVAL sysex; range-checks 1..16383ms.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Client — QueryCapabilities and QueryAnalogMapping

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/client.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/internal/firmata/client_test.go`

These methods follow the same pattern as `Handshake`: send the query in a goroutine (so `io.Pipe` tests don't deadlock), then `select` on the response channel vs `ctx.Done()`. The response channels are buffered-1 just like `version`.

- [ ] **Step 1: Write the failing tests**

Add to `client_test.go`:

```go
func TestQueryCapabilities_Succeeds(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	// Fake board responds with a 2-pin capability payload.
	go func() {
		_, _ = pp.board.Write([]byte{
			0xF0, 0x6C,
			0x00, 0x01, 0x01, 0x01, 0x7F, // pin 0: INPUT(1), OUTPUT(1)
			0x03, 0x08, 0x7F,             // pin 1: PWM(8)
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/firmata/ -run "TestQueryCapabilities|TestQueryAnalogMapping"`
Expected: compile error.

- [ ] **Step 3: Wire up the new channels and dispatch in `readLoop`**

In `client.go`, add to the `Client` struct (next to `version`):

```go
capabilities chan CapabilityResponse
analogMap    chan AnalogMappingResponse
```

In `New()`, allocate them:

```go
capabilities: make(chan CapabilityResponse, 1),
analogMap:    make(chan AnalogMappingResponse, 1),
```

In `readLoop`, add cases (right after `case VersionMessage:`):

```go
case CapabilityResponse:
    select {
    case c.capabilities <- m:
    default:
    }
case AnalogMappingResponse:
    select {
    case c.analogMap <- m:
    default:
    }
```

- [ ] **Step 4: Implement the query methods**

In `client.go`, add below `Handshake`:

```go
// QueryCapabilities sends a CAPABILITY_QUERY and waits for the matching
// response. Same shape as Handshake — the query is written from a goroutine
// so io.Pipe-based tests don't deadlock.
func (c *Client) QueryCapabilities(ctx context.Context) (CapabilityResponse, error) {
	go func() { _ = c.writeFrame(encodeCapabilityQuery()) }()
	select {
	case r := <-c.capabilities:
		return r, nil
	case <-ctx.Done():
		return CapabilityResponse{}, fmt.Errorf("capability query: %w (no CAPABILITY_RESPONSE — is StandardFirmataPlus or ConfigurableFirmata flashed?)", ctx.Err())
	}
}

// QueryAnalogMapping sends an ANALOG_MAPPING_QUERY and waits for the matching
// response.
func (c *Client) QueryAnalogMapping(ctx context.Context) (AnalogMappingResponse, error) {
	go func() { _ = c.writeFrame(encodeAnalogMappingQuery()) }()
	select {
	case r := <-c.analogMap:
		return r, nil
	case <-ctx.Done():
		return AnalogMappingResponse{}, fmt.Errorf("analog mapping query: %w (no ANALOG_MAPPING_RESPONSE)", ctx.Err())
	}
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/firmata/ -run "TestQueryCapabilities|TestQueryAnalogMapping" -race`
Expected: PASS.

- [ ] **Step 6: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/firmata/client.go internal/firmata/client_test.go
git commit -m "$(cat <<'EOF'
feat(firmata): QueryCapabilities and QueryAnalogMapping

Send the matching sysex query and wait on a buffered-1 response channel
populated by readLoop. Mirrors the existing Handshake pattern, including
the goroutine-write so io.Pipe-based tests don't deadlock. Errors wrap
ctx.DeadlineExceeded with a hint about firmware compatibility.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Board — Config additions and Validate

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board_test.go`

This task only touches `Config`. We're NOT yet touching the constructor or the test helper — those come in Task 12.

- [ ] **Step 1: Write the failing tests**

Add to `firmata_board_test.go` (alongside the existing `TestConfig_Validate` block, extending the same `t.Run` style):

```go
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
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test -run TestConfig_Validate_Analogs`
Expected: compile error — `Config.Analogs` and `Config.SamplingIntervalMs` undefined; `parseAnalogPin` undefined.

- [ ] **Step 3: Extend `Config` and `Validate`**

In `firmata_board.go`:

Replace the `Config` struct with:

```go
type Config struct {
	SerialPath         string                     `json:"serial_path"`
	BaudRate           int                        `json:"baud_rate,omitempty"`
	AutoResetDelay     time.Duration              `json:"auto_reset_delay,omitempty"`
	HandshakeTimeout   time.Duration              `json:"handshake_timeout,omitempty"`
	SamplingIntervalMs int                        `json:"sampling_interval_ms,omitempty"`
	Analogs            []board.AnalogReaderConfig `json:"analogs,omitempty"`
}
```

Replace `Validate` with:

```go
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.SerialPath == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "serial_path")
	}
	if c.BaudRate < 0 {
		return nil, nil, resource.NewConfigValidationError(path,
			fmt.Errorf("baud_rate must be >= 0"))
	}
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
```

Add `parseAnalogPin` near the bottom of the file:

```go
// parseAnalogPin accepts "A0".."A15" or a raw digital pin number "0".."127".
// The unknown side of the pair is returned as the sentinel -1; resolveAnalogPin
// (called in NewBoard against the analog-mapping response) fills it in:
//
//	"A3"  -> (digitalPin: -1, analogChannel: 3)
//	"14"  -> (digitalPin: 14, analogChannel: -1)
func parseAnalogPin(s string) (digitalPin int, analogChannel int, err error) {
	if s == "" {
		return 0, 0, fmt.Errorf("analog pin name is empty")
	}
	if s[0] == 'A' || s[0] == 'a' {
		n, err := strconv.Atoi(s[1:])
		if err != nil || n < 0 || n > 15 {
			return 0, 0, fmt.Errorf("invalid analog pin %q (want \"A0\".. \"A15\")", s)
		}
		return -1, n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 127 {
		return 0, 0, fmt.Errorf("invalid analog pin %q (want \"A0\".. \"A15\" or 0..127)", s)
	}
	return n, -1, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test -run "TestConfig_Validate|TestConfig_Validate_Analogs"`
Expected: PASS.

- [ ] **Step 5: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS — including the existing v1 tests, since the constructor still sees a default-zero `Analogs` and `SamplingIntervalMs`.

- [ ] **Step 6: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "$(cat <<'EOF'
feat(board): Config.Analogs + SamplingIntervalMs with Validate coverage

Adds the analog declarations to the config schema and validates them at
the string level: pin parses, names unique, no duplicate pin literals,
sampling_interval_ms in 0..16383. Capability-level checks (analog support,
PWM support) happen in NewBoard once we have the firmware's CAPABILITY
response. Adds parseAnalogPin which accepts both "A0"-style aliases and
raw digital-pin numbers, returning -1 as the sentinel for the unknown side.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Board — `pinSupports` helper + struct fields

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board.go`

Add the new fields to `firmataBoard` and a small helper. No tests yet — these become testable once the test helper is reworked in Task 12.

- [ ] **Step 1: Add new fields**

In `firmata_board.go`, extend the `firmataBoard` struct:

```go
type firmataBoard struct {
	resource.Named
	resource.AlwaysRebuild

	logger logging.Logger

	closer io.Closer
	client *firmata.Client

	drainDone chan struct{}

	closeOnce sync.Once
	closeErr  error

	mu       sync.Mutex
	pinModes map[int]firmata.PinMode

	// Capability data populated in NewBoard right after handshake.
	capabilities firmata.CapabilityResponse
	analogMap    firmata.AnalogMappingResponse

	// analogs is the set of declared analog readers, keyed by their config
	// name. ownedPins maps each digital pin used by a declared analog back
	// to that analog's name so GPIOPinByName(...).Set/Get can refuse it.
	analogs   map[string]*firmataAnalog
	ownedPins map[int]string

	// pwmDuty is the last duty value written to each pin via SetPWM. Read
	// back by PWM(). Guarded by mu.
	pwmDuty map[int]float64
}
```

In `newBoardFromClient`, initialise the new maps with non-nil zero values so other call sites can ignore the analog feature when not needed:

```go
b := &firmataBoard{
	Named:     name.AsNamed(),
	logger:    logger,
	closer:    closer,
	client:    c,
	drainDone: make(chan struct{}),
	pinModes:  make(map[int]firmata.PinMode),
	analogs:   map[string]*firmataAnalog{},
	ownedPins: map[int]string{},
	pwmDuty:   map[int]float64{},
}
```

- [ ] **Step 2: Add `pinSupports` helper near the bottom of the file**

```go
// pinSupports reports whether the firmware advertises the given mode for
// the pin. Returns false if the pin is out of the capability map or the
// capability table is empty (e.g. before handshake-time discovery has
// populated it; tests that bypass NewBoard need to inject capabilities
// explicitly).
func (b *firmataBoard) pinSupports(pin int, mode firmata.PinMode) bool {
	if pin < 0 || pin >= len(b.capabilities.Pins) {
		return false
	}
	caps := b.capabilities.Pins[pin]
	_, ok := caps[mode]
	return ok
}
```

(`firmataAnalog` struct will be added in Task 13. We declare the field today so that compilation against the new struct is incremental.)

- [ ] **Step 3: Add a forward-declaration stub of `firmataAnalog`**

Right before `pinSupports`, add the struct definition (its methods come in Task 13):

```go
// firmataAnalog implements board.Analog over a firmata.Client. Lazily
// configures the pin and enables reporting on first Read; subsequent reads
// return the cached value from Client.analogState.
type firmataAnalog struct {
	board      *firmataBoard
	name       string
	digitalPin int
	channel    uint8
	enableOnce sync.Once
	enableErr  atomic.Pointer[error]
}
```

Add the new import for `sync/atomic` to the top of the file.

- [ ] **Step 4: Verify compile**

Run: `go build ./...`
Expected: success.

- [ ] **Step 5: Run the suite**

Run: `go test -race ./...`
Expected: PASS — no behavior change yet.

- [ ] **Step 6: Commit**

```bash
git add firmata_board.go
git commit -m "$(cat <<'EOF'
chore(board): scaffold analog/PWM state on firmataBoard

Adds capabilities/analogMap/analogs/ownedPins/pwmDuty fields, the
pinSupports helper, and a forward-declared firmataAnalog struct. No
behavior change yet — methods on firmataAnalog and the analog-aware
constructor flow follow in subsequent commits.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 12: Board test helper rework — `newTestBoardWithCaps`

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board_test.go`

The existing `newTestBoard` constructs a board via `newBoardFromClient` directly, leaving `capabilities`/`analogMap` empty. For tests of analog/PWM behavior, we need a test board whose capability map already includes the pins under test. We'll add a *second* helper that does the same direct-injection but lets the test pre-load capabilities.

- [ ] **Step 1: Plan the helper**

Goals:
- The existing `newTestBoard` should keep working unchanged (so v1 GPIO tests don't churn).
- A new `newTestBoardWithCaps(t, caps, analogMap)` builds a board whose `capabilities` and `analogMap` are pre-populated. It also exposes `arduinoW` and `sentBuf` like the existing helper.
- Uses an Uno-style capability map by default for any test that calls a "simple" variant.

- [ ] **Step 2: Add the helper to `firmata_board_test.go`**

```go
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
// analog-mapping data already injected. analogs may be empty.
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
```

`resolveAnalogPin` doesn't exist yet — we'll add it in Task 13. Since this helper is dead until tests call it, the file still compiles? **No** — go fails compilation on undefined `resolveAnalogPin`. Add a minimal stub at the bottom of `firmata_board.go` (just enough to satisfy the compiler) and flesh it out in Task 13:

```go
// resolveAnalogPin is implemented in Task 13.
func resolveAnalogPin(digitalPin, analogChannel int, _ firmata.AnalogMappingResponse) (int, int, error) {
	return digitalPin, analogChannel, fmt.Errorf("resolveAnalogPin: not implemented")
}
```

- [ ] **Step 3: Verify compile**

Run: `go build ./...`
Expected: success.

- [ ] **Step 4: Run the suite**

Run: `go test -race ./...`
Expected: PASS — the existing tests don't call the new helper, and the new helper is unreachable until later tasks add tests that use it.

- [ ] **Step 5: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "$(cat <<'EOF'
test(board): add newTestBoardWithCaps and Uno-shaped capability fixtures

Lets later tests pre-load capabilities and analog mapping without going
through the full handshake/query flow (which the pipe-based testBoard
helper bypasses). resolveAnalogPin is stubbed for now; full
implementation follows in the next commit.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 13: Board — `resolveAnalogPin` + `firmataAnalog.Read`/`Write`

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `firmata_board_test.go`:

```go
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
		0xC0, 0x01,     // REPORT_ANALOG(0, on)
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
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test -run "TestResolveAnalogPin|TestAnalogRead|TestAnalogWrite_OnAnalogReader"`
Expected: most fail — `resolveAnalogPin` is the stub from Task 12; `AnalogByName` still returns `errUnimplemented`; `firmataAnalog.Read`/`Write` don't exist yet.

- [ ] **Step 3: Implement `resolveAnalogPin`**

In `firmata_board.go`, replace the stub with:

```go
// resolveAnalogPin fills in whichever side of the (digitalPin, analogChannel)
// pair was left as the -1 sentinel by parseAnalogPin, using the analog mapping
// reported by the firmware. Returns an error if the pin/channel doesn't appear
// in the mapping.
func resolveAnalogPin(digitalPin, analogChannel int, amap firmata.AnalogMappingResponse) (int, int, error) {
	switch {
	case digitalPin >= 0 && analogChannel == -1:
		// Digital -> channel.
		if digitalPin >= len(amap.ChannelByPin) {
			return 0, 0, fmt.Errorf("pin %d out of range of analog mapping (len=%d)", digitalPin, len(amap.ChannelByPin))
		}
		ch := amap.ChannelByPin[digitalPin]
		if ch == 0x7F {
			return 0, 0, fmt.Errorf("pin %d is not analog-capable", digitalPin)
		}
		return digitalPin, int(ch), nil
	case digitalPin == -1 && analogChannel >= 0:
		// Channel -> digital pin: linear scan, mappings are short.
		for i, ch := range amap.ChannelByPin {
			if int(ch) == analogChannel {
				return i, analogChannel, nil
			}
		}
		return 0, 0, fmt.Errorf("analog channel %d not advertised by firmware", analogChannel)
	default:
		return 0, 0, fmt.Errorf("resolveAnalogPin: invalid input (digitalPin=%d, channel=%d)", digitalPin, analogChannel)
	}
}
```

- [ ] **Step 4: Implement `firmataAnalog.Read`, `firmataAnalog.Write`, and `AnalogByName`**

In `firmata_board.go`, add right after the `firmataAnalog` struct:

```go
func (a *firmataAnalog) Read(_ context.Context, _ map[string]any) (board.AnalogValue, error) {
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
	// enableErr is set once inside enableOnce.Do and never cleared, so
	// subsequent Reads return the same cached error rather than retrying
	// mid-traffic.
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
```

Replace the stub `AnalogByName` near the bottom of the file:

```go
func (b *firmataBoard) AnalogByName(name string) (board.Analog, error) {
	a, ok := b.analogs[name]
	if !ok {
		return nil, fmt.Errorf("firmata board: no analog named %q", name)
	}
	return a, nil
}
```

- [ ] **Step 5: Run the new tests**

Run: `go test -run "TestResolveAnalogPin|TestAnalogRead|TestAnalogWrite_OnAnalogReader" -race`
Expected: PASS.

- [ ] **Step 6: Run the whole suite**

Run: `go test -race ./...`
Expected: regress on `TestUnimplementedMethods_ReturnSentinelError` — `AnalogByName("a0")` no longer returns `errUnimplemented` (it returns the "no analog named" error). We'll fix that test in Task 17. For now, leave the failure visible.

If the regression makes you nervous, scope the test run for now: `go test -run "^(TestConfig|TestResolveAnalogPin|TestAnalogRead|TestAnalogWrite_OnAnalogReader|TestGPIOPin|TestSet_AfterStreamClose)" -race ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "$(cat <<'EOF'
feat(board): firmataAnalog.Read/Write + AnalogByName + resolveAnalogPin

Read lazily emits SET_PIN_MODE(ANALOG) + REPORT_ANALOG once via sync.Once
and then returns the cached value from Client.ReadAnalog. Subsequent
Reads are non-blocking. Write returns errUnimplemented since standard
Firmata boards have no DAC. resolveAnalogPin fills in the missing side
of the (digitalPin, analogChannel) pair from the firmware's analog map.

Note: TestUnimplementedMethods_ReturnSentinelError now regresses on the
AnalogByName assertion. Fixed in a subsequent commit (after the rest of
the analog/PWM surface lands).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 14: Board — pin ownership on `Set` / `Get`

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board_test.go`

- [ ] **Step 1: Write the failing test**

Add to `firmata_board_test.go`:

```go
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
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test -run "TestGPIOPin_Set_RejectedWhenOwnedByAnalog|TestGPIOPin_Get_RejectedWhenOwnedByAnalog|TestGPIOPin_Set_UnaffectedWhenAnotherPinIsOwned" -race`
Expected: ownership-related tests fail (pins write happily); the "unaffected" test should already pass.

- [ ] **Step 3: Add ownership checks at the top of `firmataGPIOPin.Set` and `firmataGPIOPin.Get`**

In `firmata_board.go`, add the check at the very top of both methods:

```go
func (p *firmataGPIOPin) Set(_ context.Context, high bool, _ map[string]any) error {
	if owner, taken := p.board.ownedPins[p.pin]; taken {
		return fmt.Errorf("firmata board: pin %d is owned by analog %q", p.pin, owner)
	}
	if err := p.ensureMode(firmata.PinModeOutput); err != nil {
		return err
	}
	return p.board.client.DigitalWrite(p.pin, high)
}

func (p *firmataGPIOPin) Get(_ context.Context, _ map[string]any) (bool, error) {
	if owner, taken := p.board.ownedPins[p.pin]; taken {
		return false, fmt.Errorf("firmata board: pin %d is owned by analog %q", p.pin, owner)
	}
	if err := p.ensureMode(firmata.PinModeInputPullup); err != nil {
		return false, err
	}
	return p.board.client.ReadDigital(p.pin), nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -run "TestGPIOPin_Set_RejectedWhenOwnedByAnalog|TestGPIOPin_Get_RejectedWhenOwnedByAnalog|TestGPIOPin_Set_UnaffectedWhenAnotherPinIsOwned" -race`
Expected: PASS.

- [ ] **Step 5: Run the whole suite (excluding the test we know is regressed)**

Run: `go test -run "^(TestConfig|TestResolveAnalogPin|TestAnalogRead|TestAnalogWrite_OnAnalogReader|TestGPIOPin|TestSet_AfterStreamClose)" -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "$(cat <<'EOF'
feat(board): refuse Set/Get on pins owned by a declared analog

ownedPins is populated when an analogs[] entry is built. If a caller
fetches a GPIOPin for a pin already owned, Set/Get returns a clear error
before any wire I/O. Pins not declared as analogs are unaffected.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 15: Board — `SetPWM` and `PWM`

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `firmata_board_test.go`:

```go
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
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test -run "TestSetPWM|TestPWM_Defaults" -race`
Expected: most fail — `SetPWM` returns `errUnimplemented` today.

- [ ] **Step 3: Implement `SetPWM` and `PWM`**

In `firmata_board.go`, replace the existing `SetPWM` and `PWM` stubs:

```go
func (p *firmataGPIOPin) SetPWM(_ context.Context, dutyCyclePct float64, _ map[string]any) error {
	if owner, taken := p.board.ownedPins[p.pin]; taken {
		return fmt.Errorf("firmata board: pin %d is owned by analog %q", p.pin, owner)
	}
	duty, err := board.ValidatePWMDutyCycle(dutyCyclePct)
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
```

`PWMFreq` and `SetPWMFreq` continue to return `errUnimplemented` — leave them alone.

`ensureMode` already accepts any `firmata.PinMode`. The existing branch in `ensureMode` enables digital reporting on `INPUT`/`INPUT_PULLUP`; for `PinModePWM` that's a no-op, which is correct.

- [ ] **Step 4: Run the new tests**

Run: `go test -run "TestSetPWM|TestPWM_Defaults" -race`
Expected: PASS.

- [ ] **Step 5: Run the whole suite (still expecting the v1 unimplemented-error test to regress)**

Run: `go test -run "^(TestConfig|TestResolve|TestAnalog|TestGPIOPin|TestSet|TestPWM)" -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "$(cat <<'EOF'
feat(board): firmataGPIOPin.SetPWM + PWM

SetPWM rejects pins not advertised as PWM-capable in the firmware's
CAPABILITY_RESPONSE, validates duty cycle 0..1, lazily switches the pin
to PWM mode, and emits the right wire form (ANALOG_MESSAGE for pin <=15,
EXTENDED_ANALOG sysex for pin >=16). PWM() returns the cached last-written
duty, defaulting to zero. PWMFreq/SetPWMFreq remain unimplemented because
Firmata has no spec for runtime frequency control.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 16: Board — extend `NewBoard` to wire up capabilities and analogs

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board.go`
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board_test.go`

This is the only constructor-level change. The existing `newBoardFromClient` stays as-is (it's the test-only direct injection point used by `newTestBoard`/`newTestBoardWithCaps`).

- [ ] **Step 1: Write the failing test**

We need a different harness to drive the full constructor end-to-end over a pipe. Add a new helper and one cohesive integration test.

Add to `firmata_board_test.go`:

```go
// runConstructorWithFakeFirmware builds a *firmataBoard via the production
// path (newBoard equivalent that takes an io.ReadWriteCloser injected via a
// helper hook), with the fake firmware writing handshake + capability +
// analog-mapping responses sequentially. Returns the constructed board, the
// arduino-side writer (for further frame injection), and a buffer that
// captures all bytes the board ever wrote.
//
// We can't reuse newBoardFromClient here because the goal is to exercise the
// query path. Instead, we replicate NewBoard's body but skip serial.Open and
// the DTR dance.
func newConstructorTestBoard(
	t *testing.T,
	cfg *Config,
	caps firmata.CapabilityResponse,
	amap firmata.AnalogMappingResponse,
) (*firmataBoard, io.WriteCloser, *bytes.Buffer) {
	t.Helper()

	arduinoR, arduinoW := io.Pipe()
	sentBuf := &bytes.Buffer{}
	rw := &rwFake{r: arduinoR, w: sentBuf}
	c := firmata.New(rw)

	// Fake firmware: respond to whatever the board asks for, in order.
	go func() {
		// REPORT_VERSION 2.5 (the board's Handshake)
		_, _ = arduinoW.Write([]byte{0xF9, 0x02, 0x05})
		// CAPABILITY_RESPONSE
		_, _ = arduinoW.Write(encodeCapabilityResponseForTest(caps))
		// ANALOG_MAPPING_RESPONSE
		_, _ = arduinoW.Write(encodeAnalogMappingResponseForTest(amap))
	}()

	hsCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, _, err := c.Handshake(hsCtx); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	gotCaps, err := c.QueryCapabilities(hsCtx)
	if err != nil {
		t.Fatalf("QueryCapabilities: %v", err)
	}
	gotAmap, err := c.QueryAnalogMapping(hsCtx)
	if err != nil {
		t.Fatalf("QueryAnalogMapping: %v", err)
	}

	if cfg.SamplingIntervalMs > 0 {
		if err := c.SetSamplingInterval(cfg.SamplingIntervalMs); err != nil {
			t.Fatalf("SetSamplingInterval: %v", err)
		}
	}

	name := board.Named("test")
	b := newBoardFromClient(name, c, rw, logging.NewTestLogger(t))
	b.capabilities = gotCaps
	b.analogMap = gotAmap

	for _, ac := range cfg.Analogs {
		dpin, ch, err := parseAnalogPin(ac.Pin)
		if err != nil {
			t.Fatalf("parseAnalogPin: %v", err)
		}
		dpin, ch, err = resolveAnalogPin(dpin, ch, gotAmap)
		if err != nil {
			t.Fatalf("resolveAnalogPin(%q): %v", ac.Pin, err)
		}
		if !b.pinSupports(dpin, firmata.PinModeAnalog) {
			t.Fatalf("analog %q: pin %d not analog-capable", ac.Name, dpin)
		}
		b.analogs[ac.Name] = &firmataAnalog{
			board: b, name: ac.Name, digitalPin: dpin, channel: uint8(ch),
		}
		b.ownedPins[dpin] = ac.Name
	}

	t.Cleanup(func() {
		_ = arduinoW.Close()
		_ = b.Close(context.Background())
	})

	return b, arduinoW, sentBuf
}

// encodeCapabilityResponseForTest re-serializes a CapabilityResponse using
// the wire format. Used only by tests to feed a fake firmware reply.
func encodeCapabilityResponseForTest(cr firmata.CapabilityResponse) []byte {
	out := []byte{0xF0, 0x6C}
	for _, p := range cr.Pins {
		// Iterate in a deterministic order so test diffs are stable.
		modes := make([]firmata.PinMode, 0, len(p))
		for m := range p {
			modes = append(modes, m)
		}
		// Simple ascending sort by mode value.
		for i := 1; i < len(modes); i++ {
			for j := i; j > 0 && modes[j] < modes[j-1]; j-- {
				modes[j], modes[j-1] = modes[j-1], modes[j]
			}
		}
		for _, m := range modes {
			out = append(out, uint8(m), p[m])
		}
		out = append(out, 0x7F)
	}
	out = append(out, 0xF7)
	return out
}

func encodeAnalogMappingResponseForTest(am firmata.AnalogMappingResponse) []byte {
	out := []byte{0xF0, 0x6A}
	out = append(out, am.ChannelByPin...)
	out = append(out, 0xF7)
	return out
}

func TestConstructor_EmitsSamplingIntervalWhenConfigured(t *testing.T) {
	cfg := &Config{
		SerialPath:         "/dev/null", // unused (we bypass serial.Open)
		SamplingIntervalMs: 50,
	}
	_, _, sentBuf := newConstructorTestBoard(t, cfg, unoCaps(), unoAnalogMap())

	// The wire bytes will include the queries plus the sampling-interval sysex.
	// We just check that a SAMPLING_INTERVAL frame appears.
	want := encodeSamplingIntervalForTest(50)
	if !bytes.Contains(sentBuf.Bytes(), want) {
		t.Errorf("expected SAMPLING_INTERVAL frame % X in % X", want, sentBuf.Bytes())
	}
}

func encodeSamplingIntervalForTest(ms uint16) []byte {
	return []byte{0xF0, 0x7A, uint8(ms & 0x7F), uint8((ms >> 7) & 0x7F), 0xF7}
}

func TestConstructor_BuildsAnalogReaders(t *testing.T) {
	cfg := &Config{
		SerialPath: "/dev/null",
		Analogs:    []board.AnalogReaderConfig{{Name: "x", Pin: "A0"}},
	}
	b, _, _ := newConstructorTestBoard(t, cfg, unoCaps(), unoAnalogMap())

	a, err := b.AnalogByName("x")
	if err != nil {
		t.Fatalf("AnalogByName: %v", err)
	}
	if a == nil {
		t.Fatal("AnalogByName returned nil analog")
	}
}

func TestConstructor_RejectsAnalogOnNonAnalogPin(t *testing.T) {
	// Custom capability: pin 2 supports INPUT/OUTPUT but not analog.
	caps := firmata.CapabilityResponse{Pins: make([]firmata.PinCapabilities, 4)}
	caps.Pins[2] = firmata.PinCapabilities{firmata.PinModeOutput: 1}
	amap := firmata.AnalogMappingResponse{ChannelByPin: []uint8{0x7F, 0x7F, 0x7F, 0x7F}}

	cfg := &Config{
		SerialPath: "/dev/null",
		Analogs:    []board.AnalogReaderConfig{{Name: "bad", Pin: "2"}},
	}

	// We deliberately use a different harness here: the helper above calls
	// t.Fatalf on resolution failures, so wrap it in a sub-test with a
	// custom harness that checks for the expected error.
	arduinoR, arduinoW := io.Pipe()
	defer arduinoW.Close()
	sentBuf := &bytes.Buffer{}
	rw := &rwFake{r: arduinoR, w: sentBuf}
	c := firmata.New(rw)
	defer c.Close()

	go func() {
		_, _ = arduinoW.Write([]byte{0xF9, 0x02, 0x05})
		_, _ = arduinoW.Write(encodeCapabilityResponseForTest(caps))
		_, _ = arduinoW.Write(encodeAnalogMappingResponseForTest(amap))
	}()

	hsCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, _ = c.Handshake(hsCtx)
	gotCaps, _ := c.QueryCapabilities(hsCtx)
	gotAmap, _ := c.QueryAnalogMapping(hsCtx)

	// Replicate the analog construction loop and assert it errors.
	for _, ac := range cfg.Analogs {
		dpin, ch, err := parseAnalogPin(ac.Pin)
		if err != nil {
			t.Fatalf("parseAnalogPin: %v", err)
		}
		dpin, ch, err = resolveAnalogPin(dpin, ch, gotAmap)
		if err == nil && len(gotCaps.Pins) > dpin {
			if _, ok := gotCaps.Pins[dpin][firmata.PinModeAnalog]; !ok {
				err = fmt.Errorf("pin %d does not support analog", dpin)
			}
		}
		if err == nil {
			t.Fatalf("expected analog %q to be rejected; got nil", ac.Name)
		}
		_ = ch
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test -run "TestConstructor" -race`
Expected: FAIL — most pieces compile, but the SetSamplingInterval may be missing from the helper or `Analogs` go uninitialized.

- [ ] **Step 3: Update the production `NewBoard` to perform the queries and build analogs**

In `firmata_board.go`, replace the body of `NewBoard` (after the existing handshake) so it:
1. Runs `QueryCapabilities` and `QueryAnalogMapping` against `hsCtx`.
2. Optionally calls `SetSamplingInterval`.
3. Builds the `analogs`/`ownedPins` maps via `parseAnalogPin` + `resolveAnalogPin` + capability check.

```go
// after Handshake succeeds:
caps, err := c.QueryCapabilities(hsCtx)
if err != nil {
	_ = c.Close()
	_ = sp.Close()
	return nil, fmt.Errorf("capability query on %s: %w", cfg.SerialPath, err)
}
amap, err := c.QueryAnalogMapping(hsCtx)
if err != nil {
	_ = c.Close()
	_ = sp.Close()
	return nil, fmt.Errorf("analog mapping query on %s: %w", cfg.SerialPath, err)
}

if cfg.SamplingIntervalMs > 0 {
	if err := c.SetSamplingInterval(cfg.SamplingIntervalMs); err != nil {
		_ = c.Close()
		_ = sp.Close()
		return nil, fmt.Errorf("set sampling interval on %s: %w", cfg.SerialPath, err)
	}
}

b := newBoardFromClient(conf.ResourceName(), c, sp, logger)
b.capabilities = caps
b.analogMap = amap

for _, ac := range cfg.Analogs {
	dpin, ch, err := parseAnalogPin(ac.Pin)
	if err != nil {
		_ = b.Close(ctx)
		return nil, fmt.Errorf("analog %q: %w", ac.Name, err)
	}
	dpin, ch, err = resolveAnalogPin(dpin, ch, amap)
	if err != nil {
		_ = b.Close(ctx)
		return nil, fmt.Errorf("analog %q: %w", ac.Name, err)
	}
	if !b.pinSupports(dpin, firmata.PinModeAnalog) {
		_ = b.Close(ctx)
		return nil, fmt.Errorf("analog %q: pin %d does not support analog mode", ac.Name, dpin)
	}
	if ac.SamplesPerSecond != 0 {
		logger.Warnf("firmata board: analog %q samples_per_sec=%d ignored — Firmata only supports a global sampling_interval_ms",
			ac.Name, ac.SamplesPerSecond)
	}
	b.analogs[ac.Name] = &firmataAnalog{
		board: b, name: ac.Name, digitalPin: dpin, channel: uint8(ch),
	}
	b.ownedPins[dpin] = ac.Name
}

return b, nil
```

- [ ] **Step 4: Run the new tests**

Run: `go test -run TestConstructor -race`
Expected: PASS.

- [ ] **Step 5: Run the whole suite (still expecting the v1 unimplemented-error test to regress)**

Run: `go test -race ./...`
Expected: only `TestUnimplementedMethods_ReturnSentinelError` fails (we'll fix it next).

- [ ] **Step 6: Commit**

```bash
git add firmata_board.go firmata_board_test.go
git commit -m "$(cat <<'EOF'
feat(board): NewBoard issues capability + analog-mapping queries, builds analogs

After Handshake, the constructor sends CAPABILITY_QUERY and
ANALOG_MAPPING_QUERY (both bounded by the existing handshake_timeout),
optionally SAMPLING_INTERVAL, then resolves each analogs[] entry against
the firmware's analog map and capability table. Capability mismatches are
surfaced as construction errors with the resource cleanly closed.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 17: Update `TestUnimplementedMethods_ReturnSentinelError`

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/firmata_board_test.go`

The v1 test asserts a sentinel-error contract for methods that are now implemented. Tighten it to the methods that genuinely remain unimplemented in this scope: `PWMFreq`, `SetPWMFreq`, `DigitalInterruptByName`, `SetPowerMode`, `StreamTicks`. Drop the assertions for `PWM`, `SetPWM`, `AnalogByName` (their behavior is verified by the new analog/PWM tests).

- [ ] **Step 1: Replace the test body**

Find `TestUnimplementedMethods_ReturnSentinelError` and replace it with:

```go
func TestUnimplementedMethods_ReturnSentinelError(t *testing.T) {
	tb := newTestBoard(t)
	defer tb.cleanup()
	ctx := context.Background()

	// PWM frequency control is genuinely unimplemented — Firmata has no
	// runtime-frequency wire spec.
	pin, _ := tb.b.GPIOPinByName("5")
	if _, err := pin.PWMFreq(ctx, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("PWMFreq: want errUnimplemented, got %v", err)
	}
	if err := pin.SetPWMFreq(ctx, 1000, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("SetPWMFreq: want errUnimplemented, got %v", err)
	}

	// Digital interrupts ship in a separate spec; today they remain unimplemented.
	if _, err := tb.b.DigitalInterruptByName("d0"); !errors.Is(err, errUnimplemented) {
		t.Errorf("DigitalInterruptByName: want errUnimplemented, got %v", err)
	}

	// SetPowerMode and StreamTicks remain explicit non-goals.
	if err := tb.b.SetPowerMode(ctx, 0, nil, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("SetPowerMode: want errUnimplemented, got %v", err)
	}
	if err := tb.b.StreamTicks(ctx, nil, nil, nil); !errors.Is(err, errUnimplemented) {
		t.Errorf("StreamTicks: want errUnimplemented, got %v", err)
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test -run TestUnimplementedMethods_ReturnSentinelError -race`
Expected: PASS.

- [ ] **Step 3: Run the whole suite**

Run: `go test -race ./...`
Expected: PASS — full green.

- [ ] **Step 4: Commit**

```bash
git add firmata_board_test.go
git commit -m "$(cat <<'EOF'
test(board): tighten errUnimplemented contract to PWMFreq/interrupt/power/stream

PWM, SetPWM, and AnalogByName are now implemented and have positive
coverage elsewhere in the suite, so they're removed from the sentinel
list. The remaining methods (PWMFreq/SetPWMFreq, DigitalInterruptByName,
SetPowerMode, StreamTicks) still return errUnimplemented intentionally.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 18: README — analog + PWM section, scope update

**Files:**
- Modify: `/Users/nick.hehr/src/viam-firmata/README.md`

- [ ] **Step 1: Update the "Scope (v1)" section**

Find the `### Scope (v1)` section in `README.md` and replace it with:

```markdown
### Scope

This release supports **digital GPIO**, **analog reads**, and **PWM**. The
following board API methods still return an "unimplemented" error and will
surface as configuration or runtime errors if you try to use them:

- Digital interrupts (`DigitalInterruptByName`, `StreamTicks`)
- PWM frequency control (`PWMFreq`, `SetPWMFreq`) — Firmata has no spec for
  runtime frequency control. Most AVR PWM pins run at ~490 Hz; some timer-1
  pins run at ~980 Hz. If you need a specific frequency, you'll have to
  patch the Firmata sketch on the Arduino side.
- Power-mode control (`SetPowerMode`)

Use `GPIOPinByName(name).Set(ctx, high, ...)` / `.Get(ctx, ...)` /
`.SetPWM(ctx, duty, ...)` — pin names are the digital pin numbers as strings
(`"2"`, `"13"`, ...). Use `AnalogByName(name)` for analog readers declared in
the `analogs[]` config.
```

- [ ] **Step 2: Add the analog + PWM attributes documentation**

After the existing `### Attributes` table in `README.md`, add the two new optional fields and a new section. First update the `Attributes` table to include the new optional fields. Replace the existing table with:

```markdown
### Attributes

The following attributes are available for the board component:

| Name                    | Type     | Inclusion    | Description                                                                                                                                                                                                  |
| ----------------------- | -------- | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `serial_path`           | string   | **Required** | Path to the USB-serial device the Arduino enumerates as (see [Communication](#communication) below).                                                                                                         |
| `baud_rate`             | int      | Optional     | Serial baud rate for communication with the Arduino. Default is `57600` to match ConfigurableFirmata's stock sketch.                                                                                          |
| `auto_reset_delay`      | duration | Optional     | Time to wait after toggling DTR for the Arduino's bootloader to hand off to the sketch. Default is `2s`. Accepts any string parseable by [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration).      |
| `handshake_timeout`     | duration | Optional     | How long to wait for the Firmata `REPORT_VERSION` reply before giving up. Default is `5s`.                                                                                                                   |
| `sampling_interval_ms`  | int      | Optional     | Global firmware-side analog sampling interval in milliseconds (1..16383). When unset, the firmware default applies (typically 19ms on AVR). Applies to *all* enabled analog reports — Firmata has no per-pin rate. |
| `analogs`               | array    | Optional     | List of analog reader declarations — see [Analog readers](#analog-readers) below. Each entry has `name` (used by `AnalogByName`) and `pin` (`"A0"`-style or a raw digital-pin number).                         |

### Analog readers

Declare each analog input you want exposed to Viam as an entry in `analogs`:

```json
{
  "serial_path": "/dev/tty.usbmodem14201",
  "sampling_interval_ms": 50,
  "analogs": [
    { "name": "joy_x", "pin": "A0" },
    { "name": "thermistor", "pin": "15" }
  ]
}
```

`pin` accepts either the silkscreen alias (`"A0"`, `"A1"`, ...) or the raw
digital-pin number (`"14"` is the same as `"A0"` on an Uno). On first `Read`,
the module sends `SET_PIN_MODE(ANALOG)` and `REPORT_ANALOG`; subsequent
`Read` calls return the cached 10-bit value (`Min=0`, `Max=1023`,
`StepSize=5/1024 V`).

A pin declared in `analogs` is *owned* by that reader: calling
`GPIOPinByName(...).Set/Get/SetPWM` on it returns a clear error rather than
silently flipping the pin out of analog mode. Choose a different pin if you
need it as a GPIO.

> **Note:** the `samples_per_sec` field on individual `analogs[]` entries is
> accepted for forward compatibility with the Viam `AnalogReaderConfig`
> schema, but it is **ignored** — Firmata only supports the global
> `sampling_interval_ms` setting above.

### PWM

Any pin advertised as PWM-capable in the firmware's `CAPABILITY_RESPONSE`
can be driven via `GPIOPinByName(name).SetPWM(ctx, duty, nil)` — duty is a
float in `0.0..1.0`. The current duty is cached and returned by `PWM(ctx, nil)`.

PWM frequency on standard ConfigurableFirmata builds is fixed by the
Arduino's hardware timers (~490 Hz on most pins, ~980 Hz on timer-1 pins).
`PWMFreq`/`SetPWMFreq` return an "unimplemented" error.
```

- [ ] **Step 3: Update the "Local install" sample config**

Find the existing JSON example under `#### Local install` and add a couple of analog/PWM-friendly fields so users can copy a more interesting starting point:

```json
{
  "modules": [
    {
      "type": "local",
      "name": "firmata",
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
        "sampling_interval_ms": 50,
        "analogs": [
          { "name": "joy_x", "pin": "A0" }
        ]
      }
    }
  ]
}
```

- [ ] **Step 4: Update the design-docs section at the bottom**

Append a row referencing the new spec and plan:

```markdown
- Spec (analog + PWM): [`docs/superpowers/specs/2026-04-28-viam-firmata-analog-pwm-design.md`](docs/superpowers/specs/2026-04-28-viam-firmata-analog-pwm-design.md)
- Plan (analog + PWM): [`docs/superpowers/plans/2026-04-28-viam-firmata-analog-pwm.md`](docs/superpowers/plans/2026-04-28-viam-firmata-analog-pwm.md)
```

- [ ] **Step 5: Run the suite (sanity)**

Run: `go test -race ./...`
Expected: PASS — README changes alone shouldn't break anything but a quick check makes sure no test was implicitly relying on README contents (none should).

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -m "$(cat <<'EOF'
docs(readme): document analog readers, PWM, sampling_interval_ms

Updates the scope section now that v2 ships analog + PWM, adds an
"Analog readers" section with config schema and an explanation of pin
ownership, documents the fixed-PWM-frequency caveat, and links the new
spec/plan from the bottom-of-page index.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Final verification

- [ ] **Step 1: Run the whole suite with `-race`**

Run: `go test -race ./...`
Expected: PASS, no race warnings.

- [ ] **Step 2: Run `go vet`**

Run: `go vet ./...`
Expected: clean.

- [ ] **Step 3: Run `go build` for the module entrypoint**

Run: `go build ./cmd/module && go build ./cmd/firmata-poc`
Expected: both succeed.

- [ ] **Step 4: Verify the implementation against the spec**

Open `docs/superpowers/specs/2026-04-28-viam-firmata-analog-pwm-design.md` and walk through Section 7 (deliverables checklist). Confirm each item is reflected in committed code or docs.

- [ ] **Step 5 (optional): Hardware smoke-test**

If you have an Arduino Uno running ConfigurableFirmata wired up:

1. Wire pin 9 to an LED (with appropriate resistor) and pin A0 to a potentiometer wiper.
2. Build the module: `make build`
3. Start a local `viam-server` with a config like the README's `Analog readers` example, `analogs[].pin: "A0"`, plus a script (Go SDK or Viam app shell) that calls `pin.SetPWM(ctx, 0.5)` for pin 9 and `analog.Read(ctx)` for `joy_x`.
4. Verify the LED settles at half-brightness and the analog reading tracks the potentiometer.

This step is not gating — pipe-based tests fully exercise the wire protocol.
