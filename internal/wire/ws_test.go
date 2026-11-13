package wire_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

func TestWebSocketGracefulShutdown(t *testing.T) {
	track(t)
	serverDone := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := wire.AcceptWebSocket(w, r, nil)
		if err != nil {
			serverDone <- err
			return
		}
		// Handshake stays on the raw connection.
		bytes, err := raw.ReadMessage(r.Context())
		if err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		if _, err = wire.Decode(wire.DaemonUplink, bytes, wire.MaxMetadataBytes); err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		hw := &circusv1.Welcome{PrincipalId: "daemon"}
		so, err := wire.FillWelcome(hw, wire.Options{})
		if err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		welcome, _ := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: hw}})
		if err = raw.WriteMessage(r.Context(), welcome); err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		l, err := wire.New(connWrapper{raw}, wire.DaemonControlSide, so)
		if err != nil {
			serverDone <- err
			_ = raw.Close()
			return
		}
		defer l.Close()
		err = testSend(l, r.Context(), &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{ReconnectAfterMs: 30000}}})
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), wait.Timeout)
			err = l.Shutdown(ctx)
			cancel()
		}
		serverDone <- err
	}))
	defer srv.Close()
	raw, _, err := wire.DialWebSocket(contextFor(t), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	hello, _ := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{ProtocolVersion: 1}}})
	if err = raw.WriteMessage(contextFor(t), hello); err != nil {
		t.Fatal(err)
	}
	welcome, err := raw.ReadMessage(contextFor(t))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := wire.Decode(wire.DaemonDownlink, welcome, wire.MaxMetadataBytes)
	if err != nil || decoded.(*circusv1.DaemonDownlink).GetWelcome().GetPrincipalId() != "daemon" {
		t.Fatal("raw Welcome", err)
	}
	co, err := wire.OptionsFromWelcome(decoded.(*circusv1.DaemonDownlink).GetWelcome(), wire.Options{})
	if err != nil {
		t.Fatal(err)
	}
	l := newLink(t, connWrapper{raw}, wire.DaemonDownlink, wire.DaemonUplink, co)
	wait.Recv(t, l.Done())
	if receive(t, l).(*circusv1.DaemonDownlink).GetGoAway() == nil {
		t.Fatal("GoAway lost")
	}
	if websocket.CloseStatus(l.Err()) != websocket.StatusNormalClosure {
		t.Fatal("missing normal close handshake", l.Err())
	}
	if err := wait.Recv(t, serverDone); err != nil {
		t.Fatal(err)
	}
}
func TestWebSocketHandshakeDeadline(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			track(t)
			done := make(chan error, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := wire.AcceptWebSocket(w, r, nil)
				if err != nil {
					done <- err
					return
				}
				defer raw.Close()
				if _, err = raw.ReadMessage(r.Context()); err != nil {
					done <- err
					return
				}
				hw := &circusv1.Welcome{}
				so, err := wire.FillWelcome(hw, wire.Options{})
				if err != nil {
					done <- err
					return
				}
				welcome, _ := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: hw}})
				if err = raw.WriteMessage(r.Context(), welcome); err != nil {
					done <- err
					return
				}
				l, err := wire.New(connWrapper{raw}, wire.DaemonControlSide, so)
				if err != nil {
					done <- err
					return
				}
				defer l.Close()
				ctx, cancel := context.WithTimeout(r.Context(), 20*time.Millisecond)
				defer cancel()
				start := time.Now()
				err = l.Shutdown(ctx)
				if time.Since(start) > time.Second {
					done <- fmt.Errorf("close handshake ignored deadline")
					return
				}
				done <- err
			})
			var srv *httptest.Server
			if secure {
				srv = httptest.NewTLSServer(handler)
			} else {
				srv = httptest.NewServer(handler)
			}
			defer srv.Close()
			raw, _, err := wire.DialWebSocket(contextFor(t), "ws"+strings.TrimPrefix(srv.URL, "http"), &websocket.DialOptions{HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			hello, _ := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{}}})
			if err = raw.WriteMessage(contextFor(t), hello); err != nil {
				t.Fatal(err)
			}
			if _, err = raw.ReadMessage(contextFor(t)); err != nil {
				t.Fatal(err)
			}
			// Deliberately do not read the close frame, so no peer acknowledgement occurs.
			if err := wait.Recv(t, done); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func TestWebSocketNegotiatedReadLimit(t *testing.T) {
	track(t)
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := wire.AcceptWebSocket(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		if _, err = raw.ReadMessage(r.Context()); err != nil {
			done <- err
			return
		}
		hw := &circusv1.Welcome{}
		so, err := wire.FillWelcome(hw, wire.Options{MaxFragmentBytes: wire.DefaultMaxFragmentBytes})
		if err != nil {
			done <- err
			return
		}
		welcome, _ := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: hw}})
		if err = raw.WriteMessage(r.Context(), welcome); err != nil {
			done <- err
			return
		}
		l, err := wire.New(connWrapper{raw}, wire.DaemonControlSide, so)
		if err != nil {
			done <- err
			return
		}
		defer l.Close()
		ctx, cancel := context.WithTimeout(r.Context(), wait.Timeout)
		defer cancel()
		m, err := l.Receive(ctx)
		if err == nil && !proto.Equal(m, result(strings.Repeat("x", 300<<10))) {
			err = fmt.Errorf("negotiated large frame content")
		}
		done <- err
	}))
	defer srv.Close()
	raw, _, err := wire.DialWebSocket(contextFor(t), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	hello, _ := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{}}})
	if err = raw.WriteMessage(contextFor(t), hello); err != nil {
		t.Fatal(err)
	}
	welcome, err := raw.ReadMessage(contextFor(t))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := wire.Decode(wire.DaemonDownlink, welcome, wire.MaxMetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	co, err := wire.OptionsFromWelcome(decoded.(*circusv1.DaemonDownlink).GetWelcome(), wire.Options{})
	if err != nil || co.MaxFragmentBytes != wire.DefaultMaxFragmentBytes {
		t.Fatal(co, err)
	}
	l := newLink(t, connWrapper{raw}, wire.DaemonDownlink, wire.DaemonUplink, co)
	send(t, l, result(strings.Repeat("x", 300<<10)))
	if err := wait.Recv(t, done); err != nil {
		t.Fatal(err)
	}
}

type connWrapper struct{ wire.Conn }
