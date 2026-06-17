package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLoggerPreservesFlusher proves the Logger wrapper still exposes
// http.Flusher to the handler. Without it, the SSE task stream's
// `w.(http.Flusher)` assertion fails against *statusWriter and flushing becomes
// a no-op, so streamed events stay buffered until the connection closes and the
// browser never sees incremental task updates.
func TestLoggerPreservesFlusher(t *testing.T) {
	var sawFlusher bool
	handler := Logger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/image/tasks/stream", nil)
	// httptest.ResponseRecorder implements http.Flusher, so the wrapper must
	// surface it through.
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !sawFlusher {
		t.Fatal("handler did not see http.Flusher through the Logger wrapper")
	}
}

// TestStatusWriterFlushDelegates verifies Flush forwards to the underlying
// writer rather than silently dropping.
func TestStatusWriterFlushDelegates(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	sw.Flush()
	if !rec.Flushed {
		t.Fatal("statusWriter.Flush did not delegate to the underlying ResponseWriter")
	}
}
