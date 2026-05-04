# viam-firmata

A Viam [`board`](https://docs.viam.com/components/board/) component backed by an
Arduino (or any AVR-class device) running
[ConfigurableFirmata](https://github.com/firmata/ConfigurableFirmata) over USB
serial.

Lets a machine running `viam-server` drive digital GPIO on the Arduino through
the standard board API — `GPIOPinByName(name).Set/Get` — over the same Firmata
connection used by the bundled `firmata-poc` CLI.

## Hardware prerequisites

- An Arduino Uno (or any AVR board — Uno is the tested target).
- A USB cable connecting the Arduino to the machine running `viam-server`.
- Whatever you want to drive: an LED on a digital output pin, a pushbutton on a
  digital input pin (the board configures `INPUT_PULLUP` so wire button → GND),
  etc.

## Flash ConfigurableFirmata

The Arduino must be running ConfigurableFirmata before `viam-server` can talk
to it. You only need to do this once per board.

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

If you don't have `arduino-cli`: on macOS `brew install arduino-cli`, otherwise
see the [arduino-cli install docs](https://arduino.github.io/arduino-cli/latest/installation/).

## Model devrel:firmata:board

A [`board`](https://docs.viam.com/components/board/) component implementation
backed by an Arduino running ConfigurableFirmata. Add the module and a board
component to your machine config (in [app.viam.com](https://app.viam.com),
under your machine's **CONFIGURE** tab, or by editing the JSON directly).

Follow the [Flash ConfigurableFirmata](#flash-configurablefirmata) steps above
before configuring this component for the first time.

### Configuration

On MacOS, the serial port path may look like this:
```json
{
  "serial_path": "/dev/tty.usbmodem14201"
}
```

On the Arduino UNO Q, the correct serial path is:
```json
{
  "serial_path": "/dev/ttyHS1"
}
```

On other Linux systems, the path could look like:
```json
{
  "serial_path": "/dev/ttyUSB0" // or /dev/ttyACM0
}
```

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
| `enable_diagnostics`    | bool     | Optional     | When true, emits firmware capability dumps, per-pin state probes after `SetPWM`/first analog `Read`, and forwards firmware `STRING_DATA` warnings — all at Debug level. Off by default.                          |

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

## Development

#### Local install

Build the binary from source, then point a local module at it:

```sh
make build
# → produces ./bin/viam-firmata
```

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

### Communication

The module talks to the Arduino over USB serial. Find the available serial
port from your machine's command line:

On macOS, look for `usbmodem` in the name:

```
you@machine: ls /dev/tty.*
/dev/tty.Bluetooth-Incoming-Port
/dev/tty.usbmodem14201
```

On Linux, look for `ACM` or `USB` in the name:

```
you@machine: ls /dev/tty*
/dev/ttyACM0
/dev/ttyUSB0
```

On Windows, look for `COM` in the name:

```
you@machine: mode
COM0
COM1
```

Or use `arduino-cli board list` to print the port and FQBN of every connected
Arduino.

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

## Troubleshooting

- **`handshake: ... no REPORT_VERSION received`** — wrong serial port path or
  the Arduino isn't running ConfigurableFirmata. Re-run `arduino-cli board list`
  to confirm the port, and re-flash if you uploaded a different sketch.
- **`open /dev/tty…: permission denied`** — on Linux, add the user that runs
  `viam-server` to the `dialout` (Debian/Ubuntu) or `uucp` (Arch) group, then
  log out/in.
- **Pin doesn't toggle** — the Arduino auto-resets on every connection, which
  takes ~2s. If your machine config changes often, increase `auto_reset_delay`.
  If toggling silently fails, check for another process holding the serial
  port (Arduino IDE serial monitor, `screen`, another `viam-server` instance).
- **Garbage bytes in logs around startup** — normal. The decoder skips
  non-command bytes until it sees a valid Firmata frame.

## Standalone PoC binary

The repo also ships a `firmata-poc` CLI that exercises the same internal
codec without `viam-server`. Useful for sanity-checking a board before
configuring it as a Viam component.

```sh
go run ./cmd/firmata-poc -port /dev/tty.usbmodem14201
# blinks pin 13 every 500ms for 10s and prints pin-2 changes

# Customize:
go run ./cmd/firmata-poc \
    -port /dev/tty.usbmodem14201 \
    -out-pin 13 \
    -in-pin 2 \
    -duration 15s \
    -toggle-interval 250ms
```

## Running the tests

The `internal/firmata` package and the board wiring are hardware-free and
fully unit-tested over `io.Pipe` fakes:

```sh
go test ./...
go test -race ./...
```

## Design docs

- Spec: [`docs/superpowers/specs/2026-04-21-viam-firmata-poc-design.md`](docs/superpowers/specs/2026-04-21-viam-firmata-poc-design.md)
- Plan: [`docs/superpowers/plans/2026-04-21-viam-firmata-poc.md`](docs/superpowers/plans/2026-04-21-viam-firmata-poc.md)
- Spec (analog + PWM): [`docs/superpowers/specs/2026-04-28-viam-firmata-analog-pwm-design.md`](docs/superpowers/specs/2026-04-28-viam-firmata-analog-pwm-design.md)
- Plan (analog + PWM): [`docs/superpowers/plans/2026-04-28-viam-firmata-analog-pwm.md`](docs/superpowers/plans/2026-04-28-viam-firmata-analog-pwm.md)
