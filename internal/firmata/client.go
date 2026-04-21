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
	rw         io.ReadWriteCloser
	br         *bufio.Reader
	portState  [16]uint8 // last-known input mask per port
	outState   [16]uint8 // last-written output mask per port
	events     chan PinChange
	version    chan VersionMessage
	readErr    atomic.Pointer[error]
	readerDone chan struct{}
	writeMu    sync.Mutex
	closed     atomic.Bool
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
func (c *Client) readLoop() {
	defer close(c.readerDone)
	defer close(c.events)
	for {
		msg, err := decode(c.br)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				c.readErr.Store(&err)
			}
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
		case UnknownMessage:
			// Ignore — sysex, analog reports we didn't subscribe to, etc.
		}
	}
}

// dispatchDigital diffs the new port mask against the last-known one
// and emits one PinChange per changed bit.
func (c *Client) dispatchDigital(m DigitalPortMessage) {
	prev := c.portState[m.Port]
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
	c.portState[m.Port] = m.Mask
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
