package firmata

import (
	"bufio"
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

// writeFrame sends an already-encoded frame, honoring writeMu and surfacing
// any prior reader-side error.
func (c *Client) writeFrame(frame []byte) error {
	if errp := c.readErr.Load(); errp != nil {
		return fmt.Errorf("firmata: stream closed: %w", *errp)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.rw.Write(frame)
	return err
}
