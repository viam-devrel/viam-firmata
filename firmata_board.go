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
	"sync/atomic"
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
	SerialPath         string                     `json:"serial_path"`
	BaudRate           int                        `json:"baud_rate,omitempty"`
	AutoResetDelay     time.Duration              `json:"auto_reset_delay,omitempty"`
	HandshakeTimeout   time.Duration              `json:"handshake_timeout,omitempty"`
	SamplingIntervalMs int                        `json:"sampling_interval_ms,omitempty"`
	Analogs            []board.AnalogReaderConfig `json:"analogs,omitempty"`
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
		analogs:   map[string]*firmataAnalog{},
		ownedPins: map[int]string{},
		pwmDuty:   map[int]float64{},
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
	if owner, taken := p.board.ownedPins[p.pin]; taken {
		return fmt.Errorf("firmata board: pin %d is owned by analog %q", p.pin, owner)
	}
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
	if owner, taken := p.board.ownedPins[p.pin]; taken {
		return false, fmt.Errorf("firmata board: pin %d is owned by analog %q", p.pin, owner)
	}
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

	if err := sp.SetDTR(false); err != nil {
		logger.Debugf("firmata board: SetDTR(false) not supported: %v", err)
	}
	select {
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		_ = sp.Close()
		return nil, ctx.Err()
	}
	if err := sp.SetDTR(true); err != nil {
		logger.Debugf("firmata board: SetDTR(true) not supported: %v", err)
	}
	logger.Infof("firmata board: waiting %s for auto-reset on %s", resetDelay, cfg.SerialPath)
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
	logger.Infof("firmata board: connected to %s — firmware v%d.%d", cfg.SerialPath, major, minor)

	return newBoardFromClient(conf.ResourceName(), c, sp, logger), nil
}

// --- Board-level methods outside the v1 digital-GPIO scope ---

func (b *firmataBoard) AnalogByName(name string) (board.Analog, error) {
	a, ok := b.analogs[name]
	if !ok {
		return nil, fmt.Errorf("firmata board: no analog named %q", name)
	}
	return a, nil
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

// parseAnalogPin accepts "A0".."A15" or a raw digital pin number "0".."127".
// The unknown side of the pair is returned as the sentinel -1; resolveAnalogPin
// (called in NewBoard against the analog-mapping response) fills it in:
//
//	"A3"  -> (digitalPin: -1, analogChannel: 3)
//	"14"  -> (digitalPin: 14, analogChannel: -1)
func parseAnalogPin(s string) (digitalPin int, analogChannel int, err error) {
	if s == "" {
		return -1, -1, fmt.Errorf("analog pin name is empty")
	}
	if s[0] == 'A' || s[0] == 'a' {
		n, err := strconv.Atoi(s[1:])
		if err != nil || n < 0 || n > 15 {
			return -1, -1, fmt.Errorf("invalid analog pin %q (want \"A0\".. \"A15\")", s)
		}
		return -1, n, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 127 {
		return -1, -1, fmt.Errorf("invalid analog pin %q (want \"A0\".. \"A15\" or 0..127)", s)
	}
	return n, -1, nil
}

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
