package firmata

import (
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
