# DEVLOG — 2026-04-29 — Analog/PWM bring-up on Arduino UNO Q

Branch: `feat/analog-pwm`
Live machine: `arduino-q` (DevRelDemos / test) — part `3c19f111-f40b-403c-a39f-35f2d5ace1db`
Hardware: Arduino UNO Q (Zephyr-based, STM32U585AI, FQBN `arduino:zephyr:unoq`), `/dev/ttyHS1` from the Linux side
Firmware: ConfigurableFirmata (protocol v2.7) compiled against the AVR/Uno Q board block in `Boards.h`

## Reported symptoms

1. `AnalogByName("A0").Read()` against a TMP36 wired to A0 returned `12` repeatedly.
2. `GPIOPinByName("10").SetPWM(ctx, 0.6, nil)` did not light an LED. Same for pin 11 across duty 0.2 / 0.5 / 1.0.
3. `GPIOPinByName("10").Set(ctx, true, nil)` worked (LED solid on) — digital path was fine end-to-end.

## Investigation timeline

### Phase 1 — verify the wire protocol

Added a one-shot diagnostic dump in `initializeAnalogs` (`firmata_board.go`) that logs:
- `CAPABILITY_RESPONSE` — per-pin `(mode, resolution)` pairs.
- `ANALOG_MAPPING_RESPONSE` — full `ChannelByPin` table.
- Resolved `(name → digital pin, channel)` for each declared analog reader.

Findings on the live device:
- 52 pins reported; canonical Uno-shape analog mapping `[127×14, 0,1,2,3,4,5, 127×32]`, so `A0 → channel 0 → digital pin 14`.
- Pin 10 / 11 advertised `PWM@8b` — refuted the "we hardcode `*255` but firmware wants 12-bit" hypothesis.
- Pins 14-19 advertised `ANALOG@14b` — our `Max: 1023` / `StepSize: 5.0/1024` metadata in `firmataAnalog.Read` (firmata_board.go:401-406) is wrong (Uno Q is 14-bit, 3.3 V Vref) but doesn't affect the raw `Value`.
- No `STRING_DATA` warnings → firmware accepted everything.

### Phase 2 — verify the firmware actually applied modes

Surfaced firmware-side messages by:
- Adding `StringDataMessage` (sysex 0x71) and `PinStateResponse` (sysex 0x6E) decode + a `Diagnostics()` channel to `internal/firmata/client.go`.
- Adding a `QueryPinState` round-trip and a `diagnosticProbes` flag on the board (only `NewBoard` turns it on; unit tests stay strict).
- After `SetPinMode(PWM)` and `SetPinMode(ANALOG)`, the probe reads back the firmware's `pinConfig[pin]` and logs it.

Findings:
- After `SetPWM(11, 0.6)`: probe reports `firmware-mode=0x03 (PWM) state=0`. Mode bookkeeping is correct; CF's `pinState[]` is never written by `analogWrite`, so `state=0` is meaningless here, not a bug indicator.
- After `Read()` on A0: probe reports `firmware-mode=0x02 (ANALOG)`. First read returned a real-looking sample (133), every subsequent read returned a fixed 12 — analog reporting wasn't continuously updating.
- Cross-checked with `pyfirmata2` on the same `/dev/ttyHS1`: same failure mode → bug is in the firmware/core, not in our Go module.

### Phase 3 — root cause in ConfigurableFirmata's Zephyr handling

Inspected `../ConfigurableFirmata/` and the Arduino-Zephyr core on the device via `adb shell`. Three failure paths, all driven by the same architectural mismatch: **on Zephyr, GPIO and PWM/ADC are independent device drivers competing for the same physical pin via pinctrl, and pinctrl is only applied at driver init — not per `pwm_set_cycles` / `adc_read` call**. Once any code claims a pin via the GPIO driver, the alt-function MODER set up at Zephyr boot is gone, and subsequent `analogWrite` / `analogRead` updates go to the registers but never reach the pin.

