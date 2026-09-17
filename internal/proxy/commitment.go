package proxy

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"sync"
)

// CommitState represents the downstream response commitment state.
type CommitState int

const (
	// NotCommitted indicates no headers or body bytes have been sent downstream.
	NotCommitted CommitState = iota
	// HeadersCommitted indicates downstream status code and headers have been written.
	HeadersCommitted
	// BodyCommitted indicates a complete buffered response has been written downstream.
	BodyCommitted
	// StreamCommitted indicates streaming response has begun (headers flushed, chunks flowing).
	StreamCommitted
)

func (s CommitState) String() string {
	switch s {
	case NotCommitted:
		return "NotCommitted"
	case HeadersCommitted:
		return "HeadersCommitted"
	case BodyCommitted:
		return "BodyCommitted"
	case StreamCommitted:
		return "StreamCommitted"
	default:
		return "Unknown"
	}
}

// Sentinel typed errors for terminal states where cross-account replay is forbidden.
var (
	ErrCommittedStream    = errors.New("stream committed to downstream; failover prohibited")
	ErrClientDisconnected = errors.New("client disconnected; failover prohibited")
	ErrAmbiguousTransport = errors.New("ambiguous transport failure; failover prohibited")
)

// ResponseCommitter wraps http.ResponseWriter to track and mechanically enforce commitment.
type ResponseCommitter struct {
	w      http.ResponseWriter
	mu     sync.Mutex
	state  CommitState
	status int
}

// NewResponseCommitter constructs a ResponseCommitter wrapping w.
func NewResponseCommitter(w http.ResponseWriter) *ResponseCommitter {
	if rc, ok := w.(*ResponseCommitter); ok {
		return rc
	}
	return &ResponseCommitter{
		w:     w,
		state: NotCommitted,
	}
}

// State returns the current CommitState.
func (c *ResponseCommitter) State() CommitState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// IsCommitted returns true if headers or data have been written downstream.
func (c *ResponseCommitter) IsCommitted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state != NotCommitted
}

// Header returns the underlying ResponseWriter's header map.
func (c *ResponseCommitter) Header() http.Header {
	return c.w.Header()
}

// WriteHeader writes the status code and transitions state to HeadersCommitted.
func (c *ResponseCommitter) WriteHeader(status int) {
	c.mu.Lock()
	if c.state == NotCommitted {
		c.state = HeadersCommitted
		c.status = status
	}
	c.mu.Unlock()
	c.w.WriteHeader(status)
}

// Write writes data downstream and ensures state is at least HeadersCommitted.
func (c *ResponseCommitter) Write(b []byte) (int, error) {
	c.mu.Lock()
	if c.state == NotCommitted {
		c.state = HeadersCommitted
	}
	c.mu.Unlock()
	return c.w.Write(b)
}

// MarkStream marks the response commitment state as StreamCommitted.
func (c *ResponseCommitter) MarkStream() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StreamCommitted
}

// MarkBody marks the response commitment state as BodyCommitted.
func (c *ResponseCommitter) MarkBody() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = BodyCommitted
}

// Flush flushes buffered data if the underlying ResponseWriter implements http.Flusher.
func (c *ResponseCommitter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to underlying ResponseWriter if it implements http.Hijacker.
func (c *ResponseCommitter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := c.w.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

// Unwrap returns the underlying http.ResponseWriter.
func (c *ResponseCommitter) Unwrap() http.ResponseWriter {
	return c.w
}

// DispatchAction represents the replay/failover decision for the outer loop.
type DispatchAction int

const (
	// ActionFailoverNext indicates the error is an ExplicitRetryableUpstreamResponse
	// (or pre-dispatch token refresh error). The outer loop may advance to the next account.
	ActionFailoverNext DispatchAction = iota

	// ActionTerminal indicates the dispatch reached a terminal state.
	// Cross-account failover is structurally forbidden.
	ActionTerminal
)

func (a DispatchAction) String() string {
	switch a {
	case ActionFailoverNext:
		return "ActionFailoverNext"
	case ActionTerminal:
		return "ActionTerminal"
	default:
		return "Unknown"
	}
}

// DispatchOutcome encapsulates the decision and cause for an account dispatch attempt.
type DispatchOutcome struct {
	Action DispatchAction
	Reason string
	Err    error
}
