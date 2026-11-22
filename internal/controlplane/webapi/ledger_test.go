package webapi_test

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
)

func createLedgerProject(t *testing.T, owner *client) string {
	t.Helper()
	result, err := owner.projects.CreateProject(t.Context(), connect.NewRequest(&circusv1.CreateProjectRequest{Name: "Ledger"}))
	if err != nil {
		t.Fatal(err)
	}
	return result.Msg.GetProject().GetProjectId()
}

func ledgerCommit(t *testing.T, h *harness, project string, decisions []ledger.Decision, message, code string) string {
	t.Helper()
	head, err := h.ledger.CommitMain(t.Context(), project, decisions, ledger.CommitOptions{Author: "worker", Message: message, CodeCommit: code})
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func TestLedgerService(t *testing.T) {
	h, owner := newHarness(t)
	ctx := t.Context()
	id := createLedgerProject(t, owner)
	api := circusv1connect.NewLedgerServiceClient(owner.http, h.server.URL, connect.WithProtoJSON())
	list := &circusv1.GetLedgerRequest{ProjectId: id}
	read := &circusv1.GetDecisionRequest{ProjectId: id}
	history := &circusv1.ListDecisionHistoryRequest{ProjectId: id}

	for _, tc := range []struct {
		client *client
		origin string
		code   connect.Code
	}{
		{h.browser(t), "", connect.CodeUnauthenticated},
		{h.signIn(t, "ledger-reader", "ledger-reader@example.com"), h.server.URL, connect.CodeNotFound},
		{owner, "https://another.example", connect.CodePermissionDenied},
	} {
		testAPI := circusv1connect.NewLedgerServiceClient(tc.client.http, h.server.URL)
		listReq, readReq, historyReq := connect.NewRequest(list), connect.NewRequest(read), connect.NewRequest(history)
		for _, req := range []connect.AnyRequest{listReq, readReq, historyReq} {
			if tc.origin != "" {
				req.Header().Set("Origin", tc.origin)
			}
		}
		_, err := testAPI.GetLedger(ctx, listReq)
		wantCode(t, err, tc.code)
		_, err = testAPI.GetDecision(ctx, readReq)
		wantCode(t, err, tc.code)
		_, err = testAPI.ListDecisionHistory(ctx, historyReq)
		wantCode(t, err, tc.code)
	}
	empty, err := api.GetLedger(ctx, connect.NewRequest(list))
	if err != nil || empty.Msg.HeadCommit != "" || len(empty.Msg.Entries) != 0 || empty.Msg.NextNumber != 0 {
		t.Fatalf("empty ledger = %v, %v", empty, err)
	}
	_, err = api.GetDecision(ctx, connect.NewRequest(read))
	wantCode(t, err, connect.CodeNotFound)
	emptyHistory, err := api.ListDecisionHistory(ctx, connect.NewRequest(history))
	if err != nil || len(emptyHistory.Msg.Versions) != 0 || emptyHistory.Msg.NextPageToken != "" {
		t.Fatalf("empty history = %v, %v", emptyHistory, err)
	}

	first := ledger.Decision{Number: 0, Dir: "nested", Title: "Original", Body: []byte("first\r\n\r\n正文\n")}
	other := ledger.Decision{Number: 7, Title: "Root"}
	code := strings.Repeat("a", 40)
	old := ledgerCommit(t, h, id, []ledger.Decision{first, other}, "Seed\n\nDetails", code)
	d := first
	d.Title = "Moved: 中文"
	d.Dir = "alpha"
	d.Body = []byte("new\x00 body")
	updated := ledgerCommit(t, h, id, []ledger.Decision{d, other}, "Update", "")
	listed, err := api.GetLedger(ctx, connect.NewRequest(list))
	if err != nil {
		t.Fatal(err)
	}
	gotIndex := listed.Msg
	if gotIndex.HeadCommit != updated || gotIndex.NextNumber != 8 || len(gotIndex.Entries) != 2 ||
		gotIndex.Entries[0].Number != 7 || gotIndex.Entries[1].Number != 0 || gotIndex.Entries[1].Path != "alpha/D0-Moved-中文.md" {
		t.Fatalf("GetLedger = %v", gotIndex)
	}
	latest, err := api.GetDecision(ctx, connect.NewRequest(read))
	if err != nil {
		t.Fatal(err)
	}
	if got := latest.Msg.GetDecision(); got.Commit != updated || got.Title != d.Title || got.Dir != d.Dir ||
		got.Body != string(d.Body) || got.CommittedAtUnixMs != h.clock.Load() {
		t.Fatalf("latest = %v", got)
	}
	prior, err := api.GetDecision(ctx, connect.NewRequest(&circusv1.GetDecisionRequest{ProjectId: id, Commit: old}))
	if err != nil || prior.Msg.GetDecision().Body != string(first.Body) || prior.Msg.GetDecision().Dir != first.Dir {
		t.Fatalf("old decision = %v, %v", prior, err)
	}
	_, err = api.GetDecision(ctx, connect.NewRequest(&circusv1.GetDecisionRequest{ProjectId: id, Commit: strings.Repeat("b", 40)}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = api.GetDecision(ctx, connect.NewRequest(&circusv1.GetDecisionRequest{ProjectId: id, Number: 1}))
	wantCode(t, err, connect.CodeNotFound)

	history.PageSize = 1
	page, err := api.ListDecisionHistory(ctx, connect.NewRequest(history))
	if err != nil {
		t.Fatal(err)
	}
	token := page.Msg.GetNextPageToken()
	v := page.Msg.GetVersions()
	if len(v) != 1 || token == "" || v[0].Commit != updated || v[0].Added || !v[0].Moved ||
		!v[0].Retitled || !v[0].ContentChanged || v[0].CodeCommit != "" || v[0].Summary != "Update" || v[0].Author != "worker" {
		t.Fatalf("first page = %v", page.Msg)
	}
	d.Body = []byte("one more change")
	ledgerCommit(t, h, id, []ledger.Decision{d, other}, "New head between pages", "")
	history.PageToken = token
	page, err = api.ListDecisionHistory(ctx, connect.NewRequest(history))
	if err != nil {
		t.Fatal(err)
	}
	v = page.Msg.GetVersions()
	if len(v) != 1 || page.Msg.NextPageToken != "" || v[0].Commit != old || !v[0].Added || v[0].Moved ||
		v[0].Retitled || v[0].ContentChanged || v[0].CodeCommit != code || v[0].Summary != "Seed" {
		t.Fatalf("last page = %v", page.Msg)
	}

	for _, bad := range []string{"", "a/b", strings.Repeat("x", store.MaxIDBytes+1)} {
		_, err := api.GetLedger(ctx, connect.NewRequest(&circusv1.GetLedgerRequest{ProjectId: bad}))
		wantField(t, err, connect.CodeInvalidArgument, "project_id")
	}
	for _, bad := range []string{strings.Repeat("A", 40), strings.Repeat("z", 40), "short"} {
		_, err := api.GetDecision(ctx, connect.NewRequest(&circusv1.GetDecisionRequest{ProjectId: id, Commit: bad}))
		wantField(t, err, connect.CodeInvalidArgument, "commit")
	}
	_, err = api.GetDecision(ctx, connect.NewRequest(&circusv1.GetDecisionRequest{ProjectId: id, Number: math.MaxUint32}))
	wantField(t, err, connect.CodeInvalidArgument, "number")
	_, err = api.ListDecisionHistory(ctx, connect.NewRequest(&circusv1.ListDecisionHistoryRequest{ProjectId: id, Number: math.MaxUint32}))
	wantField(t, err, connect.CodeInvalidArgument, "number")
	_, err = api.ListDecisionHistory(ctx, connect.NewRequest(&circusv1.ListDecisionHistoryRequest{ProjectId: id, PageSize: 101}))
	wantField(t, err, connect.CodeInvalidArgument, "page_size")
	_, err = api.ListDecisionHistory(ctx, connect.NewRequest(&circusv1.ListDecisionHistoryRequest{ProjectId: id, PageToken: "invalid"}))
	wantField(t, err, connect.CodeInvalidArgument, "page_token")
	_, err = api.ListDecisionHistory(ctx, connect.NewRequest(&circusv1.ListDecisionHistoryRequest{ProjectId: id, Number: 7, PageToken: token}))
	wantField(t, err, connect.CodeInvalidArgument, "page_token")
	_, err = api.ListDecisionHistory(ctx, connect.NewRequest(&circusv1.ListDecisionHistoryRequest{ProjectId: id, PageToken: strings.Repeat("a", 5000)}))
	wantCode(t, err, connect.CodeResourceExhausted)

	dir := filepath.Join(h.dir, "ledger", id+".git")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: id})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted repository stat = %v", err)
	}
	_, err = api.GetLedger(ctx, connect.NewRequest(list))
	wantCode(t, err, connect.CodeNotFound)
}

