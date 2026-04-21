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
