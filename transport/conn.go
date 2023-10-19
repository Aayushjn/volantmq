package transport

import (
	"errors"
	"net"
	"os"

	"github.com/VolantMQ/volantmq/auth"
	"github.com/VolantMQ/volantmq/metrics"
	"github.com/quic-go/quic-go"
)

// Conn is wrapper to net.Conn
// implemented to encapsulate bytes statistic
type Conn interface {
	net.Conn
}

type conn struct {
	net.Conn
	stat metrics.Bytes
}

type QuicConn interface {
	quic.Stream
}

type quicConn struct {
	quic.Stream
	stat metrics.Bytes
}

var _ Conn = (*conn)(nil)

// Handler ...
type Handler interface {
	OnConnection(Conn, *auth.Manager) error
	OnQuicConnection(quic.Stream, *auth.Manager) error
}

func newConn(cn net.Conn, stat metrics.Bytes) *conn {
	c := &conn{
		Conn: cn,
		stat: stat,
	}

	return c
}

func newQuicConn(stream quic.Stream, stat metrics.Bytes) *quicConn {
	return &quicConn{
		Stream: stream,
		stat:   stat,
	}
}

// Read ...
func (c *conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)

	c.stat.OnRecv(n)

	return n, err
}

func (c *quicConn) Read(b []byte) (int, error) {
	n, err := c.Stream.Read(b)
	c.stat.OnRecv(n)
	return n, err
}

// Write ...
func (c *conn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.stat.OnSent(n)

	return n, err
}

func (c *quicConn) Write(b []byte) (int, error) {
	n, err := c.Stream.Write(b)
	c.stat.OnSent(n)
	return n, err
}

// File ...
func (c *conn) File() (*os.File, error) {
	if t, ok := c.Conn.(*net.TCPConn); ok {
		return t.File()
	}

	return nil, errors.New("not implemented")
}
