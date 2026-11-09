// Package maintenance coordinates an authenticated update without interrupting work.
package maintenance

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

const LeaseDuration = 2 * time.Minute

type contextKey uint8

const (
	connectionKey contextKey = iota
	requestKey
)

// Controller fences admission and tracks requests through net/http's final flush.
// It deliberately does not change the server's ordinary immediate Stop behavior.
type Controller struct {
	mu       sync.Mutex
	now      func() time.Time
	instance string
	owner    string
	expires  time.Time
	stopping bool
	signal   chan struct{}
	requests map[*request]struct{}
	sessions map[*Session]struct{}
}

type request struct {
	controller   *Controller
	conn         net.Conn
	returned     bool
	flushed      bool
	hijacked     bool
	socketClosed bool
	http2        bool
}

// Session remains registered until the WebSocket handler and upstream cleanup finish.
type Session struct {
	controller *Controller
	busy       bool
}

// Status contains counts only; lease tokens and request/session contents are never exposed.
type Status struct {
	SchemaVersion        int    `json:"schema_version"`
	InstanceID           string `json:"instance_id"`
	Draining             bool   `json:"draining"`
	Stopping             bool   `json:"stopping"`
	LeaseRemainingMS     int64  `json:"lease_remaining_ms"`
	ActiveRequests       int    `json:"active_requests"`
	WebsocketSessions    int    `json:"websocket_sessions"`
	ActiveWebsocketTurns int    `json:"active_websocket_turns"`
	Ready                bool   `json:"ready"`
}

func New() *Controller {
	return &Controller{now: time.Now, instance: uuid.NewString(), signal: make(chan struct{}),
		requests: make(map[*request]struct{}), sessions: make(map[*Session]struct{})}
}

func (d *Controller) expireLocked() {
	if d.owner != "" && !d.stopping && !d.now().Before(d.expires) {
		d.owner = ""
		d.signal = make(chan struct{})
	}
}

func (d *Controller) statusLocked() Status {
	d.expireLocked()
	s := Status{SchemaVersion: 1, InstanceID: d.instance, Draining: d.owner != "", Stopping: d.stopping,
		ActiveRequests: len(d.requests), WebsocketSessions: len(d.sessions)}
	if s.Draining && !s.Stopping {
		s.LeaseRemainingMS = max(0, d.expires.Sub(d.now()).Milliseconds())
	}
	for session := range d.sessions {
		if session.busy {
			s.ActiveWebsocketTurns++
		}
	}
	s.Ready = s.Draining && !s.Stopping && s.ActiveRequests == 0 && s.WebsocketSessions == 0
	return s
}

func (d *Controller) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statusLocked()
}

// Begin is idempotent for its owner; retries never extend a lease.
func (d *Controller) Begin(instance, owner string) (Status, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked()
	if instance != d.instance || len(owner) < 32 || len(owner) > 128 || d.stopping || (d.owner != "" && d.owner != owner) {
		return d.statusLocked(), false
	}
	if d.owner == "" {
		d.owner = owner
		d.expires = d.now().Add(LeaseDuration)
		close(d.signal)
	}
	return d.statusLocked(), true
}

func (d *Controller) Cancel(instance, owner string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked()
	if instance != d.instance || owner == "" || owner != d.owner || d.stopping {
		return false
	}
	d.owner = ""
	d.signal = make(chan struct{})
	return true
}

// CommitStop checks readiness and permanently fences admission in the same lock.
// The caller must stop THIS server after success. An external readiness check
// followed by killing a PID is unsafe: its lease could expire in between.
func (d *Controller) CommitStop(instance, owner string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.statusLocked()
	if instance != d.instance || owner == "" || owner != d.owner || !s.Ready {
		return false
	}
	d.stopping = true
	return true
}

func (d *Controller) ConnContext(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, connectionKey, conn)
}

