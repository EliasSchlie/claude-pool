// Package paths resolves all file paths within a pool directory.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Pool holds resolved paths for a pool directory.
type Pool struct {
	Root string // Pool root directory (--pool-dir)
}

func New(root string) *Pool {
	return &Pool{Root: root}
}

func (p *Pool) ConfigJSON() string     { return filepath.Join(p.Root, "config.json") }
func (p *Pool) PoolJSON() string       { return filepath.Join(p.Root, "pool.json") }
func (p *Pool) Socket() string         { return filepath.Join(p.Root, "api.sock") }
func (p *Pool) DaemonPID() string      { return filepath.Join(p.Root, "daemon.pid") }
func (p *Pool) LogDir() string         { return filepath.Join(p.Root, "logs") }
func (p *Pool) DaemonLog() string      { return filepath.Join(p.Root, "logs", "daemon.log") }
func (p *Pool) ErrorLog() string       { return filepath.Join(p.Root, "logs", "error.log") }
func (p *Pool) OffloadedDir() string   { return filepath.Join(p.Root, "offloaded") }
func (p *Pool) ArchivedDir() string    { return filepath.Join(p.Root, "archived") }
func (p *Pool) SessionPIDsDir() string { return filepath.Join(p.Root, "session-pids") }
func (p *Pool) IdleSignalsDir() string { return filepath.Join(p.Root, "idle-signals") }
func (p *Pool) HooksDir() string       { return filepath.Join(p.Root, "hooks") }

// SessionOffloaded returns the directory for an offloaded session's metadata.
func (p *Pool) SessionOffloaded(id string) string {
	return filepath.Join(p.OffloadedDir(), id)
}

// SessionArchived returns the directory for an archived session's metadata.
func (p *Pool) SessionArchived(id string) string {
	return filepath.Join(p.ArchivedDir(), id)
}

// IdleSignal returns the path to a session's idle signal file, keyed by PID.
func (p *Pool) IdleSignal(pid int) string {
	return filepath.Join(p.IdleSignalsDir(), fmt.Sprintf("%d", pid))
}

// SessionPID returns the path for a PID mapping file, keyed by PID.
func (p *Pool) SessionPID(pid int) string {
	return filepath.Join(p.SessionPIDsDir(), fmt.Sprintf("%d", pid))
}

// EnsureDirs creates all required subdirectories.
func (p *Pool) EnsureDirs() error {
	dirs := []string{
		p.LogDir(),
		p.OffloadedDir(),
		p.ArchivedDir(),
		p.SessionPIDsDir(),
		p.IdleSignalsDir(),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	return nil
}
