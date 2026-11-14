package wire_test

import (
	"errors"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"google.golang.org/protobuf/proto"
)

type handshakeResult struct {
	hello *circusv1.Hello
	opts  wire.Options
	err   error
}

// handshake runs AcceptHello with server options and DialHello with local
// options over a pipe.
func handshake(t *testing.T, up wire.Kind, hello *circusv1.Hello, principal string, server, local wire.Options, check func(*circusv1.Hello) bool) (handshakeResult, *circusv1.Welcome, wire.Options, error) {
	t.Helper()
	ctx := contextFor(t)
	client, srv := wiretest.Pipe(wiretest.Options{})
	defer client.Close()
	done := make(chan handshakeResult, 1)
	go func() {
		h, o, err := wire.AcceptHello(ctx, srv, up, server, func(h *circusv1.Hello) *circusv1.Welcome {
			if check != nil && !check(h) {
				return nil
			}
			return &circusv1.Welcome{PrincipalId: "p", InstanceId: "i", ServerTimeUnixMs: 1}
		})
		done <- handshakeResult{h, o, err}
	}()
	w, o, err := wire.DialHello(ctx, client, up, hello, principal, local)
	return <-done, w, o, err
}

func TestHelloHandshake(t *testing.T) {
	track(t)
	for _, up := range []wire.Kind{wire.DaemonUplink, wire.WorkerUplink} {
		hello := &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion, BootId: "boot"}
		srv, w, o, err := handshake(t, up, hello, "p", wire.Options{MaxMessageBytes: 1 << 20}, wire.Options{}, nil)
		if err != nil || srv.err != nil || srv.hello.GetBootId() != "boot" || w.NegotiatedVersion != wire.ProtocolVersion || w.ServerTimeUnixMs != 1 {
			t.Fatalf("%v: %v %+v %+v", up, err, srv, w)
		}
		if o.MaxMessageBytes != 1<<20 || srv.opts.MaxMessageBytes != 1<<20 {
			t.Fatalf("negotiated %d / %d", o.MaxMessageBytes, srv.opts.MaxMessageBytes)
		}
	}
}

func TestHelloHandshakeRefusals(t *testing.T) {
	track(t)
	var rejected *wire.RejectError
	hello := &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion}
	// Another protocol version, or a Hello the server refuses.
	srv, _, _, err := handshake(t, wire.WorkerUplink, &circusv1.Hello{ProtocolVersion: 99}, "p", wire.Options{}, wire.Options{}, nil)
	if !errors.As(err, &rejected) || rejected.Reject.Code != circusv1.Reject_CODE_VERSION_UNSUPPORTED || rejected.Reject.MaxVersion != wire.ProtocolVersion || srv.err == nil {
		t.Fatalf("version: %v / %v", err, srv.err)
	}
	srv, _, _, err = handshake(t, wire.DaemonUplink, hello, "p", wire.Options{}, wire.Options{}, func(*circusv1.Hello) bool { return false })
	if !errors.As(err, &rejected) || rejected.Reject.Code != circusv1.Reject_CODE_BAD_HELLO || srv.err == nil {
		t.Fatalf("refused: %v / %v", err, srv.err)
	}
	// A Welcome for another principal, or above the local message cap.
	if _, _, _, err = handshake(t, wire.WorkerUplink, hello, "other", wire.Options{}, wire.Options{}, nil); !errors.Is(err, wire.ErrIncompatible) {
		t.Fatalf("principal: %v", err)
	}
	if _, _, _, err = handshake(t, wire.WorkerUplink, hello, "p", wire.Options{MaxMessageBytes: 2 << 20}, wire.Options{MaxMessageBytes: 1 << 20}, nil); !errors.Is(err, wire.ErrIncompatible) {
		t.Fatalf("local cap: %v", err)
	}
	if _, _, _, err = handshake(t, wire.WorkerUplink, hello, "p", wire.Options{MaxMessageBytes: 2 * wire.DefaultMaxMessageBytes}, wire.Options{}, nil); !errors.Is(err, wire.ErrIncompatible) {
		t.Fatalf("default local cap: %v", err)
	}
}

// A Welcome that negotiates another protocol version is refused, even if
// the server accepted the Hello.
func TestDialHelloRefusesNegotiatedVersion(t *testing.T) {
	track(t)
	ctx := contextFor(t)
	client, srv := wiretest.Pipe(wiretest.Options{})
	defer client.Close()
	go func() {
		if _, err := srv.ReadMessage(ctx); err != nil {
			return
		}
		w := &circusv1.Welcome{PrincipalId: "p", NegotiatedVersion: wire.ProtocolVersion + 1}
		if _, err := wire.FillWelcome(w, wire.Options{}); err != nil {
			return
		}
		data, _ := proto.Marshal(&circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_Welcome{Welcome: w}})
		_ = srv.WriteMessage(ctx, data)
	}()
	_, _, err := wire.DialHello(ctx, client, wire.WorkerUplink, &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion}, "p", wire.Options{})
	if !errors.Is(err, wire.ErrIncompatible) {
		t.Fatalf("negotiated version: %v", err)
	}
}
