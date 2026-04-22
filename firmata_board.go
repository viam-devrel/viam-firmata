// Package firmataboard provides a Viam board component backed by a device
// running ConfigurableFirmata over USB serial. See
// docs/superpowers/specs/2026-04-21-viam-firmata-board-module-design.md.
package firmataboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"github.com/viam-devrel/viam-firmata/internal/firmata"
)

// Model identifies this board implementation in the Viam registry.
var Model = resource.NewModel("devrel", "firmata", "board")

// errUnimplemented is returned by every board/GPIOPin method that is outside
// the v1 digital-GPIO-only scope. Tests use errors.Is to assert on it.
var errUnimplemented = errors.New("firmata board: method not implemented")

// Config is the attributes block for a devrel:firmata:board component.
type Config struct {
	SerialPath       string        `json:"serial_path"`
	BaudRate         int           `json:"baud_rate,omitempty"`
	AutoResetDelay   time.Duration `json:"auto_reset_delay,omitempty"`
	HandshakeTimeout time.Duration `json:"handshake_timeout,omitempty"`
}

// Validate checks required fields and reports that this board has no resource
// dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.SerialPath == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "serial_path")
	}
	if c.BaudRate < 0 {
		return nil, nil, resource.NewConfigValidationError(path,
			fmt.Errorf("baud_rate must be >= 0"))
	}
	return nil, nil, nil
}

type firmataBoard struct {
	resource.Named
	resource.AlwaysRebuild

	logger logging.Logger

	closer io.Closer // owns the underlying transport (real: serial port; tests: pipe)
	client *firmata.Client

	drainDone chan struct{}

	closeOnce sync.Once
	closeErr  error

	mu       sync.Mutex
	pinModes map[int]firmata.PinMode
}

// newBoardFromClient wires a *firmata.Client (and the io.Closer that owns the
// underlying transport) into a firmataBoard. Exposed at package scope so that
// firmata_board_test.go can drive the board over io.Pipe fakes without
// touching a real serial port.
func newBoardFromClient(name resource.Name, c *firmata.Client, closer io.Closer, logger logging.Logger) *firmataBoard {
	b := &firmataBoard{
		Named:     name.AsNamed(),
		logger:    logger,
		closer:    closer,
		client:    c,
		drainDone: make(chan struct{}),
		pinModes:  make(map[int]firmata.PinMode),
	}
	go b.drainEvents()
	return b
}

// drainEvents consumes and discards Client.Events() so the reader goroutine
// inside firmata.Client is never back-pressured. v1 does not expose events to
// Viam callers — all digital reads go through ReadDigital on the cached state.
func (b *firmataBoard) drainEvents() {
	defer close(b.drainDone)
	for range b.client.Events() {
	}
}

// Close tears down the client and the underlying transport and waits for the
// drain goroutine to exit. Idempotent.
func (b *firmataBoard) Close(_ context.Context) error {
	b.closeOnce.Do(func() {
		b.closeErr = b.client.Close() // unblocks reader → closes events → drain exits
		<-b.drainDone
		if cerr := b.closer.Close(); b.closeErr == nil {
			b.closeErr = cerr
		}
	})
	return b.closeErr
}

type firmataGPIOPin struct {
	board *firmataBoard
	pin   int
}

func (b *firmataBoard) GPIOPinByName(name string) (board.GPIOPin, error) {
	pin, err := strconv.Atoi(name)
	if err != nil || pin < 0 || pin > 127 {
		return nil, fmt.Errorf("firmata: invalid pin name %q (want decimal 0-127)", name)
	}
	return &firmataGPIOPin{board: b, pin: pin}, nil
}

// ensureMode sends SET_PIN_MODE only when the pin's cached mode differs from
// the requested one. For INPUT/INPUT_PULLUP it also enables per-port
// reporting on first configuration.
func (p *firmataGPIOPin) ensureMode(mode firmata.PinMode) error {
	p.board.mu.Lock()
	defer p.board.mu.Unlock()
	if current, ok := p.board.pinModes[p.pin]; ok && current == mode {
		return nil
	}
	if err := p.board.client.SetPinMode(p.pin, mode); err != nil {
		return err
	}
	p.board.pinModes[p.pin] = mode
	if mode == firmata.PinModeInput || mode == firmata.PinModeInputPullup {
		if err := p.board.client.EnableDigitalReporting(p.pin/8, true); err != nil {
			return err
		}
	}
	return nil
}

func (p *firmataGPIOPin) Set(_ context.Context, high bool, _ map[string]any) error {
	if err := p.ensureMode(firmata.PinModeOutput); err != nil {
		return err
	}
	return p.board.client.DigitalWrite(p.pin, high)
}

// Get is implemented in a later task; see firmata_board.go in subsequent tasks.
func (p *firmataGPIOPin) Get(_ context.Context, _ map[string]any) (bool, error) {
	return false, errUnimplemented
}

// PWM is intentionally unimplemented in v1; PWM support is out of scope.
func (p *firmataGPIOPin) PWM(_ context.Context, _ map[string]any) (float64, error) {
	return 0, errUnimplemented
}

// SetPWM is intentionally unimplemented in v1; PWM support is out of scope.
func (p *firmataGPIOPin) SetPWM(_ context.Context, _ float64, _ map[string]any) error {
	return errUnimplemented
}

// PWMFreq is intentionally unimplemented in v1; PWM support is out of scope.
func (p *firmataGPIOPin) PWMFreq(_ context.Context, _ map[string]any) (uint, error) {
	return 0, errUnimplemented
}

// SetPWMFreq is intentionally unimplemented in v1; PWM support is out of scope.
func (p *firmataGPIOPin) SetPWMFreq(_ context.Context, _ uint, _ map[string]any) error {
	return errUnimplemented
}
