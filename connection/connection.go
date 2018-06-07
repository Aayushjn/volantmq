// Copyright (c) 2014 The VolantMQ Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connection

import (
	"container/list"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VolantMQ/vlapi/mqttp"
	"github.com/VolantMQ/vlapi/plugin/persistence"
	"github.com/VolantMQ/volantmq/configuration"
	"github.com/VolantMQ/volantmq/systree"
	"github.com/VolantMQ/volantmq/transport"
	"github.com/VolantMQ/volantmq/types"
	"go.uber.org/zap"
)

type state int

const (
	stateConnecting state = iota
	stateAuth
	stateConnected
	stateReAuth
	stateDisconnected
	stateConnectFailed
)

// nolint: golint
var (
	ErrOverflow    = errors.New("session: overflow")
	ErrPersistence = errors.New("session: error during persistence restore")
)

var expectedPacketType = map[state]map[mqttp.Type]bool{
	stateConnecting: {mqttp.CONNECT: true},
	stateAuth: {
		mqttp.AUTH:       true,
		mqttp.DISCONNECT: true,
	},
	stateConnected: {
		mqttp.PUBLISH:     true,
		mqttp.PUBACK:      true,
		mqttp.PUBREC:      true,
		mqttp.PUBREL:      true,
		mqttp.PUBCOMP:     true,
		mqttp.SUBSCRIBE:   true,
		mqttp.SUBACK:      true,
		mqttp.UNSUBSCRIBE: true,
		mqttp.UNSUBACK:    true,
		mqttp.PINGREQ:     true,
		mqttp.AUTH:        true,
		mqttp.DISCONNECT:  true,
	},
	stateReAuth: {
		mqttp.PUBLISH:     true,
		mqttp.PUBACK:      true,
		mqttp.PUBREC:      true,
		mqttp.PUBREL:      true,
		mqttp.PUBCOMP:     true,
		mqttp.SUBSCRIBE:   true,
		mqttp.SUBACK:      true,
		mqttp.UNSUBSCRIBE: true,
		mqttp.UNSUBACK:    true,
		mqttp.PINGREQ:     true,
		mqttp.AUTH:        true,
		mqttp.DISCONNECT:  true,
	},
}

func (s state) desc() string {
	switch s {
	case stateConnecting:
		return "CONNECTING"
	case stateAuth:
		return "AUTH"
	case stateConnected:
		return "CONNECTED"
	case stateReAuth:
		return "RE-AUTH"
	case stateDisconnected:
		return "DISCONNECTED"
	default:
		return "CONNECT_FAILED"
	}
}

// DisconnectParams session state when stopped
type DisconnectParams struct {
	Reason  mqttp.ReasonCode
	Packets persistence.PersistedPackets
}

//type onDisconnect func(*DisconnectParams)

// Callbacks provided by sessions manager to signal session state
type Callbacks struct {
	// OnStop called when session stopped net connection and should be either suspended or deleted
	OnStop func(string, bool)
}

// WillConfig configures session for will messages
type WillConfig struct {
	Topic   string
	Message []byte
	Retain  bool
	QoS     mqttp.QosType
}

// AuthParams ...
type AuthParams struct {
	AuthMethod string
	AuthData   []byte
	Reason     mqttp.ReasonCode
}

// ConnectParams ...
type ConnectParams struct {
	AuthParams
	ID              string
	Error           error
	ExpireIn        *uint32
	Will            *mqttp.Publish
	Username        []byte
	Password        []byte
	WillDelay       uint32
	MaxTxPacketSize uint32
	SendQuota       uint16
	KeepAlive       uint16
	IDGen           bool
	CleanStart      bool
	Durable         bool
	Version         mqttp.ProtocolVersion
}

// SessionCallbacks ...
type SessionCallbacks interface {
	SignalPublish(*mqttp.Publish) error
	SignalSubscribe(*mqttp.Subscribe) (mqttp.Provider, error)
	SignalUnSubscribe(*mqttp.UnSubscribe) (mqttp.Provider, error)
	SignalDisconnect(*mqttp.Disconnect) (mqttp.Provider, error)
	SignalOffline()
	SignalConnectionClose(DisconnectParams)
}

