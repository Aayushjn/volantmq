package transport

import (
	"errors"
	"os"

	"github.com/VolantMQ/volantmq/auth"
	"github.com/VolantMQ/volantmq/metrics"
	"github.com/quic-go/quic-go"
)

// Conn is wrapper to quic.Stream
// implemented to encapsulate bytes statistic
type Conn interface {
	quic.Stream
}

type conn struct {
	quic.Stream
	stat metrics.Bytes
}

var _ Conn = (*conn)(nil)

// Handler ...
type Handler interface {
	OnConnection(Conn, *auth.Manager) error
}

func newConn(st quic.Stream, stat metrics.Bytes) *conn {
	c := &conn{
		Stream: st,
		stat:   stat,
	}

	return c
}

// Read ...
func (c *conn) Read(b []byte) (int, error) {
	n, err := c.Stream.Read(b)

	c.stat.OnRecv(n)

	return n, err
}

// Write ...
func (c *conn) Write(b []byte) (int, error) {
	n, err := c.Stream.Write(b)
	c.stat.OnSent(n)

	return n, err
}

// File ...
func (c *conn) File() (*os.File, error) {
	// if t, ok := c.Conn.(*net.TCPConn); ok {
	// 	return t.File()
	// }

	return nil, errors.New("not implemented")
}
