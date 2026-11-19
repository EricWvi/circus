package webapi

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// streamWriteChunk is the largest piece streamWriter writes under one
// deadline: a write fails only when no chunk completed within the write
// timeout, so a slow link that keeps moving is never cut, whatever the
// message size.
const streamWriteChunk = 32 << 10

// streamWriterKey carries a SessionService request's *streamWriter.
type streamWriterKey struct{}

// withStreamWriter wraps every SessionService response in a streamWriter
// with write timeout timeout. When the handler returns, net/http still
// writes the end of the response (the chunked terminator) after it, so the
// deadline is set to now + timeout rather than cleared: that last write is
// bounded too. The server's top-level handler clears the deadline at the
// start of every request, so a kept-alive connection's next request does
// not inherit it.
func withStreamWriter(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		sw := &streamWriter{ResponseWriter: w, setDeadline: rc.SetWriteDeadline, timeout: timeout}
		defer func() { _ = rc.SetWriteDeadline(time.Now().Add(timeout)) }()
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), streamWriterKey{}, sw)))
	})
}

// streamWriter bounds a response's writes by progress: before every chunk of
// at most streamWriteChunk bytes it moves the connection's write deadline
// to now + timeout. abort moves it to now, failing a write in progress at
// once and every later one; abort and the deadline of each chunk are set
// under one lock, so a chunk started just before abort cannot push the
// deadline out again.
type streamWriter struct {
	http.ResponseWriter
	setDeadline func(time.Time) error
	timeout     time.Duration

	mu      sync.Mutex
	aborted bool
}

func (sw *streamWriter) Write(p []byte) (int, error) {
	written := 0
	for {
		n := min(len(p)-written, streamWriteChunk)
		sw.mu.Lock()
		deadline := time.Now().Add(sw.timeout)
		if sw.aborted {
			deadline = time.Now()
		}
		_ = sw.setDeadline(deadline)
		sw.mu.Unlock()
		m, err := sw.ResponseWriter.Write(p[written : written+n])
		written += m
		if err != nil || written == len(p) {
			return written, err
		}
	}
}

// abort fails the response's current and later writes.
func (sw *streamWriter) abort() {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.aborted = true
	_ = sw.setDeadline(time.Now())
}

// resume lets later writes run under the write timeout again: the stream's
// final status after an abort that did not interrupt a write.
func (sw *streamWriter) resume() {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.aborted = false
}

// Flush and Unwrap keep http.Flusher and http.ResponseController working
// through the wrapper.
func (sw *streamWriter) Flush() { _ = http.NewResponseController(sw.ResponseWriter).Flush() }

func (sw *streamWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }
