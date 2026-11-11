package wire

import (
	"fmt"
	"math"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// FillWelcome is the server half of limit negotiation. It writes o's effective
// limits (after defaults) into w as explicit values, never 0, and returns the
// Options the server must pass to New. The client derives the same limits with
// OptionsFromWelcome, so defaults compiled into two different binaries cannot
// make the sides disagree. Identity fields of w are left untouched. Values the
// wire cannot carry exactly (sub-millisecond durations, overflow) are errors.
func FillWelcome(w *circusv1.Welcome, o Options) (Options, error) {
	if w == nil {
		return o, fmt.Errorf("wire: missing Welcome")
	}
	o, err := o.defaults()
	if err != nil {
		return o, err
	}
	ms := func(d time.Duration) (uint32, bool) {
		return uint32(d / time.Millisecond), d%time.Millisecond == 0 && d/time.Millisecond <= math.MaxUint32
	}
	interval, okInterval := ms(o.HeartbeatInterval)
	timeout, okTimeout := ms(o.HeartbeatTimeout)
	if !okInterval || !okTimeout || uint64(o.MaxFragmentBytes) > math.MaxUint32 {
		return o, fmt.Errorf("wire: limits not representable in Welcome")
	}
	w.HeartbeatIntervalMs, w.HeartbeatTimeoutMs = interval, timeout
	w.MaxMessageBytes = uint64(o.MaxMessageBytes)
	w.MaxFragmentBytes = uint32(o.MaxFragmentBytes)
	w.MaxInFlightMessages = uint32(o.MaxInFlightMessages) // validated <= 2
	w.MaxInFlightBytes = uint64(o.MaxInFlightBytes)
	return o, nil
}

// OptionsFromWelcome is the client half: it applies the peer-agreed fields,
// preserving local-only base options (clock, shared budget, queues, consumer
// stall and fragment timeout). FillWelcome never writes 0; a zero field is
// accepted only as the protocol default for compatibility with the proto
// comments. Invalid/unsupported values are never clamped.
func OptionsFromWelcome(w *circusv1.Welcome, base Options) (Options, error) {
	if w == nil {
		return base, fmt.Errorf("wire: missing Welcome")
	}
	maxInt := uint64(^uint(0) >> 1)
	if w.MaxMessageBytes > maxInt || w.MaxInFlightBytes > maxInt || uint64(w.MaxFragmentBytes) > maxInt || uint64(w.MaxInFlightMessages) > maxInt {
		return base, fmt.Errorf("wire: negotiated integer overflow")
	}
	base.MaxMessageBytes = int(w.MaxMessageBytes)
	base.MaxFragmentBytes = int(w.MaxFragmentBytes)
	base.MaxInFlightMessages = int(w.MaxInFlightMessages)
	base.MaxInFlightBytes = int(w.MaxInFlightBytes)
	base.HeartbeatInterval = time.Duration(w.HeartbeatIntervalMs) * time.Millisecond
	base.HeartbeatTimeout = time.Duration(w.HeartbeatTimeoutMs) * time.Millisecond
	// uint32 milliseconds fits time.Duration on all supported Go targets.
	return base.defaults()
}
