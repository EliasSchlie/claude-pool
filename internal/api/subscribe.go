package api

import (
	"encoding/json"
	"log"
	"net"
	"sync"
	"time"
)

// Subscriber receives filtered events on a persistent connection.
//
// New subscribers start in "pending" mode: events are buffered instead of sent
// immediately. This handles two cases:
//   - Race window: a command on another connection may be processed before the
//     subscribe message, so its event needs to be captured and delivered later.
//   - Re-subscribe: if the client sends a second subscribe (with different
//     filters) before the pending buffer is flushed, the buffer is cleared so
//     events matched by the old filters are discarded.
//
// The subscriber transitions to "committed" mode (direct delivery) either when
// Commit() is called explicitly or after a short timeout.
type Subscriber struct {
	conn        net.Conn
	connectedAt time.Time // connection accept time — replay boundary

	mu        sync.Mutex
	sessions  map[string]bool // nil = all sessions
	events    map[string]bool // nil = all events
	statuses  map[string]bool // nil = all statuses; restricts to "status" events only
	fields    map[string]bool // nil = all fields; restricts to "updated" events only
	committed bool
	pending   []Msg
}

// NewSubscriber creates a subscriber from subscribe request options.
// The subscriber starts in pending mode (events are buffered).
// connectedAt is the time the connection was accepted — only events after
// this time are replayed from the ring buffer.
func NewSubscriber(conn net.Conn, opts Msg, connectedAt time.Time) *Subscriber {
	s := &Subscriber{conn: conn, connectedAt: connectedAt}
	s.applyFilters(opts)
	return s
}

// UpdateFilters replaces the subscriber's filters, clears any buffered events
// (which matched the old filters), and commits (switches to direct delivery).
func (s *Subscriber) UpdateFilters(opts Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.applyFilters(opts)
	s.pending = nil
	s.committed = true
}

// Commit flushes buffered events (re-checking against current filters) and
// switches to direct delivery. Safe to call multiple times.
func (s *Subscriber) Commit() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.committed {
		return
	}
	s.committed = true

	for _, event := range s.pending {
		if s.matchesLocked(event) {
			s.directSend(event)
		}
	}
	s.pending = nil
}

// Matches returns true if the event passes all filters (ANDed).
func (s *Subscriber) Matches(event Msg) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.matchesLocked(event)
}

// Send delivers an event. In pending mode, events are buffered.
// In committed mode, events are sent directly to the connection.
func (s *Subscriber) Send(event Msg) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.committed {
		s.pending = append(s.pending, event)
		return nil
	}
	return s.directSend(event)
}

// Conn returns the underlying connection.
func (s *Subscriber) Conn() net.Conn {
	return s.conn
}

func (s *Subscriber) applyFilters(opts Msg) {
	s.sessions = toStringSet(opts["sessions"])
	s.events = toStringSet(opts["events"])
	s.statuses = toStringSet(opts["statuses"])
	s.fields = toStringSet(opts["fields"])
}

func (s *Subscriber) matchesLocked(event Msg) bool {
	if s.sessions != nil {
		sid, _ := event["sessionId"].(string)
		if !s.sessions[sid] {
			return false
		}
	}

	eventType, _ := event["event"].(string)
	if s.events != nil {
		if !s.events[eventType] {
			return false
		}
	}

	if s.statuses != nil {
		if eventType != "status" {
			return false
		}
		status, _ := event["status"].(string)
		if !s.statuses[status] {
			return false
		}
	}

	if s.fields != nil {
		if eventType != "updated" {
			return false
		}
		changes, ok := event["changes"].(Msg)
		if !ok {
			return false
		}
		match := false
		for k := range changes {
			if s.fields[k] {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}

	return true
}

func (s *Subscriber) directSend(event Msg) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = s.conn.Write(data)
	return err
}

// recentEvent pairs an event with its broadcast timestamp.
type recentEvent struct {
	event Msg
	at    time.Time
}

// SubscriberHub manages active subscribers, broadcasts events, and maintains
// a short ring buffer so that events from the race window between connection
// accept and subscribe processing can be replayed.
type SubscriberHub struct {
	mu   sync.Mutex
	subs map[*Subscriber]struct{}

	ring    [replayBufferSize]recentEvent
	ringPos int
	ringLen int
}

const replayBufferSize = 64

// NewSubscriberHub creates a new hub.
func NewSubscriberHub() *SubscriberHub {
	return &SubscriberHub{
		subs: make(map[*Subscriber]struct{}),
	}
}

// Add registers a subscriber and replays recent events from the ring buffer.
// Replayed events go to the subscriber's pending buffer (not sent directly).
// Call Commit() after a short delay to flush; if a re-subscribe arrives first,
// UpdateFilters() clears the buffer so stale-filter events are discarded.
func (h *SubscriberHub) Add(s *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.subs[s] = struct{}{}

	// Replay events from the race window (after connection accept) into
	// the subscriber's pending buffer. The pending buffer is flushed on
	// Commit() or cleared on re-subscribe (UpdateFilters).
	start := (h.ringPos - h.ringLen + replayBufferSize) % replayBufferSize
	for i := 0; i < h.ringLen; i++ {
		re := h.ring[(start+i)%replayBufferSize]
		if !re.at.After(s.connectedAt) {
			continue // event predates this connection
		}
		if s.Matches(re.event) {
			s.Send(re.event) // goes to pending buffer (subscriber is uncommitted)
		}
	}
}

// Remove unregisters a subscriber.
func (h *SubscriberHub) Remove(s *Subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// FindByConn returns the subscriber for a given connection, or nil.
func (h *SubscriberHub) FindByConn(conn net.Conn) *Subscriber {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.Conn() == conn {
			return s
		}
	}
	return nil
}

// RemoveByConn removes the subscriber for a given connection, if any.
func (h *SubscriberHub) RemoveByConn(conn net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.Conn() == conn {
			delete(h.subs, s)
			return
		}
	}
}

// Broadcast sends an event to all matching subscribers and stores it in
// the ring buffer for replay to future subscribers.
func (h *SubscriberHub) Broadcast(event Msg) {
	h.mu.Lock()
	h.ring[h.ringPos] = recentEvent{event: event, at: time.Now()}
	h.ringPos = (h.ringPos + 1) % replayBufferSize
	if h.ringLen < replayBufferSize {
		h.ringLen++
	}
	subs := make([]*Subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	for _, s := range subs {
		if s.Matches(event) {
			if err := s.Send(event); err != nil {
				log.Printf("subscriber send error: %v", err)
				h.Remove(s)
			}
		}
	}
}

// CommitAfter schedules a subscriber commit after a short delay.
// This gives the connection time to process a potential re-subscribe
// (which clears the buffer and changes filters) before flushing.
func CommitAfter(s *Subscriber, d time.Duration) {
	go func() {
		time.Sleep(d)
		s.Commit()
	}()
}

func toStringSet(v any) map[string]bool {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	set := make(map[string]bool, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			set[s] = true
		}
	}
	return set
}
