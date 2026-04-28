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

| Name                | Type     | Inclusion    | Description                                                                                                                                                                                                  |
| ------------------- | -------- | ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `serial_path`       | string   | **Required** | Path to the USB-serial device the Arduino enumerates as (see [Communication](#communication) below).                                                                                                         |
| `baud_rate`         | int      | Optional     | Serial baud rate for communication with the Arduino. Default is `57600` to match ConfigurableFirmata's stock sketch.                                                                                          |
| `auto_reset_delay`  | duration | Optional     | Time to wait after toggling DTR for the Arduino's bootloader to hand off to the sketch. Default is `2s`. Accepts any string parseable by [`time.ParseDuration`](https://pkg.go.dev/time#ParseDuration).      |
| `handshake_timeout` | duration | Optional     | How long to wait for the Firmata `REPORT_VERSION` reply before giving up. Default is `5s`.                                                                                                                   |

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
        "serial_path": "/dev/tty.usbmodem14201"
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

### Scope (v1)

This release supports **digital GPIO only**. The following board API methods
return an "unimplemented" error and will surface as configuration or runtime
errors if you try to use them:

- Analog reads (`AnalogByName`)
- PWM (`SetPWM`, `PWMFreq`, `SetPWMFreq`)
- Digital interrupts (`DigitalInterruptByName`, `StreamTicks`)
- Power-mode control (`SetPowerMode`)

Use `GPIOPinByName(name).Set(ctx, high, …)` and `.Get(ctx, …)` — pin names are
the digital pin numbers as strings (`"2"`, `"13"`, …).

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
