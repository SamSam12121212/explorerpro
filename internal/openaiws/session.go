package openaiws

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrNoDialer         = errors.New("openai websocket dialer is not configured")
	ErrAlreadyConnected = errors.New("openai websocket session is already connected")
	ErrNotConnected     = errors.New("openai websocket session is not connected")
)

type SessionState string

const (
	SessionStateDisconnected SessionState = "disconnected"
	SessionStateConnecting   SessionState = "connecting"
	SessionStateConnected    SessionState = "connected"
	SessionStateClosed       SessionState = "closed"
)

type Snapshot struct {
	State            SessionState `json:"state"`
	SocketGeneration uint64       `json:"socket_generation"`
	ConnectedAt      time.Time    `json:"connected_at,omitempty"`
	LastReadAt       time.Time    `json:"last_read_at,omitempty"`
	LastWriteAt      time.Time    `json:"last_write_at,omitempty"`
}

type Session struct {
	cfg    Config
	dialer Dialer

	mu               sync.RWMutex
	conn             Conn
	state            SessionState
	socketGeneration uint64
	connectedAt      time.Time
	lastReadAt       time.Time
	lastWriteAt      time.Time
	readCancel       context.CancelFunc
	readDone         chan struct{}
	readEvents       chan ServerEvent
	readErr          error
}

func NewSession(cfg Config, dialer Dialer) *Session {
	return &Session{
		cfg:    cfg,
		dialer: dialer,
		state:  SessionStateDisconnected,
	}
}

func (s *Session) Connect(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return err
	}

	if s.dialer == nil {
		return ErrNoDialer
	}

	s.mu.Lock()
	if s.conn != nil {
		s.mu.Unlock()
		return ErrAlreadyConnected
	}
	s.state = SessionStateConnecting
	s.mu.Unlock()

	dialCtx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	defer cancel()

	conn, err := s.dialer.Dial(dialCtx, DialRequest{
		URL:    s.cfg.ResponsesSocketURL,
		Header: s.cfg.HandshakeHeaders(),
	})
	if err != nil {
		s.mu.Lock()
		s.state = SessionStateDisconnected
		s.mu.Unlock()
		return fmt.Errorf("dial responses websocket: %w", err)
	}

	now := time.Now().UTC()
	readCtx, readCancel := context.WithCancel(context.Background())
	readEvents := make(chan ServerEvent, 256)
	readDone := make(chan struct{})

	s.mu.Lock()
	s.conn = conn
	s.state = SessionStateConnected
	s.socketGeneration++
	s.connectedAt = now
	s.lastReadAt = time.Time{}
	s.lastWriteAt = time.Time{}
	s.readCancel = readCancel
	s.readDone = readDone
	s.readEvents = readEvents
	s.readErr = nil
	s.mu.Unlock()

	go s.runReadLoop(readCtx, conn, readEvents, readDone)

	return nil
}

func (s *Session) Send(ctx context.Context, event ClientEvent) error {
	payload, err := event.Bytes()
	if err != nil {
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
	defer cancel()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn == nil {
		return ErrNotConnected
	}

	if err := s.conn.Write(writeCtx, payload); err != nil {
		if shouldInvalidateSession(err) {
			_ = s.invalidateConnLocked("write failed")
		}
		return fmt.Errorf("write websocket event: %w", err)
	}

	s.lastWriteAt = time.Now().UTC()
	return nil
}

func (s *Session) Receive(ctx context.Context) (ServerEvent, error) {
	s.mu.RLock()
	readEvents := s.readEvents
	s.mu.RUnlock()

	if readEvents == nil {
		return ServerEvent{}, ErrNotConnected
	}

	select {
	case <-ctx.Done():
		return ServerEvent{}, ctx.Err()
	case event, ok := <-readEvents:
		if ok {
			return event, nil
		}
		return ServerEvent{}, s.readLoopError()
	}
}

func (s *Session) Ping(ctx context.Context) error {
	s.mu.RLock()
	conn := s.conn
	s.mu.RUnlock()

	if conn == nil {
		return ErrNotConnected
	}

	pingCtx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
	defer cancel()

	if err := conn.Ping(pingCtx); err != nil {
		if shouldInvalidateSession(err) {
			s.invalidateConn(conn, "ping failed")
		}
		return fmt.Errorf("ping websocket session: %w", err)
	}

	return nil
}

func (s *Session) Close() error {
	s.mu.Lock()
	conn := s.conn
	readCancel := s.readCancel
	readDone := s.readDone
	s.conn = nil
	s.state = SessionStateClosed
	s.readCancel = nil
	s.readDone = nil
	s.readEvents = nil
	s.readErr = nil
	s.mu.Unlock()

	if readCancel != nil {
		readCancel()
	}

	if conn == nil {
		return nil
	}

	if err := conn.Close(CloseCodeNormal, "normal closure"); err != nil {
		return fmt.Errorf("close websocket session: %w", err)
	}

	if readDone != nil {
		<-readDone
	}

	return nil
}

func (s *Session) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return Snapshot{
		State:            s.state,
		SocketGeneration: s.socketGeneration,
		ConnectedAt:      s.connectedAt,
		LastReadAt:       s.lastReadAt,
		LastWriteAt:      s.lastWriteAt,
	}
}

func shouldInvalidateSession(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (s *Session) invalidateConn(conn Conn, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != conn {
		return
	}
	_ = s.invalidateConnLocked(reason)
}

func (s *Session) invalidateConnLocked(reason string) error {
	conn := s.conn
	readCancel := s.readCancel
	s.conn = nil
	s.state = SessionStateDisconnected
	s.readCancel = nil
	s.readEvents = nil
	s.readErr = ErrNotConnected
	if readCancel != nil {
		readCancel()
	}
	if conn == nil {
		return nil
	}
	return conn.Close(CloseCodeInternalError, reason)
}

func (s *Session) runReadLoop(ctx context.Context, conn Conn, readEvents chan ServerEvent, readDone chan struct{}) {
	defer close(readDone)
	defer close(readEvents)

	for {
		payload, err := conn.Read(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				s.finishReadLoop(conn, nil, SessionStateClosed)
				return
			}
			s.finishReadLoop(conn, fmt.Errorf("read websocket event: %w", err), SessionStateDisconnected)
			return
		}

		event, err := DecodeServerEvent(payload)
		if err != nil {
			s.finishReadLoop(conn, err, SessionStateDisconnected)
			return
		}

		s.mu.Lock()
		if s.conn == conn {
			s.lastReadAt = time.Now().UTC()
		}
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			s.finishReadLoop(conn, nil, SessionStateClosed)
			return
		case readEvents <- event:
		}
	}
}

func (s *Session) finishReadLoop(conn Conn, err error, nextState SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conn != conn {
		return
	}

	s.conn = nil
	s.readCancel = nil
	s.readDone = nil
	s.readEvents = nil
	s.readErr = err
	if s.state != SessionStateClosed {
		s.state = nextState
	}
}

func (s *Session) readLoopError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.readErr != nil {
		return s.readErr
	}

	if s.state == SessionStateClosed {
		return ErrNotConnected
	}

	return ErrNotConnected
}
