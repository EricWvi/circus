package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wirepolicy"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
)

// TestMeasureReceivePoolShare is the Q33 measurement (calls/DESIGN.md
// "Capacity planning"): one shared receive pool serves every daemon link and
// there is no per-link share. N devices connect to the real gateway; K of
// them push large results (8-64 MiB) to a slow consumer, the others send a
// small (1 KiB, within the 64 KiB link reserve) result every 50 ms, a
// medium (256 KiB) one every second and a 4 MiB one every 5 s, all consumed
// at once. It reports their delivery latency and starvation, link closures
// by cause and pool high-water marks, with and without the large devices.
// Opt-in: CIRCUS_MEASURE=1; CIRCUS_MEASURE_SECONDS (default 45),
// CIRCUS_MEASURE_SLOW_MIBPS (large consumer rate, default 50) and
// CIRCUS_MEASURE_SCENARIOS (names) tune it.
func TestMeasureReceivePoolShare(t *testing.T) {
	if os.Getenv("CIRCUS_MEASURE") != "1" {
		t.Skip("measurement; set CIRCUS_MEASURE=1 (see calls/DESIGN.md, Capacity planning and deviation 1)")
	}
	duration := 45 * time.Second
	if v := os.Getenv("CIRCUS_MEASURE_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		duration = time.Duration(n) * time.Second
	}
	rate := 50
	if v := os.Getenv("CIRCUS_MEASURE_SLOW_MIBPS"); v != "" {
		var err error
		if rate, err = strconv.Atoi(v); err != nil {
			t.Fatal(err)
		}
	}
	opts, err := wire.FillWelcome(&circusv1.Welcome{}, wire.Options{})
	if err != nil {
		t.Fatal(err)
	}
	minSend, minReceive, err := wire.MinBudget(opts)
	if err != nil {
		t.Fatal(err)
	}
	const linkPeak = 462 << 20 // one link's receive phase bound (wire docs)
	type scenario struct {
		name           string
		devices, large int
		receive        int
	}
	var scenarios []scenario
	for _, n := range []int{4, 16, 64} {
		k := min(max(1, n/4), 8)
		scenarios = append(scenarios,
			scenario{fmt.Sprintf("n%d-none", n), n, 0, 2 * minReceive},
			scenario{fmt.Sprintf("n%d-k%d-2min", n, k), n, k, 2 * minReceive},
			scenario{fmt.Sprintf("n%d-k%d-640", n, k), n, k, 640 << 20})
		if k > 1 {
			scenarios = append(scenarios, scenario{fmt.Sprintf("n%d-k%d-k462", n, k), n, k, max(2*minReceive, k*linkPeak)})
		}
	}
	if v := os.Getenv("CIRCUS_MEASURE_SCENARIOS"); v != "" {
		keep := strings.Split(v, ",")
		scenarios = slices.DeleteFunc(scenarios, func(s scenario) bool { return !slices.Contains(keep, s.name) })
	}
	large := map[int]string{}
	for _, mib := range []int{8, 16, 32, 64} {
		large[mib] = strings.Repeat("b", mib<<20-64<<10) // fits the 64 MiB envelope
	}
	var latency, closures strings.Builder
	latency.WriteString("\n| scenario | class | sent | delivered | p50 | p99 | max | >1s | >5s |\n|---|---|---|---|---|---|---|---|---|\n")
	closures.WriteString("\n| scenario | N | K | recv pool MiB | recv peak MiB | large MiB/s | closed small: exhausted/stalled/other | closed large: exhausted/stalled/other |\n|---|---|---|---|---|---|---|---|\n")
	for _, s := range scenarios {
		m := runPoolScenario(t, s.devices, s.large, 2*minSend, s.receive, duration, rate, large)
		for _, class := range poolClasses {
			list := m.latency[class.prefix]
			fmt.Fprintf(&latency, "| %s | %s | %d | %d | %s | %s | %s | %.2f%% | %.2f%% |\n", s.name, class.name, m.sent[class.prefix], len(list), quantile(list, 0.5), quantile(list, 0.99), quantile(list, 1), over(list, time.Second), over(list, 5*time.Second))
		}
		fmt.Fprintf(&closures, "| %s | %d | %d | %d | %d | %.0f | %d/%d/%d | %d/%d/%d |\n", s.name, s.devices, s.large, s.receive>>20, m.peak>>20, float64(m.largeBytes)/float64(1<<20)/m.elapsed.Seconds(),
			m.closed["small/exhausted"], m.closed["small/stalled"], m.closed["small/other"],
			m.closed["large/exhausted"], m.closed["large/stalled"], m.closed["large/other"])
		t.Logf("%s done: closed %v (other: %v; daemon side: %v) peak %d MiB", s.name, m.closed, m.other, m.daemon, m.peak>>20)
	}
	t.Log(latency.String() + closures.String())
}

