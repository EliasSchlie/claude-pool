package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// Handler processes a request and returns a response.
// Returning nil means "no response to write" (e.g., subscribe mode).
type Handler func(conn net.Conn, req Msg) Msg

// Server is the Unix socket API server.
type Server struct {
	socketPath   string
	listener     net.Listener
	handler      Handler
	onDisconnect func(net.Conn) // called when a connection closes
	wg           sync.WaitGroup
	done         chan struct{}

	connsMu   sync.Mutex
	conns     map[net.Conn]struct{}
	connTimes map[net.Conn]time.Time // when each connection was accepted
}

func NewServer(socketPath string, handler Handler) *Server {
	return &Server{
		socketPath: socketPath,
		handler:    handler,
		done:       make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
		connTimes:  make(map[net.Conn]time.Time),
	}
}

// OnDisconnect registers a callback invoked when a connection closes.
func (s *Server) OnDisconnect(fn func(net.Conn)) {
	s.onDisconnect = fn
}

// ConnAcceptedAt returns when a connection was accepted. Used by subscribers
// to determine the replay boundary — only events after this time are replayed.
func (s *Server) ConnAcceptedAt(conn net.Conn) time.Time {
	s.connsMu.Lock()
	t := s.connTimes[conn]
	s.connsMu.Unlock()
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// Start begins listening on the Unix socket.
func (s *Server) Start() error {
	os.Remove(s.socketPath)

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.socketPath, err)
	}
	if err := os.Chmod(s.socketPath, 0600); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	s.listener = ln

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.acceptLoop()
	}()

	return nil
}

// Stop closes the listener, removes the socket, then waits for connections to drain.
func (s *Server) Stop() {
	close(s.done)
	if s.listener != nil {
		s.listener.Close()
	}
	os.Remove(s.socketPath)

	s.connsMu.Lock()
	for conn := range s.conns {
		conn.Close()
	}
	s.connsMu.Unlock()

	s.wg.Wait()
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				log.Printf("accept error: %v", err)
				continue
			}
		}

		now := time.Now()
		s.connsMu.Lock()
		s.conns[conn] = struct{}{}
		s.connTimes[conn] = now
		s.connsMu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer func() {
		if s.onDisconnect != nil {
			s.onDisconnect(conn)
		}
		s.connsMu.Lock()
		delete(s.conns, conn)
		delete(s.connTimes, conn)
		s.connsMu.Unlock()
		conn.Close()
	}()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		select {
		case <-s.done:
			return
		default:
		}

		var req Msg
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			s.writeMsg(conn, ErrorResponse(nil, "invalid JSON: "+err.Error()))
			continue
		}

		resp := s.handler(conn, req)
		if resp == nil {
			continue
		}
		s.writeMsg(conn, resp)
	}
}

func (s *Server) writeMsg(conn net.Conn, msg Msg) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("marshal error: %v", err)
		return
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		log.Printf("write error: %v", err)
	}
}