type ackQueues struct {
	pubIn  ackQueue
	pubOut ackQueue
}

type flow struct {
	flowInUse   sync.Map
	flowCounter uint32
}

type tx struct {
	// txGMessages contains any messages except PUBLISH QoS 1/2
	txGMessages list.List
	// txQMessages contains PUBLISH QoS 1/2 messages. Separate queue is required to keep other messages sending
	//             if quota reached
	txQMessages     list.List
	txGLock         sync.Mutex
	txQLock         sync.Mutex
	txLock          sync.Mutex
	txTopicAlias    map[string]uint16
	txWg            sync.WaitGroup
	txTimer         *time.Timer
	txRunning       uint32
	txAvailable     chan int
	txQuotaExceeded bool
}
type rx struct {
	rxWg         sync.WaitGroup
	connWg       sync.WaitGroup
	rxTopicAlias map[uint16]string
	rxRecv       []byte
	keepAlive    time.Duration
	rxRemaining  int
}

// impl of the connection
type impl struct {
	SessionCallbacks
	id         string
	metric     systree.PacketsMetric
	conn       transport.Conn
	signalAuth OnAuthCb
	ackQueues
	tx
	rx
	flow
	quit              chan struct{}
	connect           chan interface{}
	onStart           types.Once
	onConnDisconnect  types.OnceWait
	started           sync.WaitGroup
	log               *zap.SugaredLogger
	authMethod        string
	connectProcessed  uint32
	maxRxPacketSize   uint32
	maxTxPacketSize   uint32
	txQuota           int32
	rxQuota           int32
	state             state
	topicAliasCurrMax uint16
	maxTxTopicAlias   uint16
	maxRxTopicAlias   uint16
	version           mqttp.ProtocolVersion
	retainAvailable   bool
	offlineQoS0       bool
}

type unacknowledged struct {
	packet mqttp.Provider
}

// Size ...
func (u *unacknowledged) Size() (int, error) {
	return u.packet.Size()
}

type sizeAble interface {
	Size() (int, error)
}

type baseAPI interface {
	Stop(error) bool
}

// Initial ...
type Initial interface {
	baseAPI
	Accept() (chan interface{}, error)
	Send(mqttp.Provider) error
	Acknowledge(p *mqttp.ConnAck, opts ...Option) bool
	Session() Session
}

// Session ...
type Session interface {
	baseAPI
	persistence.PacketLoader
	Publish(string, *mqttp.Publish)
	LoadRemaining(g, q *list.List)
	SetOptions(opts ...Option) error
	Start() error
}

var _ Initial = (*impl)(nil)
var _ Session = (*impl)(nil)

// New allocate new connection object
func New(opts ...Option) Initial {
	s := &impl{
		state: stateConnecting,
		quit:  make(chan struct{}),
	}

	for _, opt := range opts {
		opt(s)
	}

	s.txAvailable = make(chan int, 1)
	s.txTopicAlias = make(map[string]uint16)
	s.rxTopicAlias = make(map[uint16]string)
	s.txTimer = time.NewTimer(1 * time.Second)
	s.txTimer.Stop()

	s.started.Add(1)
	s.pubIn.onRelease = s.onReleaseIn
	s.pubOut.onRelease = s.onReleaseOut

	s.log = configuration.GetLogger().Named("connection")

	return s
}

// Accept start handling incoming connection
func (s *impl) Accept() (chan interface{}, error) {
	var err error

	defer func() {
		if err != nil {
			close(s.connect)
			s.conn.Close()
		}
	}()
	s.connect = make(chan interface{})

	s.rxConnection()

	return s.connect, nil
}

//func (s *impl) Accept() (chan interface{}, error) {
//	var err error
//
//	defer func() {
//		if err != nil {
//			close(s.connect)
//			s.conn.Close()
//		}
//	}()
//	s.connect = make(chan interface{})
//
//	if s.keepAlive > 0 {
//		if err = s.conn.SetReadDeadline(time.Now().Add(s.keepAlive)); err != nil {
//			return nil, err
//		}
//	}
//
//	if err = s.conn.Start(s.rxConnection); err != nil {
//		return nil, err
//	}
//
//	return s.connect, nil
//}