// poolClasses are the timed results of the ordinary devices: every period
// ticks (of 50 ms) each sends one of size bytes.
var poolClasses = []struct {
	name   string
	prefix byte
	size   int
	period int
}{{"1 KiB", 's', 1 << 10, 1}, {"256 KiB", 'm', 256 << 10, 20}, {"4 MiB", 'x', 4 << 20, 100}}

type poolMetrics struct {
	mu         sync.Mutex
	latency    map[byte][]time.Duration
	sent       map[byte]int64
	closed     map[string]int
	other      map[string]int // errors of closures counted as other
	daemon     map[string]int // daemon-side link errors
	largeBytes int64
	peak       int
	elapsed    time.Duration
}

// errorKind drops socket addresses so equal causes count together.
func errorKind(err error) string {
	text := fmt.Sprint(err)
	if i := strings.Index(text, "tcp 127.0.0.1"); i >= 0 {
		if j := strings.Index(text[i:], ": "); j >= 0 {
			text = text[:i] + text[i+j:]
		}
	}
	return text
}

func quantile(list []time.Duration, q float64) string {
	if len(list) == 0 {
		return "-"
	}
	return list[min(len(list)-1, int(q*float64(len(list))))].Round(time.Millisecond).String()
}

func over(list []time.Duration, limit time.Duration) float64 {
	if len(list) == 0 {
		return 0
	}
	n := 0
	for _, d := range list {
		if d > limit {
			n++
		}
	}
	return 100 * float64(n) / float64(len(list))
}

// poolHooks is the consumer: small and medium results are consumed at once
// and timed; large ones take size/rate, a slow but live consumer.
type poolHooks struct {
	h         *handler
	m         *poolMetrics
	perMiB    time.Duration
	recording atomic.Bool
	counting  atomic.Bool // link closures before the deadline
}

func (p *poolHooks) OnConnected(context.Context, Connection) error { return nil }
func (p *poolHooks) OnDisconnected(_ context.Context, c Connection) error {
	if !p.counting.Load() {
		return nil
	}
	p.h.mu.Lock()
	peer := p.h.peers[c.Target.ConnectionID]
	p.h.mu.Unlock()
	cause := "other"
	if peer != nil && peer.link != nil {
		switch err := peer.link.Err(); {
		case errors.Is(err, wire.ErrBudgetExhausted):
			cause = "exhausted"
		case errors.Is(err, wire.ErrConsumerStalled):
			cause = "stalled"
		default:
			p.m.mu.Lock()
			p.m.other[errorKind(err)]++
			p.m.mu.Unlock()
		}
	}
	class := "small"
	if strings.HasPrefix(c.DeviceName, "large") {
		class = "large"
	}
	p.m.mu.Lock()
	p.m.closed[class+"/"+cause]++
	p.m.mu.Unlock()
	return nil
}
func (p *poolHooks) OnCallAck(context.Context, Connection, *circusv1.CallAck) error { return nil }
func (p *poolHooks) OnCallStatus(context.Context, Connection, *circusv1.CallStatus) error {
	return nil
}
func (p *poolHooks) OnJobEvent(context.Context, Connection, *circusv1.JobEvent) error { return nil }
func (p *poolHooks) OnJobSnapshot(context.Context, Connection, *circusv1.JobSnapshot) error {
	return nil
}
func (p *poolHooks) OnDeviceResult(_ context.Context, _ Connection, v *circusv1.DeviceResult) error {
	text := v.GetOutput().GetContent()[0].GetText()
	switch v.CallId[0] {
	case 'b':
		time.Sleep(time.Duration(len(text)>>20) * p.perMiB) // the slow consumer
		if p.recording.Load() {
			p.m.mu.Lock()
			p.m.largeBytes += int64(len(text))
			p.m.mu.Unlock()
		}
	default:
		sent, err := strconv.ParseInt(text[:19], 10, 64)
		if err != nil || !p.recording.Load() {
			return nil
		}
		latency := time.Since(time.Unix(0, sent))
		p.m.mu.Lock()
		p.m.latency[v.CallId[0]] = append(p.m.latency[v.CallId[0]], latency)
		p.m.mu.Unlock()
	}
	return nil
}

type poolAuth struct{}

