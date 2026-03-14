package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
)

// Handler processes a request and returns a response.
// For subscribe requests, it should return nil and handle the connection directly.
type Handler func(conn net.Conn, req Msg) Msg

// Server is the Unix socket API server.
type Server struct {
	socketPath string
	listener   net.Listener
	handler    Handler
	wg         sync.WaitGroup
	done       chan struct{}
	conns      map[net.Conn]struct{}
	connsMu    sync.Mutex
}

func NewServer(socketPath string, handler Handler) *Server {
	return &Server{
		socketPath: socketPath,
		handler:    handler,
		done:       make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
	}
}

// Start begins listening on the Unix socket.
func (s *Server) Start() error {
	// Remove stale socket if it exists
	os.Remove(s.socketPath)

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.socketPath, err)
	}

	// Owner-only permissions
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

// Stop closes the listener, removes the socket (so clients see it's gone immediately),
// then waits for active connections to drain.
func (s *Server) Stop() {
	close(s.done)
	if s.listener != nil {
		s.listener.Close()
	}
	os.Remove(s.socketPath)

	// Close all tracked connections so handleConn goroutines unblock
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
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	s.connsMu.Lock()
	s.conns[conn] = struct{}{}
	s.connsMu.Unlock()

	owned := false // set when handler takes ownership (e.g., subscribe)
	defer func() {
		if !owned {
			s.connsMu.Lock()
			delete(s.conns, conn)
			s.connsMu.Unlock()
			conn.Close()
		}
	}()

	scanner := bufio.NewScanner(conn)
	// Allow large messages (16MB)
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
			// Handler took ownership of connection (e.g., subscribe).
			// Don't close or untrack — the handler manages it now.
			owned = true
			return
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