// Session object
func (s *impl) Session() Session {
	return s
}

func (s *impl) Start() error {
	defer s.txLock.Unlock()
	s.txLock.Lock()

	s.txRun()

	return nil
}

// Send packet to connection
func (s *impl) Send(pkt mqttp.Provider) (err error) {
	defer func() {
		if err != nil {
			close(s.connect)
			s.conn.Close()
		}
	}()

	if pkt.Type() == mqttp.AUTH {
		s.state = stateAuth
	}

	s.gPush(pkt)

	s.rxConnection()

	//if s.keepAlive.Nanoseconds() > 0 {
	//	if err = s.conn.SetReadDeadline(time.Now().Add(s.keepAlive)); err != nil {
	//		s.connect <- err
	//		return
	//	}
	//}
	//
	//return s.conn.Resume()
	return nil
}

// Acknowledge incoming connection
func (s *impl) Acknowledge(p *mqttp.ConnAck, opts ...Option) bool {
	ack := true
	s.conn.SetReadDeadline(time.Time{})
	//s.conn.Stop()
	s.connWg.Wait()

	close(s.connect)

	if p.ReturnCode() == mqttp.CodeSuccess {
		s.state = stateConnected

		for _, opt := range opts {
			opt(s)
		}

		//if s.keepAlive > 0 {
		//	s.conn.SetReadDeadline(time.Now().Add(s.keepAlive))
		//}
		//if err := s.conn.Start(s.rxRun); err != nil {
		//	s.log.Error("Cannot start receiver", zap.String("ClientID", s.id), zap.Error(err))
		//	s.state = stateConnectFailed
		//	ack = false
		//}
	} else {
		s.state = stateConnectFailed
		ack = false
	}

	buf, _ := mqttp.Encode(p)
	bufs := net.Buffers{buf}
	bufs.WriteTo(s.conn)

	if !ack {
		s.Stop(nil)
	} else {
		s.rxRun()
	}

	return ack
}

// Stop connection. Function assumed to be invoked once server about to either shutdown, disconnect
// or session is being replaced
// Effective only first invoke
func (s *impl) Stop(reason error) bool {
	return s.onConnectionClose(reason)
}

// LoadRemaining ...
func (s *impl) LoadRemaining(g, q *list.List) {
	select {
	case <-s.quit:
		return
	default:
	}
	s.gLoadList(g)
	s.qLoadList(q)
}

// LoadPersistedPacket ...
func (s *impl) LoadPersistedPacket(entry *persistence.PersistedPacket) error {
	var err error
	var pkt mqttp.Provider

	if pkt, _, err = mqttp.Decode(s.version, entry.Data); err != nil {
		s.log.Error("Couldn't decode persisted message", zap.Error(err))
		return ErrPersistence
	}

	if entry.Flags.UnAck {
		switch p := pkt.(type) {
		case *mqttp.Publish:
			id, _ := p.ID()
			s.flowReAcquire(id)
		case *mqttp.Ack:
			id, _ := p.ID()
			s.flowReAcquire(id)
		}

		s.qLoad(&unacknowledged{packet: pkt})
	} else {
		if p, ok := pkt.(*mqttp.Publish); ok {
			if len(entry.ExpireAt) > 0 {
				var tm time.Time
				if tm, err = time.Parse(time.RFC3339, entry.ExpireAt); err == nil {
					p.SetExpireAt(tm)
				} else {
					s.log.Error("Parse publish expiry", zap.String("ClientID", s.id), zap.Error(err))
				}
			}

			if p.QoS() == mqttp.QoS0 {
				s.gLoad(pkt)
			} else {
				s.qLoad(pkt)
			}
		}
	}

	return nil
}

// Publish ...
func (s *impl) Publish(id string, pkt *mqttp.Publish) {
	if pkt.QoS() == mqttp.QoS0 {
		s.gPush(pkt)
	} else {
		s.qPush(pkt)
	}
}

func genClientID() string {
	b := make([]byte, 15)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return ""
	}

	return base64.URLEncoding.EncodeToString(b)
}

