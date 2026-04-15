package eventrelay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
)

type FrameHandler func(Frame)

type Server struct {
	logger  *slog.Logger
	handler FrameHandler
	ln      net.Listener

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func NewServer(logger *slog.Logger, handler FrameHandler) *Server {
	return &Server{
		logger:  logger,
		handler: handler,
		conns:   map[net.Conn]struct{}{},
	}
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("event relay listen: %w", err)
	}
	s.ln = ln

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	s.logger.Info("event relay listening", "addr", ln.Addr().String())

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.logger.Warn("event relay accept error", "error", err)
			continue
		}

		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	s.logger.Info("event relay worker connected", "remote", conn.RemoteAddr().String())
	reader := bufio.NewReaderSize(conn, 256*1024)

	for {
		if ctx.Err() != nil {
			return
		}

		frame, err := ReadFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				s.logger.Info("event relay worker disconnected", "remote", conn.RemoteAddr().String())
				return
			}
			s.logger.Warn("event relay read error", "remote", conn.RemoteAddr().String(), "error", err)
			return
		}

		s.handler(frame)
	}
}