CF's three offenders, all AVR-era patterns where `pinMode()` was a harmless no-op for register-level peripherals:

1. **`AnalogInputFirmata::handlePinMode`** (`src/AnalogInputFirmata.cpp:81-83`) — calls `pinMode(p, INPUT)` immediately after the first `reportAnalog` (which already did one `analogRead`). Explains 133→12: first sample real, subsequent samples come from a pin that's now claimed as GPIO INPUT.
2. **`AnalogOutputFirmata::setupPwmPin`** (`src/AnalogOutputFirmata.cpp:33-37`) — calls `pinMode(p, OUTPUT)` before `analogWrite(p, 0)`.
3. **`systemResetCallback` in the example sketch** (`examples/ConfigurableFirmata/ConfigurableFirmata.ino:127-145`) — runs once on connect and forces every digital pin into `PIN_MODE_OUTPUT`, which in turn calls `DigitalOutputFirmata::handlePinMode → digitalWrite(LOW); pinMode(OUTPUT)`. This is the silent killer for PWM: even after fix #2, the boot-time GPIO claim has already locked every PWM-capable pin.

Variant overlay (`arduino_uno_q_stm32u585xx.overlay`) confirmed via adb that:
- `pwm-pin-gpios` and `pwms` line up: `pwm_pin_index(11) = 5 → arduino_pwm[5] = <&pwm1 3 PWM_HZ(500) PWM_POLARITY_INVERTED>` (D11/PB15 → TIM1_CH3N, **inverted polarity** — duty 1.0 = LED off, duty 0.0 = LED full on).
- Pin 10 uses TIM4_CH4 with `PWM_POLARITY_NORMAL` — duties map as expected.
- `PWM_HZ(500)` → 2 ms period, plenty visible on an LED.

CF already has Zephyr awareness in two places (`ConfigurableFirmata.cpp:27` for `vsnprintf`, sketch line 21 for Servo), so the three-file patch fits an established pattern.

## Outcome

**Firmware patch:** `/Users/nick.hehr/src/viam-firmata/zephyr-uno-q-pwm-analog.patch` (68 lines, 3 hunks). Validated via `git apply --check` against the local CF clone. Apply with:

```sh
cd /Users/nick.hehr/src/ConfigurableFirmata
git apply /Users/nick.hehr/src/viam-firmata/zephyr-uno-q-pwm-analog.patch
```

