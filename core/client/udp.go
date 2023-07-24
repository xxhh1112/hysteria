package client

import (
	"errors"
	"io"
	"math/rand"
	"sync"

	"github.com/apernet/hysteria/core/internal/frag"
	"github.com/apernet/hysteria/core/internal/protocol"
	"github.com/quic-go/quic-go"
)

const (
	udpMessageChanSize = 1024
)

type udpIO interface {
	ReceiveMessage() (*protocol.UDPMessage, error)
	SendMessage([]byte, *protocol.UDPMessage) error
}

type udpConn struct {
	ID        uint32
	D         *frag.Defragger
	ReceiveCh chan *protocol.UDPMessage
	SendBuf   []byte
	SendFunc  func([]byte, *protocol.UDPMessage) error
	CloseFunc func()
	Closed    bool
}

func (u *udpConn) Receive() ([]byte, string, error) {
	for {
		msg := <-u.ReceiveCh
		if msg == nil {
			// Closed
			return nil, "", io.EOF
		}
		dfMsg := u.D.Feed(msg)
		if dfMsg == nil {
			// Incomplete message, wait for more
			continue
		}
		return dfMsg.Data, dfMsg.Addr, nil
	}
}

// Send is not thread-safe, as it uses a shared SendBuf.
func (u *udpConn) Send(data []byte, addr string) error {
	// Try no frag first
	msg := &protocol.UDPMessage{
		SessionID: u.ID,
		PacketID:  0,
		FragID:    0,
		FragCount: 1,
		Addr:      addr,
		Data:      data,
	}
	err := u.SendFunc(u.SendBuf, msg)
	var errTooLarge quic.ErrMessageTooLarge
	if errors.As(err, &errTooLarge) {
		// Message too large, try fragmentation
		msg.PacketID = uint16(rand.Intn(0xFFFF)) + 1
		fMsgs := frag.FragUDPMessage(msg, int(errTooLarge))
		for _, fMsg := range fMsgs {
			err := u.SendFunc(u.SendBuf, &fMsg)
			if err != nil {
				return err
			}
		}
		return nil
	} else {
		return err
	}
}

func (u *udpConn) Close() error {
	u.CloseFunc()
	return nil
}

type udpSessionManager struct {
	io udpIO

	mutex  sync.Mutex
	m      map[uint32]*udpConn
	nextID uint32
}

func newUDPSessionManager(io udpIO) *udpSessionManager {
	return &udpSessionManager{
		io:     io,
		m:      make(map[uint32]*udpConn),
		nextID: 1,
	}
}

// Run runs the session manager main loop.
// Exit and returns error when the underlying io returns error (e.g. closed).
func (m *udpSessionManager) Run() error {
	defer m.cleanup()

	for {
		msg, err := m.io.ReceiveMessage()
		if err != nil {
			return err
		}
		m.feed(msg)
	}
}

func (m *udpSessionManager) cleanup() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	for _, conn := range m.m {
		m.close(conn)
	}
}

func (m *udpSessionManager) feed(msg *protocol.UDPMessage) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	conn, ok := m.m[msg.SessionID]
	if !ok {
		// Ignore message from unknown session
		return
	}

	select {
	case conn.ReceiveCh <- msg:
		// OK
	default:
		// Channel full, drop the message
	}
}

// NewUDP creates a new UDP session.
func (m *udpSessionManager) NewUDP() (HyUDPConn, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	id := m.nextID
	m.nextID++

	conn := &udpConn{
		ID:        id,
		D:         &frag.Defragger{},
		ReceiveCh: make(chan *protocol.UDPMessage, udpMessageChanSize),
		SendBuf:   make([]byte, protocol.MaxUDPSize),
		SendFunc:  m.io.SendMessage,
	}
	conn.CloseFunc = func() {
		m.mutex.Lock()
		defer m.mutex.Unlock()
		if !conn.Closed {
			m.close(conn)
		}
	}
	m.m[id] = conn

	return conn, nil
}

func (m *udpSessionManager) close(conn *udpConn) {
	conn.Closed = true
	close(conn.ReceiveCh)
	delete(m.m, conn.ID)
}

func (m *udpSessionManager) Count() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return len(m.m)
}
