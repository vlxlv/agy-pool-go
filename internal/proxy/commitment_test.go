package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponseCommitter_StateTransitions(t *testing.T) {
	rec := httptest.NewRecorder()
	c := NewResponseCommitter(rec)

	if c.State() != NotCommitted {
		t.Fatalf("initial state = %v, want NotCommitted", c.State())
	}
	if c.IsCommitted() {
		t.Fatal("expected IsCommitted() == false initially")
	}

	c.WriteHeader(http.StatusOK)
	if c.State() != HeadersCommitted {
		t.Fatalf("after WriteHeader, state = %v, want HeadersCommitted", c.State())
	}
	if !c.IsCommitted() {
		t.Fatal("expected IsCommitted() == true after WriteHeader")
	}

	c.MarkStream()
	if c.State() != StreamCommitted {
		t.Fatalf("after MarkStream, state = %v, want StreamCommitted", c.State())
	}

	c.MarkBody()
	if c.State() != BodyCommitted {
		t.Fatalf("after MarkBody, state = %v, want BodyCommitted", c.State())
	}
}

func TestResponseCommitter_WriteTransitionsState(t *testing.T) {
	rec := httptest.NewRecorder()
	c := NewResponseCommitter(rec)

	_, err := c.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("unexpected write error: %v", err)
	}
	if !c.IsCommitted() {
		t.Fatal("expected IsCommitted() == true after Write")
	}
}

func TestSendStream_CommitAndTruncationError(t *testing.T) {
	rec := httptest.NewRecorder()
	committer := NewResponseCommitter(rec)

	// Upstream reader that sends partial data and then fails with unexpected error
	failingReader := &errReader{
		data: []byte("event: chunk1\n\n"),
		err:  errors.New("connection reset by peer"),
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "text/event-stream")

	err := SendStream(committer, http.StatusOK, headers, failingReader)
	if err == nil {
		t.Fatal("expected SendStream to return error on mid-stream failure")
	}
	if !errors.Is(err, ErrCommittedStream) {
		t.Fatalf("expected ErrCommittedStream, got: %v", err)
	}
	if committer.State() != StreamCommitted {
		t.Fatalf("state = %v, want StreamCommitted", committer.State())
	}
	if !committer.IsCommitted() {
		t.Fatal("expected committer to be committed")
	}
}

func TestSendBuffered_CommitState(t *testing.T) {
	rec := httptest.NewRecorder()
	committer := NewResponseCommitter(rec)

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")

	SendBuffered(committer, http.StatusOK, headers, []byte(`{"status":"ok"}`))

	if committer.State() != BodyCommitted {
		t.Fatalf("state = %v, want BodyCommitted", committer.State())
	}
	if !committer.IsCommitted() {
		t.Fatal("expected committer to be committed")
	}
	if rec.Body.String() != `{"status":"ok"}` {
		t.Fatalf("body = %q, want %q", rec.Body.String(), `{"status":"ok"}`)
	}
}

type errReader struct {
	data []byte
	err  error
	read bool
}

func (r *errReader) Read(p []byte) (n int, err error) {
	if !r.read && len(r.data) > 0 {
		r.read = true
		n = copy(p, r.data)
		return n, nil
	}
	return 0, r.err
}

func (r *errReader) Close() error {
	return nil
}

func TestDispatchAction_String(t *testing.T) {
	if ActionFailoverNext.String() != "ActionFailoverNext" {
		t.Fatalf("want ActionFailoverNext, got %s", ActionFailoverNext.String())
	}
	if ActionTerminal.String() != "ActionTerminal" {
		t.Fatalf("want ActionTerminal, got %s", ActionTerminal.String())
	}
	if DispatchAction(99).String() != "Unknown" {
		t.Fatalf("want Unknown, got %s", DispatchAction(99).String())
	}
	if CommitState(99).String() != "Unknown" {
		t.Fatalf("want Unknown, got %s", CommitState(99).String())
	}
	if NotCommitted.String() != "NotCommitted" {
		t.Fatalf("want NotCommitted, got %s", NotCommitted.String())
	}
}
