// Package firmataboard provides a Viam board component backed by a device
// running ConfigurableFirmata over USB serial. See
// docs/superpowers/specs/2026-04-21-viam-firmata-board-module-design.md.
package firmataboard

import (
	"errors"
	"fmt"
	"time"

	"go.viam.com/rdk/resource"
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