func (s *impl) getWill(pkt *mqttp.Connect) *mqttp.Publish {
	var p *mqttp.Publish

	if willTopic, willPayload, willQoS, willRetain, will := pkt.Will(); will {
		p = mqttp.NewPublish(pkt.Version())
		if err := p.Set(willTopic, willPayload, willQoS, willRetain, false); err != nil {
			s.log.Error("Configure will packet", zap.String("ClientID", s.id), zap.Error(err))
			p = nil
		}
	}

	return p
}

func (s *impl) onConnect(pkt *mqttp.Connect) (mqttp.Provider, error) {
	if atomic.CompareAndSwapUint32(&s.connectProcessed, 0, 1) {
		id := string(pkt.ClientID())
		idGen := false
		if len(id) == 0 {
			idGen = true
			id = genClientID()
		}

		s.id = id

		params := &ConnectParams{
			ID:         id,
			IDGen:      idGen,
			Will:       s.getWill(pkt),
			KeepAlive:  pkt.KeepAlive(),
			Version:    pkt.Version(),
			CleanStart: pkt.IsClean(),
			Durable:    true,
		}

		params.Username, params.Password = pkt.Credentials()
		s.version = params.Version

		s.readConnProperties(pkt, params)

		// MQTT v5 has different meaning of clean comparing to MQTT v3
		//  - v3: if session is clean it is clean start and session lasts when Network connection closed
		//  - v5: clean only means "clean start" and sessions lasts on connection close on if expire propery
		//          exists and set to 0
		if (params.Version <= mqttp.ProtocolV311 && params.CleanStart) ||
			(params.Version >= mqttp.ProtocolV50 && params.ExpireIn != nil && *params.ExpireIn == 0) {
			params.Durable = false
		}

		s.connect <- params
		return nil, nil
	}

	// It's protocol error to send CONNECT packet more than once
	return nil, mqttp.CodeProtocolError
}

func (s *impl) onAuth(pkt *mqttp.Auth) (mqttp.Provider, error) {
	// AUTH packets are allowed for v5.0 only
	if s.version < mqttp.ProtocolV50 {
		return nil, mqttp.CodeRefusedServerUnavailable
	}

	reason := pkt.ReasonCode()

	// Client must not send AUTH packets before server has requested it
	// during auth or re-auth Client must respond only AUTH with CodeContinueAuthentication
	// if connection is being established Client must send AUTH only with CodeReAuthenticate
	if (s.state == stateConnecting) ||
		((s.state == stateAuth || s.state == stateReAuth) && (reason != mqttp.CodeContinueAuthentication)) ||
		((s.state == stateConnected) && reason != (mqttp.CodeReAuthenticate)) {
		return nil, mqttp.CodeProtocolError
	}

	params := AuthParams{
		Reason: reason,
	}

	// [MQTT-3.15.2.2.2]
	if prop := pkt.PropertyGet(mqttp.PropertyAuthMethod); prop != nil {
		if val, e := prop.AsString(); e == nil {
			params.AuthMethod = val
		}
	}

	// AUTH packet must provide AuthMethod property
	if len(params.AuthMethod) == 0 {
		return nil, mqttp.CodeProtocolError
	}

	// [MQTT-4.12.0-7] - If the Client does not include an Authentication Method in the CONNECT,
	//                   the Client MUST NOT send an AUTH packet to the Server
	// [MQTT-4.12.1-1] - The Client MUST set the Authentication Method to the same value as
	//                   the Authentication Method originally used to authenticate the Network Connection
	if len(s.authMethod) == 0 || s.authMethod != params.AuthMethod {
		return nil, mqttp.CodeProtocolError
	}

	// [MQTT-3.15.2.2.3]
	if prop := pkt.PropertyGet(mqttp.PropertyAuthData); prop != nil {
		if val, e := prop.AsBinary(); e == nil {
			params.AuthData = val
		}
	}

	if s.state == stateConnecting || s.state == stateAuth {
		s.connect <- params
		return nil, nil
	}

	return s.signalAuth(s.id, &params)
}

