package wire

import (
	"context"
	"errors"
	"fmt"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

const (
	// ProtocolVersion is the only link protocol version.
	ProtocolVersion = 1
	// HelloTimeout bounds the raw Hello/Welcome exchange.
	HelloTimeout = 10 * time.Second
)

// ErrIncompatible reports a Welcome this client must not use: another
// protocol version, another principal, or a message limit above the local
// cap. Retrying the same configuration cannot succeed.
var ErrIncompatible = errors.New("wire: incompatible Welcome")

// RejectError is the server's Reject answer to Hello.
type RejectError struct{ Reject *circusv1.Reject }

func (e *RejectError) Error() string { return "wire: Hello rejected: " + e.Reject.GetCode().String() }

// downlink returns the envelope kind that answers an uplink kind.
func downlink(up Kind) (Kind, error) {
	switch up {
	case DaemonUplink:
		return DaemonDownlink, nil
	case WorkerUplink:
		return WorkerDownlink, nil
	default:
		return 0, fmt.Errorf("wire: %d is not an uplink kind", up)
	}
}

// DialHello runs the client half of the raw handshake on conn: it sends
// hello in an up envelope (DaemonUplink or WorkerUplink), reads one answer
// and returns the Welcome with the link options derived from it and local.
// A Reject is returned as *RejectError. A Welcome for another version or
// principalID, or with a message limit above local's MaxMessageBytes (zero:
// the default), wraps ErrIncompatible.
func DialHello(ctx context.Context, conn Conn, up Kind, hello *circusv1.Hello, principalID string, local Options) (*circusv1.Welcome, Options, error) {
	down, err := downlink(up)
	if err != nil {
		return nil, local, err
	}
	data, err := proto.Marshal(wrap(up, "hello", hello))
	if err != nil {
		return nil, local, err
	}
	if err = conn.WriteMessage(ctx, data); err != nil {
		return nil, local, err
	}
	if data, err = conn.ReadMessage(ctx); err != nil {
		return nil, local, err
	}
	m, err := Decode(down, data, MaxMetadataBytes)
	if err != nil {
		return nil, local, err
	}
	var welcome *circusv1.Welcome
	switch v := body(m).(type) {
	case *circusv1.Reject:
		return nil, local, &RejectError{Reject: v}
	case *circusv1.Welcome:
		welcome = v
	default:
		return nil, local, errors.New("wire: expected Welcome or Reject")
	}
	if welcome.NegotiatedVersion != ProtocolVersion {
		return nil, local, fmt.Errorf("%w: negotiated protocol version %d", ErrIncompatible, welcome.NegotiatedVersion)
	}
	if welcome.PrincipalId != principalID {
		return nil, local, fmt.Errorf("%w: Welcome principal does not match credentials", ErrIncompatible)
	}
	opts, err := OptionsFromWelcome(welcome, local)
	if err != nil {
		return nil, local, err
	}
	localMax := local.MaxMessageBytes
	if localMax == 0 {
		localMax = DefaultMaxMessageBytes
	}
	if opts.MaxMessageBytes > localMax {
		return nil, local, fmt.Errorf("%w: negotiated message limit exceeds local cap", ErrIncompatible)
	}
	return welcome, opts, nil
}

// AcceptHello runs the server half of the raw handshake on conn: it reads
// one Hello in an up envelope and answers it. A Hello that does not decode
// or for which welcome returns nil is refused with BAD_HELLO, one for
// another protocol version with VERSION_UNSUPPORTED; the Reject is written,
// conn is shut down and an error returned. Otherwise the Welcome (welcome
// sets its identity fields) gets the negotiated version and opts's limits
// (FillWelcome) and is written. It returns the Hello and the link options.
func AcceptHello(ctx context.Context, conn Conn, up Kind, opts Options, welcome func(*circusv1.Hello) *circusv1.Welcome) (*circusv1.Hello, Options, error) {
	down, err := downlink(up)
	if err != nil {
		return nil, opts, err
	}
	data, err := conn.ReadMessage(ctx)
	if err != nil {
		return nil, opts, err
	}
	code := circusv1.Reject_CODE_BAD_HELLO
	m, err := Decode(up, data, MaxMetadataBytes)
	if err == nil {
		if hello, ok := body(m).(*circusv1.Hello); ok {
			if w := welcome(hello); w == nil {
				code = circusv1.Reject_CODE_BAD_HELLO
			} else if hello.ProtocolVersion != ProtocolVersion {
				code = circusv1.Reject_CODE_VERSION_UNSUPPORTED
			} else {
				w.NegotiatedVersion = ProtocolVersion
				if opts, err = FillWelcome(w, opts); err != nil {
					return nil, opts, err
				}
				if data, err = proto.Marshal(wrap(down, "welcome", w)); err != nil {
					return nil, opts, err
				}
				return hello, opts, conn.WriteMessage(ctx, data)
			}
		}
	}
	data, _ = proto.Marshal(wrap(down, "reject", &circusv1.Reject{Code: code, MinVersion: ProtocolVersion, MaxVersion: ProtocolVersion}))
	_ = conn.WriteMessage(ctx, data)
	_ = conn.Shutdown(ctx)
	return nil, opts, fmt.Errorf("wire: rejected Hello: %s", code)
}