func (poolAuth) Authenticate(_ context.Context, r *http.Request) (Principal, error) {
	name := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if name == "" {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{UserID: "u", DeviceID: name, DeviceName: name, CredentialID: "cred-" + name}, nil
}
func (poolAuth) Recheck(context.Context, Principal) error { return nil }

// poolDevice is a raw daemon-side link that reconnects after a closure.
type poolDevice struct {
	name string
	url  string
}

func (d *poolDevice) dial(ctx context.Context) (*wire.Link, error) {
	conn, _, err := wire.DialWebSocket(ctx, d.url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + d.name}}})
	if err != nil {
		return nil, err
	}
	data, _ := proto.Marshal(&circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Hello{Hello: &circusv1.Hello{BootId: "boot-" + d.name, ProtocolVersion: 1}}})
	if err = conn.WriteMessage(ctx, data); err == nil {
		data, err = conn.ReadMessage(ctx)
	}
	var m proto.Message
	if err == nil {
		m, err = wire.Decode(wire.DaemonDownlink, data, wire.MaxMetadataBytes)
	}
	var opts wire.Options
	if err == nil {
		opts, err = wire.OptionsFromWelcome(m.(*circusv1.DaemonDownlink).GetWelcome(), wire.Options{})
	}
	var l *wire.Link
	if err == nil {
		l, err = wire.New(conn, wire.DaemonSide, opts)
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	go func() { // drain the downlink (heartbeats are internal; GoAway ends)
		for {
			if _, err := l.Receive(context.Background()); err != nil {
				return
			}
		}
	}()
	return l, nil
}

func poolSend(l *wire.Link, id, text string) error {
	m := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: &circusv1.DeviceResult{CallId: id, Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: text}}}}}}}
	class, key, err := wirepolicy.Classify(m)
	if err != nil {
		return err
	}
	return l.Send(context.Background(), m, class, key)
}

func stamped(size int) string {
	return strconv.FormatInt(time.Now().UnixNano(), 10) + strings.Repeat("x", size-19)
}

func runPoolScenario(t *testing.T, devices, large, sendPool, receivePool int, duration time.Duration, rate int, payloads map[int]string) *poolMetrics {
	m := &poolMetrics{latency: make(map[byte][]time.Duration), sent: make(map[byte]int64), closed: make(map[string]int), other: make(map[string]int), daemon: make(map[string]int)}
	hooks := &poolHooks{m: m, perMiB: time.Second / time.Duration(rate)}
	registry, err := NewRegistry(256)
	if err != nil {
		t.Fatal(err)
	}
	b, err := bus.New(bus.Limits{MaxInstances: 1, MaxInFlight: 64, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(Config{InstanceID: "instance", Clock: clock.Real{}, Auth: poolAuth{}, Registry: registry, Bus: b, Hooks: hooks, MaxConnections: 256, SendPoolBytes: sendPool, ReceivePoolBytes: receivePool})
	if err != nil {
		t.Fatal(err)
	}
	hooks.h = gw.(*handler)
	server := httptest.NewServer(gw)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/link/daemon"
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	links := make([]*wire.Link, devices) // each device's last link, closed after the window
	start := time.Now()
	deadline := start.Add(duration)
	hooks.recording.Store(true)
	hooks.counting.Store(true)
	for i := range devices {
		d := &poolDevice{url: url}
		isLarge := i < large
		if isLarge {
			d.name = fmt.Sprintf("large-%02d", i)
		} else {
			d.name = fmt.Sprintf("small-%02d", i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var l *wire.Link
			defer func() { links[i] = l }()
			sizes := []int{8, 16, 32, 64}
			var err error
			for n := 0; time.Now().Before(deadline) && ctx.Err() == nil; n++ {
				if l == nil || l.Err() != nil {
					if l != nil {
						m.mu.Lock()
						m.daemon[errorKind(l.Err())]++
						m.mu.Unlock()
						_ = l.Close()
						time.Sleep(200 * time.Millisecond) // reconnect backoff
					}
					if l, err = d.dial(ctx); err != nil {
						l = nil
						time.Sleep(100 * time.Millisecond)
						continue
					}
				}
				if isLarge {
					if l.Stats().QueuedMessages >= 2 {
						time.Sleep(5 * time.Millisecond)
						continue
					}
					_ = poolSend(l, fmt.Sprintf("b%d", n), payloads[sizes[n%len(sizes)]])
					continue
				}
				for _, class := range poolClasses {
					if n%class.period == 0 && poolSend(l, fmt.Sprintf("%c%d", class.prefix, n), stamped(class.size)) == nil {
						m.mu.Lock()
						m.sent[class.prefix]++
						m.mu.Unlock()
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	hooks.counting.Store(false)
	// Messages still in flight at the deadline count as undelivered after
	// a short grace for the consumer.
	time.Sleep(2 * time.Second)
	hooks.recording.Store(false)
	m.elapsed = time.Since(start)
	m.peak = gw.Stats().Budget.Receive.Peak
	for _, l := range links {
		if l != nil {
			_ = l.Close()
		}
	}
	cancel()
	shutdown, done := context.WithTimeout(context.Background(), 60*time.Second)
	defer done()
	if err := gw.Shutdown(shutdown); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	server.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, list := range m.latency {
		slices.Sort(list)
	}
	return m
}