func (s *impl) readConnProperties(req *mqttp.Connect, params *ConnectParams) {
	if s.version < mqttp.ProtocolV50 {
		return
	}

	// [MQTT-3.1.2.11.2]
	if prop := req.PropertyGet(mqttp.PropertySessionExpiryInterval); prop != nil {
		if val, e := prop.AsInt(); e == nil {
			params.ExpireIn = &val
		}
	}

	// [MQTT-3.1.2.11.3]
	if prop := req.PropertyGet(mqttp.PropertyWillDelayInterval); prop != nil {
		if val, e := prop.AsInt(); e == nil {
			params.WillDelay = val
		}
	}

	// [MQTT-3.1.2.11.4]
	if prop := req.PropertyGet(mqttp.PropertyReceiveMaximum); prop != nil {
		if val, e := prop.AsShort(); e == nil {
			//s.pubOut.quota = int32(val)
			s.txQuota = int32(val)
			params.SendQuota = val
		}
	}

	// [MQTT-3.1.2.11.5]
	if prop := req.PropertyGet(mqttp.PropertyMaximumPacketSize); prop != nil {
		if val, e := prop.AsInt(); e == nil {
			s.maxTxPacketSize = val
		}
	}

	// [MQTT-3.1.2.11.6]
	if prop := req.PropertyGet(mqttp.PropertyTopicAliasMaximum); prop != nil {
		if val, e := prop.AsShort(); e == nil {
			s.maxTxTopicAlias = val
		}
	}

	// [MQTT-3.1.2.11.10]
	if prop := req.PropertyGet(mqttp.PropertyAuthMethod); prop != nil {
		if val, e := prop.AsString(); e == nil {
			params.AuthMethod = val
			s.authMethod = val
		}
	}

	// [MQTT-3.1.2.11.11]
	if prop := req.PropertyGet(mqttp.PropertyAuthData); prop != nil {
		if len(params.AuthMethod) == 0 {
			params.Error = mqttp.CodeProtocolError
			return
		}
		if val, e := prop.AsBinary(); e == nil {
			params.AuthData = val
		}
	}

	return
}

func (s *impl) processIncoming(p mqttp.Provider) error {
	var err error
	var resp mqttp.Provider

	// [MQTT-3.1.2-33] - If a Client sets an Authentication Method in the CONNECT,
	//                   the Client MUST NOT send any packets other than AUTH or DISCONNECT packets
	//                   until it has received a CONNACK packet
	if _, ok := expectedPacketType[s.state][p.Type()]; !ok {
		s.log.Debug("Unexpected packet for current state",
			zap.String("ClientID", s.id),
			zap.String("state", s.state.desc()),
			zap.String("packet", p.Type().Name()))
		return mqttp.CodeProtocolError
	}

	switch pkt := p.(type) {
	case *mqttp.Connect:
		resp, err = s.onConnect(pkt)
	case *mqttp.Auth:
		resp, err = s.onAuth(pkt)
	case *mqttp.Publish:
		resp, err = s.onPublish(pkt)
	case *mqttp.Ack:
		resp, err = s.onAck(pkt)
	case *mqttp.Subscribe:
		resp, err = s.SignalSubscribe(pkt)
	case *mqttp.UnSubscribe:
		resp, err = s.SignalUnSubscribe(pkt)
	case *mqttp.PingReq:
		// For PINGREQ message, we should send back PINGRESP
		resp = mqttp.NewPingResp(s.version)
	case *mqttp.Disconnect:
		resp, err = s.SignalDisconnect(pkt)
	}

	if resp != nil {
		s.gPush(resp)
	}

	return err
}

