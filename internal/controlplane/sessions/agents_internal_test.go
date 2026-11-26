package sessions

import (
	"testing"
	"time"
)

// TestRetryDelay: the background reads of a worker's agents back off by
// doubling from agentRetryFirst and never wait longer than agentRetryMax.
func TestRetryDelay(t *testing.T) {
	var got []time.Duration
	for d := agentRetryFirst; len(got) < 10; d = retryDelay(d) {
		got = append(got, d)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delays %v, want %v", got, want)
		}
	}
}
