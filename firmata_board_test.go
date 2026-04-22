package firmataboard

import (
	"errors"
	"testing"
	"time"
)

func TestConfig_Validate(t *testing.T) {
	t.Run("missing serial_path", func(t *testing.T) {
		c := &Config{}
		_, _, err := c.Validate("components.0")
		if err == nil {
			t.Fatal("expected error for missing serial_path, got nil")
		}
	})

	t.Run("negative baud_rate", func(t *testing.T) {
		c := &Config{SerialPath: "/dev/null", BaudRate: -1}
		_, _, err := c.Validate("components.0")
		if err == nil {
			t.Fatal("expected error for negative baud_rate, got nil")
		}
	})

	t.Run("minimal valid config", func(t *testing.T) {
		c := &Config{SerialPath: "/dev/null"}
		req, opt, err := c.Validate("components.0")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(req) != 0 || len(opt) != 0 {
			t.Fatalf("unexpected deps: req=%v opt=%v", req, opt)
		}
	})

	t.Run("fully-populated valid config", func(t *testing.T) {
		c := &Config{
			SerialPath:       "/dev/ttyACM0",
			BaudRate:         57600,
			AutoResetDelay:   2 * time.Second,
			HandshakeTimeout: 5 * time.Second,
		}
		if _, _, err := c.Validate("components.0"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	_ = errors.New // keep import used if we add error-type assertions later
}