func TestLedgerAtBound(t *testing.T) {
	h, owner := newHarness(t)
	id := createLedgerProject(t, owner)
	title := strings.Repeat("界", 60) + strings.Repeat(`"`, store.MaxLedgerTitleBytes-180)
	dir := strings.Repeat(strings.Repeat("x", 64)+"/", 3) + strings.Repeat("y", 61)
	decisions := make([]ledger.Decision, store.MaxLedgerDecisions)
	for i := range decisions {
		decisions[i] = ledger.Decision{Number: uint32(store.MaxLedgerNumber - store.MaxLedgerDecisions + 1 + i), Title: title, Dir: dir}
	}
	header, err := ledger.Render(decisions[0])
	if err != nil {
		t.Fatal(err)
	}
	decisions[0].Body = bytes.Repeat([]byte{1}, ledger.MaxDecisionFileBytes-len(header))
	commit := ledgerCommit(t, h, id, decisions, "Full ledger", "")

	for _, tc := range []struct {
		name    string
		options []connect.ClientOption
	}{
		{"binary", nil}, {"json", []connect.ClientOption{connect.WithProtoJSON()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := circusv1connect.NewLedgerServiceClient(owner.http, h.server.URL, tc.options...)
			out, err := api.GetLedger(t.Context(), connect.NewRequest(&circusv1.GetLedgerRequest{ProjectId: id}))
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Msg.Entries) != len(decisions) || out.Msg.HeadCommit != commit || out.Msg.NextNumber != math.MaxUint32 ||
				out.Msg.Entries[0].Title != title || out.Msg.Entries[0].Dir != dir {
				t.Fatalf("ledger at bound = %d entries, head %s, next %d", len(out.Msg.Entries), out.Msg.HeadCommit, out.Msg.NextNumber)
			}
			value, err := api.GetDecision(t.Context(), connect.NewRequest(&circusv1.GetDecisionRequest{ProjectId: id, Number: decisions[0].Number}))
			if err != nil {
				t.Fatal(err)
			}
			if value.Msg.GetDecision().Body != string(decisions[0].Body) {
				t.Fatal("largest body changed")
			}
		})
	}
}

