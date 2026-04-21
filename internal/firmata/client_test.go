package firmata

import (
	"context"
	"io"
	"testing"
	"time"
)

// pipePair gives the test both ends of a bidirectional in-memory byte stream.
// host side is what the Client reads/writes. board side is what the "fake Arduino"
// in the test reads/writes.
type pipePair struct {
	host  io.ReadWriteCloser
	board io.ReadWriteCloser
}

type rwcWrapper struct {
	io.Reader
	io.Writer
	close func() error
}

func (w rwcWrapper) Close() error { return w.close() }

func newPipePair() *pipePair {
	boardR, hostW := io.Pipe() // host writes -> board reads
	hostR, boardW := io.Pipe() // board writes -> host reads
	host := rwcWrapper{
		Reader: hostR,
		Writer: hostW,
		close:  func() error { _ = hostW.Close(); _ = hostR.Close(); return nil },
	}
	board := rwcWrapper{
		Reader: boardR,
		Writer: boardW,
		close:  func() error { _ = boardW.Close(); _ = boardR.Close(); return nil },
	}
	return &pipePair{host: host, board: board}
}

func TestClientCloseStopsReader(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	// Closing the host side must unblock the reader and close Events().
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-c.Events():
		if ok {
			t.Fatal("expected Events channel to be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("reader goroutine did not exit within 1s")
	}
}

func TestHandshakeSucceeds(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	// Fake board sends a REPORT_VERSION 2.5 frame.
	go func() {
		_, _ = pp.board.Write([]byte{0xF9, 0x02, 0x05})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	major, minor, err := c.Handshake(ctx)
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if major != 2 || minor != 5 {
		t.Errorf("got %d.%d, want 2.5", major, minor)
	}
}

func TestHandshakeTimesOut(t *testing.T) {
	pp := newPipePair()
	c := New(pp.host)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := c.Handshake(ctx)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}