// Idle follows net/http's response flush. Closed also proves no bytes can remain
// in the server's user-space buffer. HTTP/2 can report Idle before its final
// frame is flushed, so it must close naturally. Hijacked sockets wait for Close.
func (d *Controller) ConnState(conn net.Conn, state http.ConnState) {
	if state != http.StateIdle && state != http.StateClosed && state != http.StateHijacked {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for ticket := range d.requests {
		if ticket.conn == conn {
			if ticket.http2 && state == http.StateIdle {
				continue
			}
			ticket.flushed = true
			if state == http.StateClosed {
				ticket.socketClosed = true
			}
			if ticket.returned && (!ticket.hijacked || ticket.socketClosed) {
				delete(d.requests, ticket)
			}
		}
	}
}

func (d *Controller) admit(r *http.Request) (*request, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked()
	if d.owner != "" {
		return nil, false
	}
	conn, _ := r.Context().Value(connectionKey).(net.Conn)
	ticket := &request{controller: d, conn: conn, http2: r.ProtoMajor >= 2}
	d.requests[ticket] = struct{}{}
	return ticket, true
}

func (t *request) finish() {
	d := t.controller
	d.mu.Lock()
	defer d.mu.Unlock()
	t.returned = true
	if t.flushed && (!t.hijacked || t.socketClosed) {
		delete(d.requests, t)
	}
}

// Wrap covers custom routes as well as inference aliases. Only the caller's
// explicit health/management exceptions bypass the admission fence.
func (d *Controller) Wrap(next http.Handler, exempt func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt != nil && exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		ticket, ok := d.admit(r)
		if !ok {
			w.Header().Set("Retry-After", "2")
			w.Header().Set("Connection", "close")
			http.Error(w, "proxy updating; retry shortly", http.StatusServiceUnavailable)
			return
		}
		defer ticket.finish()
		next.ServeHTTP(&requestWriter{ResponseWriter: w, ticket: ticket}, r.WithContext(context.WithValue(r.Context(), requestKey, ticket)))
	})
}

// ManageWebsocket atomically converts its upgrade request to a managed session.
// Unknown WebSockets stay counted until both handler exit and socket Close.
func ManageWebsocket(ctx context.Context) *Session {
	ticket, _ := ctx.Value(requestKey).(*request)
	if ticket == nil {
		return nil
	}
	d := ticket.controller
	d.mu.Lock()
	defer d.mu.Unlock()
	session := &Session{controller: d, busy: true}
	d.sessions[session] = struct{}{}
	delete(d.requests, ticket)
	return session
}

// Waiting marks a safe turn boundary. Pending tool calls must pass true.
// The channel closes when maintenance begins. Pending tool work has no idle
// close signal, so its already admitted continuation can finish first.
func (s *Session) Waiting(pendingTools bool) <-chan struct{} {
	if s == nil {
		return nil
	}
	d := s.controller
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked()
	s.busy = pendingTools
	if pendingTools {
		return nil
	}
	return d.signal
}

// BeginTurn races atomically with Begin. A pending tool continuation is already
// part of admitted work and is allowed to finish even during a drain.
func (s *Session) BeginTurn() bool {
	if s == nil {
		return true
	}
	d := s.controller
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked()
	if d.owner != "" && !s.busy {
		return false
	}
	s.busy = true
	return true
}

func (s *Session) Close() {
	if s == nil {
		return
	}
	d := s.controller
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.sessions, s)
}

// Hijacked handlers may return while a background goroutine owns the socket,
// as the AI Studio relay does. Track its actual Close rather than handler return.
type requestWriter struct {
	http.ResponseWriter
	ticket *request
}

func (w *requestWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *requestWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *requestWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	d := w.ticket.controller
	d.mu.Lock()
	w.ticket.hijacked = true
	d.mu.Unlock()
	return &hijackedConn{Conn: conn, ticket: w.ticket}, rw, nil
}
func (w *requestWriter) CloseNotify() <-chan bool {
	if notifier, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return notifier.CloseNotify()
	}
	return nil
}
func (w *requestWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

type hijackedConn struct {
	net.Conn
	ticket *request
}

func (c *hijackedConn) Close() error {
	err := c.Conn.Close()
	d := c.ticket.controller
	d.mu.Lock()
	c.ticket.socketClosed = true
	if c.ticket.returned {
		delete(d.requests, c.ticket)
	}
	d.mu.Unlock()
	return err
}
