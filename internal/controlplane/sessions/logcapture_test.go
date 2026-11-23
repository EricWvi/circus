package sessions_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/controlplane/sessions"
)

// logRecord is one captured record: its level, message and attributes
// (formatted with slog.Value.String).
type logRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// logSink is what every handler derived from one capture shares.
type logSink struct {
	mu   sync.Mutex
	raw  bytes.Buffer // every record, formatted by a text handler
	recs []logRecord
}

// capture is a slog.Handler that keeps every record at or above level, both
// as attributes and as formatted text.
type capture struct {
	sink  *logSink
	level slog.Level
	attrs []slog.Attr
	text  slog.Handler
}

func newCapture(level slog.Level) *capture {
	s := &logSink{}
	return &capture{sink: s, level: level, text: slog.NewTextHandler(lockedWriter{s}, &slog.HandlerOptions{Level: level})}
}

// lockedWriter writes to the sink's buffer; the sink's lock is held by
// Handle around the text handler's write.
type lockedWriter struct{ s *logSink }

func (w lockedWriter) Write(p []byte) (int, error) { return w.s.raw.Write(p) }

func (c *capture) Enabled(_ context.Context, l slog.Level) bool { return l >= c.level }

func (c *capture) Handle(ctx context.Context, r slog.Record) error {
	rec := logRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	for _, a := range c.attrs {
		rec.attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	c.sink.mu.Lock()
	defer c.sink.mu.Unlock()
	c.sink.recs = append(c.sink.recs, rec)
	return c.text.Handle(ctx, r)
}

func (c *capture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &capture{sink: c.sink, level: c.level, attrs: append(append([]slog.Attr(nil), c.attrs...), attrs...), text: c.text.WithAttrs(attrs)}
}

func (c *capture) WithGroup(name string) slog.Handler {
	return &capture{sink: c.sink, level: c.level, attrs: c.attrs, text: c.text.WithGroup(name)}
}

// logger returns a logger writing to c.
func (c *capture) logger() *slog.Logger { return slog.New(c) }

// raw returns everything captured, as formatted text.
func (c *capture) raw() string {
	c.sink.mu.Lock()
	defer c.sink.mu.Unlock()
	return c.sink.raw.String()
}

// matching returns the records with message msg whose attributes include
// every key-value pair of kv.
func (c *capture) matching(msg string, kv ...string) []logRecord {
	c.sink.mu.Lock()
	defer c.sink.mu.Unlock()
	var out []logRecord
	for _, r := range c.sink.recs {
		if r.msg != msg {
			continue
		}
		ok := true
		for i := 0; i+1 < len(kv); i += 2 {
			if v, found := r.attrs[kv[i]]; !found || v != kv[i+1] {
				ok = false
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

// want checks that a record with message msg and the attributes kv was
// captured and returns the first. It does not wait: the caller first waits
// for the step that writes the record (a reply on the link, the scheduler
// going idle, the link's server side being released).
func (c *capture) want(t *testing.T, msg string, kv ...string) logRecord {
	t.Helper()
	got := c.matching(msg, kv...)
	if len(got) == 0 {
		t.Fatalf("no %q record with %v; captured:\n%s", msg, kv, c.raw())
	}
	return got[0]
}

// withLogger makes the session service (and the worker link) log to c.
func withLogger(c *capture) option {
	return func(cfg *sessions.Config) { cfg.Logger = c.logger() }
}

// kv formats a value as the capture stores it.
func kv(v any) string { return fmt.Sprint(v) }
