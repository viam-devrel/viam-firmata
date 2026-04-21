// Command firmata-poc is a minimal proof-of-concept that drives a digital
// OUTPUT pin and streams pin-change events from a digital INPUT_PULLUP pin
// on an Arduino running ConfigurableFirmata, over USB serial.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"go.bug.st/serial"

	"github.com/viam-labs/viam-firmata/internal/firmata"
)

func main() {
	port := flag.String("port", "", "serial device path (e.g. /dev/tty.usbmodem14201) — required")
	baud := flag.Int("baud", 57600, "serial baud rate (ConfigurableFirmata default is 57600)")
	outPin := flag.Int("out-pin", 13, "digital pin to drive as OUTPUT (typically onboard LED)")
	inPin := flag.Int("in-pin", 2, "digital pin to read as INPUT_PULLUP")
	duration := flag.Duration("duration", 10*time.Second, "total run time")
	toggleInterval := flag.Duration("toggle-interval", 500*time.Millisecond, "how often to flip the output pin")
	flag.Parse()

	if *port == "" {
		fmt.Fprintln(os.Stderr, "error: -port is required")
		flag.Usage()
		os.Exit(2)
	}

	if err := run(*port, *baud, *outPin, *inPin, *duration, *toggleInterval); err != nil {
		log.Fatalf("firmata-poc: %v", err)
	}
}

func run(portPath string, baud, outPin, inPin int, duration, toggleInterval time.Duration) error {
	mode := &serial.Mode{BaudRate: baud}
	sp, err := serial.Open(portPath, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", portPath, err)
	}
	defer sp.Close()

	// Toggle DTR to trigger the Arduino bootloader reset, then wait for the
	// sketch to come up. The ~2s delay is intentionally hardcoded — the
	// Arduino bootloader's wait window is ~1.6s and this is not worth a flag.
	_ = sp.SetDTR(false)
	time.Sleep(100 * time.Millisecond)
	_ = sp.SetDTR(true)
	log.Println("waiting 2s for Arduino auto-reset...")
	time.Sleep(2 * time.Second)

	c := firmata.New(sp)
	defer c.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	hsCtx, hsCancel := context.WithTimeout(ctx, 5*time.Second)
	major, minor, err := c.Handshake(hsCtx)
	hsCancel()
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	log.Printf("connected — firmware Firmata v%d.%d", major, minor)

	if err := c.SetPinMode(outPin, firmata.PinModeOutput); err != nil {
		return fmt.Errorf("SetPinMode out: %w", err)
	}
	if err := c.SetPinMode(inPin, firmata.PinModeInputPullup); err != nil {
		return fmt.Errorf("SetPinMode in: %w", err)
	}
	if err := c.EnableDigitalReporting(inPin/8, true); err != nil {
		return fmt.Errorf("EnableDigitalReporting: %w", err)
	}

	runCtx, runCancel := context.WithTimeout(ctx, duration)
	defer runCancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ev := range c.Events() {
			level := "LOW"
			if ev.High {
				level = "HIGH"
			}
			log.Printf("pin %d -> %s", ev.Pin, level)
		}
	}()

	ticker := time.NewTicker(toggleInterval)
	defer ticker.Stop()
	var high bool
	log.Printf("driving pin %d every %s for %s (press ctrl-c to stop early)",
		outPin, toggleInterval, duration)
	for {
		select {
		case <-runCtx.Done():
			log.Println("run complete")
			_ = c.DigitalWrite(outPin, false) // leave the LED off
			// Close the client so the events loop exits.
			_ = c.Close()
			wg.Wait()
			return nil
		case <-ticker.C:
			high = !high
			if err := c.DigitalWrite(outPin, high); err != nil {
				return fmt.Errorf("DigitalWrite: %w", err)
			}
		}
	}
}
