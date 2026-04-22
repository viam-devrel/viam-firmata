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

	"go.bug.st/serial"
	pb "go.viam.com/api/component/board/v1"
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

	// mu serializes pin-mode bookkeeping. Held across blocking firmata I/O in
	// ensureMode, so all pin-mode changes are serialized board-wide. Acceptable
	// for v1 single-board, low-frequency GPIO. Follow-up: per-pin mutex for
	// finer-grained concurrency if this becomes a bottleneck.
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
		return nil, fmt.Errorf("firmata board: invalid pin name %q (want decimal 0-127)", name)
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
	if mode == firmata.PinModeInput || mode == firmata.PinModeInputPullup {
		if err := p.board.client.EnableDigitalReporting(p.pin/8, true); err != nil {
			return err
		}
	}
	p.board.pinModes[p.pin] = mode
	return nil
}

func (p *firmataGPIOPin) Set(_ context.Context, high bool, _ map[string]any) error {
	if err := p.ensureMode(firmata.PinModeOutput); err != nil {
		return err
	}
	return p.board.client.DigitalWrite(p.pin, high)
}

// Get configures the pin as INPUT_PULLUP on first call (which also enables
// per-port reporting) and returns the latest cached state from the firmata
// reader goroutine. No I/O happens on subsequent calls once the pin mode is
// cached.
func (p *firmataGPIOPin) Get(_ context.Context, _ map[string]any) (bool, error) {
	if err := p.ensureMode(firmata.PinModeInputPullup); err != nil {
		return false, err
	}
	return p.board.client.ReadDigital(p.pin), nil
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

// NewBoard is the constructor registered with the Viam resource manager.
// It opens the serial port, toggles DTR to reset the Arduino, waits for the
// auto-reset window, runs the Firmata handshake, and returns a live board.
func NewBoard(ctx context.Context, _ resource.Dependencies, conf resource.Config, logger logging.Logger) (board.Board, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}

	baud := cfg.BaudRate
	if baud == 0 {
		baud = 57600
	}
	resetDelay := cfg.AutoResetDelay
	if resetDelay == 0 {
		resetDelay = 2 * time.Second
	}
	hsTimeout := cfg.HandshakeTimeout
	if hsTimeout == 0 {
		hsTimeout = 5 * time.Second
	}

	sp, err := serial.Open(cfg.SerialPath, &serial.Mode{BaudRate: baud})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.SerialPath, err)
	}

	_ = sp.SetDTR(false)
	time.Sleep(100 * time.Millisecond)
	_ = sp.SetDTR(true)
	logger.Infof("firmata: waiting %s for auto-reset on %s", resetDelay, cfg.SerialPath)
	select {
	case <-time.After(resetDelay):
	case <-ctx.Done():
		_ = sp.Close()
		return nil, ctx.Err()
	}

	c := firmata.New(sp)

	hsCtx, cancel := context.WithTimeout(ctx, hsTimeout)
	major, minor, err := c.Handshake(hsCtx)
	cancel()
	if err != nil {
		_ = c.Close()
		_ = sp.Close()
		return nil, fmt.Errorf("handshake on %s: %w", cfg.SerialPath, err)
	}
	logger.Infof("firmata: connected to %s — firmware v%d.%d", cfg.SerialPath, major, minor)

	return newBoardFromClient(conf.ResourceName(), c, sp, logger), nil
}

// --- Board-level methods outside the v1 digital-GPIO scope ---

func (b *firmataBoard) AnalogByName(string) (board.Analog, error) {
	return nil, errUnimplemented
}

func (b *firmataBoard) DigitalInterruptByName(string) (board.DigitalInterrupt, error) {
	return nil, errUnimplemented
}

func (b *firmataBoard) SetPowerMode(_ context.Context, _ pb.PowerMode, _ *time.Duration, _ map[string]any) error {
	return errUnimplemented
}

func (b *firmataBoard) StreamTicks(_ context.Context, _ []board.DigitalInterrupt, _ chan board.Tick, _ map[string]any) error {
	return errUnimplemented
}

// Compile-time assertions that our types satisfy the full board interfaces.
// If RDK adds new methods, the build will fail here pointing to the missing one.
var (
	_ board.Board   = (*firmataBoard)(nil)
	_ board.GPIOPin = (*firmataGPIOPin)(nil)
)

func init() {
	resource.RegisterComponent(
		board.API, Model,
		resource.Registration[board.Board, *Config]{
			Constructor: NewBoard,
		},
	)
}
