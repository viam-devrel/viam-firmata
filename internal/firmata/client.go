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
	rw          io.ReadWriteCloser
	br          *bufio.Reader
	portState   [16]uint8  // last-known input mask per port
	outState    [16]uint8  // last-written output mask per port
	analogState [16]uint16 // last-known value per analog channel
	events      chan PinChange
	version     chan VersionMessage
	readErr     atomic.Pointer[error]
	readerDone  chan struct{}
	writeMu     sync.Mutex
	stateMu     sync.RWMutex // guards portState for external readers
	closed      atomic.Bool
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
// EOF/ErrClosedPipe are only suppressed when Close() was called locally; an EOF
// from the remote end (e.g. Arduino disconnect) surfaces as a stream error.
func (c *Client) readLoop() {
	defer close(c.readerDone)
	defer close(c.events)
	for {
		msg, err := decode(c.br)
		if err != nil {
			if c.closed.Load() && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)) {
				return
			}
			c.readErr.Store(&err)
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
		case AnalogMessage:
			c.dispatchAnalog(m)
		case UnknownMessage:
			// Ignore — sysex, analog reports we didn't subscribe to, etc.
		}
	}
}

// dispatchDigital diffs the new port mask against the last-known one
// and emits one PinChange per changed bit.
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

// Handshake blocks until the firmware sends a REPORT_VERSION frame or ctx expires.
// It also proactively sends a REPORT_VERSION query, because some firmwares only
// send the version on request (not on reset).
func (c *Client) Handshake(ctx context.Context) (major, minor uint8, err error) {
	// Ask, in case the auto-emit was missed or the firmware doesn't send one on reset.
	// Send in a goroutine: io.Pipe (used in tests) blocks the write until the other end
	// reads, so we must not block the select on the outbound query.
	go func() { _ = c.writeFrame([]byte{cmdReportVersion}) }()
	select {
	case v := <-c.version:
		return v.Major, v.Minor, nil
	case <-ctx.Done():
		return 0, 0, fmt.Errorf("handshake: %w (no REPORT_VERSION received — wrong port or baud?)", ctx.Err())
	}
}

// writeFrame sends an already-encoded frame, honoring writeMu and surfacing
// any prior reader-side error. The readErr check runs inside the mutex so a
// late-arriving reader error can't slip between the check and the write.
func (c *Client) writeFrame(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if errp := c.readErr.Load(); errp != nil {
		return fmt.Errorf("firmata: stream closed: %w", *errp)
	}
	_, err := c.rw.Write(frame)
	return err
}

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

// DigitalWrite sets a single pin HIGH or LOW. It read-modify-writes the cached
// output mask for the pin's port, so multiple pins on the same port coexist.
func (c *Client) DigitalWrite(pin int, high bool) error {
	if pin < 0 || pin > 127 {
		return fmt.Errorf("pin %d out of range", pin)
	}
	port := uint8(pin / 8)
	bit := uint8(pin % 8)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if errp := c.readErr.Load(); errp != nil {
		return fmt.Errorf("firmata: stream closed: %w", *errp)
	}
	if high {
		c.outState[port] |= 1 << bit
	} else {
		c.outState[port] &^= 1 << bit
	}
	_, err := c.rw.Write(encodeDigitalPortWrite(port, c.outState[port]))
	return err
}

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