func TestLedgerHistoryAtBound(t *testing.T) {
	h, owner := newHarness(t)
	id := createLedgerProject(t, owner)
	title := strings.Repeat(`"`, store.MaxLedgerTitleBytes)
	dir := strings.Repeat(strings.Repeat("x", 64)+"/", 3) + strings.Repeat("y", 61)
	options := ledger.CommitOptions{
		Author: strings.Repeat("a", store.MaxLedgerAuthorBytes), Message: strings.Repeat("\x01", store.MaxLedgerSummaryBytes),
		CodeCommit: strings.Repeat("a", 40),
	}
	for i := range store.MaxLedgerHistoryPage + 1 {
		_, err := h.ledger.CommitMain(t.Context(), id, []ledger.Decision{{Title: title, Dir: dir, Body: []byte{byte(i)}}}, options)
		if err != nil {
			t.Fatal(err)
		}
	}
	api := circusv1connect.NewLedgerServiceClient(owner.http, h.server.URL, connect.WithProtoJSON())
	for _, tc := range []struct {
		size  uint32
		count int
	}{{0, 50}, {100, 100}} {
		first, err := api.ListDecisionHistory(t.Context(), connect.NewRequest(&circusv1.ListDecisionHistoryRequest{ProjectId: id, PageSize: tc.size}))
		if err != nil {
			t.Fatal(err)
		}
		if len(first.Msg.Versions) != tc.count || first.Msg.NextPageToken == "" || first.Msg.Versions[0].Summary != options.Message {
			t.Fatalf("page size %d returned %d versions", tc.size, len(first.Msg.Versions))
		}
		second, err := api.ListDecisionHistory(t.Context(), connect.NewRequest(&circusv1.ListDecisionHistoryRequest{ProjectId: id, PageSize: 100, PageToken: first.Msg.NextPageToken}))
		if err != nil {
			t.Fatal(err)
		}
		if want := store.MaxLedgerHistoryPage + 1 - tc.count; len(second.Msg.Versions) != want || second.Msg.NextPageToken != "" ||
			!second.Msg.Versions[len(second.Msg.Versions)-1].Added {
			t.Fatalf("second page returned %d versions, wanted %d", len(second.Msg.Versions), want)
		}
	}
}