func (s *impl) getQueuedPackets() persistence.PersistedPackets {
	var packets persistence.PersistedPackets

	packetEncode := func(p interface{}) {
		var pkt mqttp.Provider
		pPkt := &persistence.PersistedPacket{}

		pPkt.Flags.UnAck = false

		switch tp := p.(type) {
		case *mqttp.Publish:
			if expireAt, _, expired := tp.Expired(); expired && (s.offlineQoS0 || tp.QoS() != mqttp.QoS0) {
				if !expireAt.IsZero() {
					pPkt.ExpireAt = expireAt.Format(time.RFC3339)
				}

				if tp.QoS() != mqttp.QoS0 {
					// make sure message has some IDType to prevent encode error
					tp.SetPacketID(0)
				}

				pkt = tp
			}
		case *unacknowledged:
			if pb, ok := tp.packet.(*mqttp.Publish); ok && pb.QoS() == mqttp.QoS1 {
				pb.SetDup(true)
			}

			pkt = tp.packet
			pPkt.Flags.UnAck = true
		}

		if pkt != nil {
			var err error
			if pPkt.Data, err = mqttp.Encode(pkt); err != nil {
				s.log.Error("Couldn't encode message for persistence", zap.Error(err))
			} else {
				packets = append(packets, pPkt)
			}
		}
	}

	var next *list.Element
	for elem := s.txQMessages.Front(); elem != nil; elem = next {
		next = elem.Next()
		packetEncode(s.txQMessages.Remove(elem))
	}

	for elem := s.txGMessages.Front(); elem != nil; elem = next {
		next = elem.Next()
		switch tp := s.txGMessages.Remove(elem).(type) {
		case *mqttp.Publish:
			packetEncode(tp)
		}
	}

	s.pubOut.messages.Range(func(k, v interface{}) bool {
		if pkt, ok := v.(mqttp.Provider); ok {
			packetEncode(&unacknowledged{packet: pkt})
		}

		s.pubOut.messages.Delete(k)
		return true
	})

	s.pubIn.messages.Range(func(k, v interface{}) bool {
		s.pubIn.messages.Delete(k)
		return true
	})

	return packets
}

// forward PUBLISH message to topics manager which takes care about subscribers
func (s *impl) publishToTopic(p *mqttp.Publish) error {
	// v5.0
	// If the Server included Retain Available in its CONNACK response to a Client with its value set to 0 and it
	// receives a PUBLISH packet with the RETAIN flag is set to 1, then it uses the DISCONNECT Reason
	// Code of 0x9A (Retain not supported) as described in section 4.13.
	if s.version >= mqttp.ProtocolV50 {
		// [MQTT-3.3.2.3.4]
		if prop := p.PropertyGet(mqttp.PropertyTopicAlias); prop != nil {
			if val, err := prop.AsShort(); err == nil {
				if len(p.Topic()) != 0 {
					// renew alias with new topic
					s.rxTopicAlias[val] = p.Topic()
				} else {
					if topic, kk := s.rxTopicAlias[val]; kk {
						// do not check for error as topic has been validated when arrived
						if err = p.SetTopic(topic); err != nil {
							s.log.Error("publish to topic",
								zap.String("ClientID", s.id),
								zap.String("topic", topic),
								zap.Error(err))
						}
					} else {
						return mqttp.CodeInvalidTopicAlias
					}
				}
			} else {
				return mqttp.CodeInvalidTopicAlias
			}
		}

		// [MQTT-3.3.2.3.3]
		if prop := p.PropertyGet(mqttp.PropertyPublicationExpiry); prop != nil {
			if val, err := prop.AsInt(); err == nil {
				s.log.Warn("Set pub expiration", zap.String("ClientID", s.id), zap.Duration("val", time.Duration(val)*time.Second))
				p.SetExpireAt(time.Now().Add(time.Duration(val) * time.Second))
			} else {
				return err
			}
		}
	}

	return s.SignalPublish(p)
}

// onReleaseIn ack process for incoming messages
func (s *impl) onReleaseIn(o, n mqttp.Provider) {
	switch p := o.(type) {
	case *mqttp.Publish:
		s.SignalPublish(p)
	}
}

// onReleaseOut process messages that required ack cycle
// onAckTimeout if publish message has not been acknowledged withing specified ackTimeout
// server should mark it as a dup and send again
func (s *impl) onReleaseOut(o, n mqttp.Provider) {
	switch n.Type() {
	case mqttp.PUBACK:
		fallthrough
	case mqttp.PUBCOMP:
		id, _ := n.ID()
		if s.flowRelease(id) {
			s.signalQuota()
		}
	}
}
