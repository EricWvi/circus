package controlplane

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// accessLog logs one line per request: method, path, status and duration.
// The query string, headers and body are not logged, so neither a bearer
// token nor an enrollment token can reach the log. Health probes log at
// debug level.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}
		status := rec.status
		if status == 0 && !rec.hijacked {
			status = http.StatusOK
		}
		s.logger.Log(r.Context(), level, "http request",
			"method", clip(r.Method), "path", clip(r.URL.Path), "status", status, "hijacked", rec.hijacked,
			"duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

// statusRecorder records the status and keeps the writer's optional
// interfaces reachable: Unwrap for http.ResponseController and WebSocket
// libraries, Hijack and Flush directly.
type statusRecorder struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() { _ = http.NewResponseController(r.ResponseWriter).Flush() }

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("controlplane: response writer does not support hijacking")
	}
	c, rw, err := h.Hijack()
	if err == nil {
		r.hijacked, r.status = true, http.StatusSwitchingProtocols
	}
	return c, rw, err
}
