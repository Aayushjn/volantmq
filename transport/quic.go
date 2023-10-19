package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/VolantMQ/volantmq/types"
	"github.com/quic-go/quic-go"
)

type ConfigQUIC struct {
	Scheme    string
	TLS       *tls.Config
	transport *Config
}

type quic_udp struct {
	baseConfig
	tls      *tls.Config
	listener *quic.EarlyListener
}

func NewConfigQUIC(transport *Config) *ConfigQUIC {
	return &ConfigQUIC{
		Scheme:    "udp",
		transport: transport,
	}
}

func NewQUIC(config *ConfigQUIC, internal *InternalConfig) (Provider, error) {
	l := &quic_udp{}
	l.quit = make(chan struct{})
	l.InternalConfig = *internal
	l.config = *config.transport
	l.tls = config.TLS

	var err error
	quicConfig := &quic.Config{
		Allow0RTT: true,
	}

	if l.listener, err = quic.ListenAddrEarly(config.transport.Host+":"+config.transport.Port, config.TLS, quicConfig); err != nil {
		return nil, err
	}

	connChannel := make(chan quic.EarlyConnection)

	go func() {
		for {
			conn, err := l.listener.Accept(context.Background())
			if err != nil {
				fmt.Println("Error accepting: ", err)
				return
			}
			connChannel <- conn
		}
	}()
	defer l.listener.Close()

	for conn := range connChannel {
		fmt.Println("Accepted QUIC connection")
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			panic(err)
		}
		l.handleQuicConnection(stream)
	}

	return l, nil
}

func (l *quic_udp) Ready() error {
	return l.baseReady()
}

func (l *quic_udp) Alive() error {
	return l.baseReady()
}

func (l *quic_udp) Close() error {
	var err error

	l.onceStop.Do(func() {
		close(l.quit)
		err = l.listener.Close()
		l.listener = nil
		l.log = nil
	})

	return err
}

func (l *quic_udp) Serve() error {
	accept := make(chan error, 1)
	defer close(accept)

	for {
		err := l.AcceptPool.ScheduleTimeout(time.Millisecond, func() {
			var er error

			defer func() {
				accept <- er
			}()

			select {
			case <-l.quit:
				er = types.ErrClosed
				return
			default:
			}
		})

		if err != nil && err != types.ErrScheduleTimeout {
			break
		} else if err == types.ErrScheduleTimeout {
			continue
		}

		err = <-accept

		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				delay := 5 * time.Millisecond
				time.Sleep(delay)
			} else {
				break
			}
		}
	}
	return nil
}