Then re-flash via adb on the device (the Mac-side toolchain doesn't have arduino-cli):

```sh
adb shell 'arduino-cli compile --fqbn arduino:zephyr:unoq \
  ~/Arduino/libraries/ConfigurableFirmata/examples/ConfigurableFirmata && \
  arduino-cli upload --fqbn arduino:zephyr:unoq \
  --port /dev/ttyHS1 \
  ~/Arduino/libraries/ConfigurableFirmata/examples/ConfigurableFirmata'
```

**State after Patch 1 (AnalogInput) + Patch 2 (AnalogOutput) only:**
- Analog read: ✅ working — A0 returns ~220-226 (room temp, 14-bit, 3.3 V, TMP36).
- PWM: ❌ still broken — boot-time GPIO claim from Patch 3's target locks the pin.

**Expected state after all three patches applied + re-flash:**
- Analog read: ✅ continues working (Patch 1).
- PWM on D10/D3/D6/D9 (`PWM_POLARITY_NORMAL`): ✅ duty maps as expected.
- PWM on D5/D11 (`PWM_POLARITY_INVERTED` per variant overlay): ✅ functional, but inverted — duty 0.0 → 100 % high, duty 1.0 → 0 % high. Use D10 for sanity-checking.

## Module-side changes left in place on `feat/analog-pwm`

Reasonably small, no behavior change to production callers; safe to leave through the firmware verification cycle and decide later. All in `firmata_board.go` and `internal/firmata/`:

- `logCapabilities` + `pinModeName` helpers — one-shot Info dump on board init. Useful for future board bring-ups; cheap. **Decision pending.** Can downgrade to `Debugf` once Uno Q PWM is verified end-to-end.
- `firmataGPIOPin.SetPWM` and `firmataAnalog.Read` Info logs (`SetPWM pin=… duty=… raw=…`, `analog "x" (pin N, ch C) firmware-mode=…`) — gated on `firmataBoard.diagnosticProbes`, only `NewBoard` flips it on. Unit tests stay strict via `newBoardFromClient` leaving the flag false.
- `firmata.Client.Diagnostics() <-chan string` — surfaces `STRING_DATA` and otherwise-unhandled frames. Drained from `firmataBoard.drainDiagnostics` and logged at Warn. Useful safety net even after we're done debugging.
- `firmata.Client.QueryPinState` + `PinStateResponse` decode + `encodePinStateQuery` — full sysex 0x6D / 0x6E support. Worth keeping.
- New protocol decode: `StringDataMessage` (sysex 0x71). Worth keeping.

## Follow-ups (deferred, separate PRs)

1. **`firmata_board.go:401-406` analog metadata.** `Max: 1023` / `StepSize: 5.0/1024` should come from `b.capabilities.Pins[a.digitalPin][PinModeAnalog]` and a config-supplied `vref_volts` (default 5.0 for AVR Uno, 3.3 for Uno Q). Right now Uno Q clients see correct raw `Value` but wrong `Max` / `StepSize`. Cosmetic, not a bug, but easy to get right.
2. **PWM polarity awareness.** `CAPABILITY_RESPONSE` doesn't expose polarity, so a host-side library can't auto-detect inverted pins. Cleanest path: add a `pwm_inverted: true` flag on a per-pin config (parallel to `analogs[]`), and have `SetPWM` send `(1.0 - duty) * 255` for those pins. Useful only if anyone actually drives D5 or D11 from Viam.
3. **Upstream the firmware patch.** Title: `Zephyr Arduino core: don't claim PWM/ADC pins as GPIO`. CF maintainers have already accepted Servo gating for Zephyr (sketch line 21), so the fix shape is precedented. Reference this DEVLOG for the chain of evidence.
4. **Strip diagnostic verbosity** once Patch 3 lands and PWM is confirmed: downgrade `logCapabilities`/`SetPWM`/`Read` Info logs to Debug, drop the `diagnosticProbes` flag, and decide whether `Diagnostics()` stays or moves behind a flag too.

## Useful one-liners

```sh
# Pull recent firmata-related logs
viam machines logs --machine=arduino-q --org=DevRelDemos --location=test \
  --count=300 --keyword=firmata

# Reload the module to the live part after a code change
viam module reload --part-id 3c19f111-f40b-403c-a39f-35f2d5ace1db

# Inspect the device's CF source / Arduino-Zephyr core / variant overlay
adb shell 'ls /home/arduino/Arduino/libraries/ConfigurableFirmata/src/'
adb shell 'cat /home/arduino/.arduino15/packages/arduino/hardware/zephyr/0.51.0/variants/arduino_uno_q_stm32u585xx/arduino_uno_q_stm32u585xx.overlay'

# Re-flash CF on the device
adb shell 'arduino-cli compile --fqbn arduino:zephyr:unoq \
  ~/Arduino/libraries/ConfigurableFirmata/examples/ConfigurableFirmata'
```

## Update (2026-05-04)

Follow-up #4 is partially addressed: rather than dropping the diagnostic
surfaces, they're now opt-in behind a new `enable_diagnostics` config attribute
(default off). The `logCapabilities` dump, the `SetPWM`/first-`Read`
`PIN_STATE_QUERY` probes, and the `drainDiagnostics` goroutine all wait for
the operator to set it. The boolean `firmataBoard.diagnostics` field replaces
`diagnosticProbes` to reflect the broader scope. Gated logs were demoted from
Info to Debug, and the always-on per-analog "resolved to digital pin N,
channel C" line was demoted to Debug as well.
