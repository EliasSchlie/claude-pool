package api

import (
	"encoding/json"
	"log"
	"net"
	"sync"
)

// Subscriber receives filtered events on a persistent connection.
type Subscriber struct {
	conn     net.Conn
	mu       sync.Mutex
	sessions map[string]bool // nil = all sessions
	events   map[string]bool // nil = all events
	statuses map[string]bool // nil = all statuses (only for "status" events)
	fields   map[string]bool // nil = all fields (only for "updated" events)
}

// NewSubscriber creates a subscriber from subscribe request options.
func NewSubscriber(conn net.Conn, opts Msg) *Subscriber {
	s := &Subscriber{conn: conn}
	s.UpdateFilters(opts)
	return s
}

// UpdateFilters replaces the subscriber's filters.
func (s *Subscriber) UpdateFilters(opts Msg) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions = toStringSet(opts["sessions"])
	s.events = toStringSet(opts["events"])
	s.statuses = toStringSet(opts["statuses"])
	s.fields = toStringSet(opts["fields"])
}

// Matches returns true if the event passes all filters.
func (s *Subscriber) Matches(event Msg) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Session filter
	if s.sessions != nil {
		sid, _ := event["sessionId"].(string)
		if !s.sessions[sid] {
			return false
		}
	}

	// Event type filter
	eventType, _ := event["event"].(string)
	if s.events != nil {
		if !s.events[eventType] {
			return false
		}
	}

	// Status filter (only for "status" events)
	if s.statuses != nil && eventType == "status" {
		status, _ := event["status"].(string)
		if !s.statuses[status] {
			return false
		}
	}

	// Fields filter (only for "updated" events)
	if s.fields != nil && eventType == "updated" {
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

// Send writes an event to the subscriber's connection.
func (s *Subscriber) Send(event Msg) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = s.conn.Write(data)
	return err
}

// Conn returns the underlying connection.
func (s *Subscriber) Conn() net.Conn {
	return s.conn
}

// SubscriberHub manages all active subscribers and maintains a short
// replay buffer so new subscribers receive events they narrowly missed
// (race between subscribe registration and event broadcasting).
type SubscriberHub struct {
	mu     sync.Mutex
	subs   map[*Subscriber]struct{}
	recent []Msg // replay buffer for recent events
}

const replayBufferSize = 100

// NewSubscriberHub creates a new hub.
func NewSubscriberHub() *SubscriberHub {
	return &SubscriberHub{
		subs:   make(map[*Subscriber]struct{}),
		recent: make([]Msg, 0, replayBufferSize),
	}
}

// Add registers a subscriber and replays recent events that match its filters.
// Holding the lock during both registration and replay ensures no event is
// delivered twice (via both replay and live broadcast).
func (h *SubscriberHub) Add(s *Subscriber) {
	h.mu.Lock()
	h.subs[s] = struct{}{}
	for _, event := range h.recent {
		if s.Matches(event) {
			s.Send(event)
		}
	}
	h.mu.Unlock()
}

// Remove unregisters a subscriber.
func (h *SubscriberHub) Remove(s *Subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// Broadcast sends an event to all matching subscribers and stores it
// in the replay buffer for late-arriving subscribers.
func (h *SubscriberHub) Broadcast(event Msg) {
	h.mu.Lock()
	// Store in replay buffer
	h.recent = append(h.recent, event)
	if len(h.recent) > replayBufferSize {
		h.recent = h.recent[1:]
	}
	// Snapshot current subscribers
	subs := make([]*Subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()

	// Send outside the lock to avoid blocking other operations
	for _, s := range subs {
		if s.Matches(event) {
			if err := s.Send(event); err != nil {
				log.Printf("subscriber send error: %v", err)
				h.Remove(s)
			}
		}
	}
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
