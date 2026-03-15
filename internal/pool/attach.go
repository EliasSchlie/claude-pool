package pool

import (
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	ptyPkg "github.com/EliasSchlie/claude-pool/internal/pty"
)

// attachPipe manages a per-session Unix socket for raw PTY I/O.
// Multiple clients can connect; output is broadcast to all.
// The pipe closes when the session is offloaded, dies, or explicitly closed.
type attachPipe struct {
	sessionID  string
	socketPath string
	listener   net.Listener
	proc       *ptyPkg.Process
	sub        chan []byte
	onInput    func([]byte) // called with raw bytes from client → PTY (for state tracking)

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	done    chan struct{}
	stopped chan struct{} // closed when broadcastLoop exits
}

func newAttachPipe(sessionID, socketDir string, proc *ptyPkg.Process) (*attachPipe, error) {
	socketPath := filepath.Join(socketDir, "attach-"+sessionID+".sock")
	os.Remove(socketPath) // clean stale

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		ln.Close()
		return nil, err
	}

	ap := &attachPipe{
		sessionID:  sessionID,
		socketPath: socketPath,
		listener:   ln,
		proc:       proc,
		sub:        proc.Subscribe(),
		conns:      make(map[net.Conn]struct{}),
		done:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}

	go ap.acceptLoop()
	go ap.broadcastLoop()

	return ap, nil
}

func (ap *attachPipe) acceptLoop() {
	for {
		conn, err := ap.listener.Accept()
		if err != nil {
			select {
			case <-ap.done:
				return
			default:
				log.Printf("[attach] session %s: accept error: %v", ap.sessionID, err)
				return
			}
		}

		// Replay buffered output before joining the broadcast.
		// This gives the client the current screen state (like tmux/screen).
		replay := ap.proc.Buffer()
		if len(replay) > 0 {
			if _, err := conn.Write(replay); err != nil {
				log.Printf("[attach] session %s: replay write error: %v", ap.sessionID, err)
				conn.Close()
				continue
			}
		}

		ap.mu.Lock()
		ap.conns[conn] = struct{}{}
		ap.mu.Unlock()

		log.Printf("[attach] session %s: client connected (replayed %d bytes)", ap.sessionID, len(replay))

		// Read from client → write to PTY
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					if ap.onInput != nil {
						ap.onInput(buf[:n])
					}
					if wErr := ap.proc.Write(buf[:n]); wErr != nil {
						log.Printf("[attach] session %s: pty write error: %v", ap.sessionID, wErr)
						break
					}
				}
				if err != nil {
					log.Printf("[attach] session %s: client read error: %v", ap.sessionID, err)
					break
				}
			}
			ap.removeConn(conn)
		}()
	}
}

// broadcastLoop reads PTY output from the subscriber channel and writes to all clients.
// On shutdown it closes all client connections, ensuring no stale data is
// delivered after the pipe is told to stop.
func (ap *attachPipe) broadcastLoop() {
	defer close(ap.stopped)
	for {
		select {
		case data, ok := <-ap.sub:
			if !ok {
				ap.closeConns()
				return
			}
			// Snapshot connections to avoid holding lock during writes
			ap.mu.Lock()
			conns := make([]net.Conn, 0, len(ap.conns))
			for conn := range ap.conns {
				conns = append(conns, conn)
			}
			ap.mu.Unlock()

			for _, conn := range conns {
				if _, err := conn.Write(data); err != nil {
					log.Printf("[attach] session %s: broadcast write error (%d bytes): %v", ap.sessionID, len(data), err)
					ap.removeConn(conn)
				}
			}
		case <-ap.done:
			ap.closeConns()
			return
		}
	}
}

// closeConns closes all tracked client connections.
func (ap *attachPipe) closeConns() {
	ap.mu.Lock()
	for conn := range ap.conns {
		conn.Close()
		delete(ap.conns, conn)
	}
	ap.mu.Unlock()
}

func (ap *attachPipe) removeConn(conn net.Conn) {
	ap.mu.Lock()
	delete(ap.conns, conn)
	ap.mu.Unlock()
	conn.Close()
	log.Printf("[attach] session %s: client disconnected", ap.sessionID)
}

// Close shuts down the attach pipe: closes listener, disconnects all clients, cleans up socket.
func (ap *attachPipe) Close() {
	select {
	case <-ap.done:
		return // already closed
	default:
	}

	// Set immediate write deadline on all connections to unblock any
	// pending writes in broadcastLoop (prevents deadlock when the
	// client isn't reading — e.g., test is waiting for the offload response).
	ap.mu.Lock()
	for conn := range ap.conns {
		conn.SetWriteDeadline(time.Now())
	}
	ap.mu.Unlock()

	// Unsubscribe closes the sub channel. The broadcastLoop will see
	// the closed channel, close all client connections, and exit.
	ap.proc.Unsubscribe(ap.sub)
	close(ap.done)
	ap.listener.Close()

	// Wait for broadcastLoop to finish closing connections. This ensures
	// the offload response isn't sent until clients have been disconnected.
	<-ap.stopped

	os.Remove(ap.socketPath)
	log.Printf("[attach] session %s: pipe closed", ap.sessionID)
}