func TestLedgerSlowBodiesDoNotReserveSlots(t *testing.T) {
	h, owner := newHarness(t)
	id := createLedgerProject(t, owner)
	api := circusv1connect.NewLedgerServiceClient(owner.http, h.server.URL)
	url, err := url.Parse(h.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, signedIn := range []bool{false, true} {
		t.Run(fmt.Sprintf("signed-in=%t", signedIn), func(t *testing.T) {
			for range 4 {
				c, err := new(net.Dialer).DialContext(t.Context(), "tcp", url.Host)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				req := &http.Request{Header: make(http.Header)}
				if signedIn {
					for _, cookie := range owner.http.Jar.Cookies(url) {
						req.AddCookie(cookie)
					}
				}
				if _, err := fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\nExpect: 100-continue\r\n",
					circusv1connect.LedgerServiceGetLedgerProcedure, url.Host); err != nil {
					t.Fatal(err)
				}
				if err := req.Header.Write(c); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(c, "\r\n"); err != nil {
					t.Fatal(err)
				}
				// The server sends 100 only once it tries to read the
				// request body, so all four requests are in flight.
				response, err := http.ReadResponse(bufio.NewReader(c), nil)
				if err != nil || response.StatusCode != http.StatusContinue {
					t.Fatalf("waiting for body: %v, %v", response, err)
				}
				_ = response.Body.Close()
				if _, err := io.WriteString(c, "{"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := api.GetLedger(t.Context(), connect.NewRequest(&circusv1.GetLedgerRequest{ProjectId: id})); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A signed-in client that stops reading large responses holds at most its
// own share of the ledger slots, so other users are still served.
func TestLedgerSlowReaderLeavesSlotsForOthers(t *testing.T) {
	h, owner := newHarness(t)
	id := createLedgerProject(t, owner)
	// Titles and directories that escape heavily make the listing large.
	title := strings.Repeat("\\\"", 255)
	dir := strings.Repeat("d", 64) + "/" + strings.Repeat("e", 64) + "/" + strings.Repeat("f", 64) + "/" + strings.Repeat("g", 61)
	ds := make([]ledger.Decision, store.MaxLedgerDecisions)
	for i := range ds {
		ds[i] = ledger.Decision{Number: uint32(i), Title: title + string(rune('a'+i%26)), Dir: dir}
	}
	ledgerCommit(t, h, id, ds, "Large", "")
	for range 4 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+circusv1connect.LedgerServiceGetLedgerProcedure,
			strings.NewReader(`{"projectId":"`+id+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", h.server.URL)
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := owner.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		// Leave the body unread until the test ends.
		t.Cleanup(func() { _ = resp.Body.Close() })
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("listing status %d", resp.StatusCode)
		}
	}
	other := h.signIn(t, "other", "other@example.com")
	otherID := createLedgerProject(t, other)
	api := circusv1connect.NewLedgerServiceClient(other.http, h.server.URL)
	if _, err := api.GetLedger(t.Context(), connect.NewRequest(&circusv1.GetLedgerRequest{ProjectId: otherID})); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerBrokenProjectReturnsInternal(t *testing.T) {
	h, owner := newHarness(t)
	id := createLedgerProject(t, owner)
	head := ledgerCommit(t, h, id, []ledger.Decision{{Title: "Original"}}, "Seed", "")
	api := circusv1connect.NewLedgerServiceClient(owner.http, h.server.URL)
	file := filepath.Join(h.dir, "ledger", id+".git", "objects", head[:2], head[2:])
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(file, 0); err != nil {
		t.Fatal(err)
	}
	_, err := api.GetDecision(t.Context(), connect.NewRequest(&circusv1.GetDecisionRequest{ProjectId: id}))
	wantCode(t, err, connect.CodeInternal)
	if err := h.ledger.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = api.GetLedger(t.Context(), connect.NewRequest(&circusv1.GetLedgerRequest{ProjectId: id}))
	wantCode(t, err, connect.CodeInternal)
	_, err = api.ListDecisionHistory(t.Context(), connect.NewRequest(&circusv1.ListDecisionHistoryRequest{ProjectId: id}))
	wantCode(t, err, connect.CodeInternal)
}
