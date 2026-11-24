package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/circushome"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

const ledgerUsage = `usage:
  circus ledger open <project> [--dir DIR]
  circus ledger check <changeset>
  circus ledger merge <changeset> [--commit SHA] [--resolved DIR]
  circus ledger discard <changeset> --reason TEXT
  circus ledger list <project> [--state open|merged|discarded]

Changes a project's ledger through a changeset, with the worker credential
from circus join. All forms accept --credentials PATH and --ca-file FILE.

open starts a changeset from the main line and writes its files to DIR,
by default $XDG_STATE_HOME/circus/ledger/cli/<changeset> (under
CIRCUS_HOME/state when that is set). Edit them with any tool; a new
decision is <dir>/D?<letters>-<title>.md with "number: D?<letters>", and
other files may refer to it as D?<letters>. Remove a decision's file only
to move or rename it, together with its new path.

check sends the directory's changes, prints validation problems and
whether the changeset fast-forwards, and writes each conflicting file's
versions that exist to <changeset>.conflicts/<path>.base, .changeset and
.main in that state directory, whatever DIR is. It also warns of references to
temporary numbers that no new decision has; a merge keeps those as they are.

merge checks, then merges and prints the numbers given to new decisions;
merging a merged changeset again prints the same result.
A changeset whose base is behind the main line needs a worker with the
integrator grant, and every conflicting file's final content in
--resolved DIR at its path (or an empty <path>.deleted to remove it).

A call the server refuses because it is busy with other checks or merges
of the user is tried again for up to two minutes.

Exit codes: 0 = done; 1 = error or refusal; 3 = check found problems or
conflicts.`

// ledgerRecord is what open leaves beside the changeset's directory, so
// later commands need only the changeset id.
type ledgerRecord struct {
	ChangesetID     string `json:"changeset_id"`
	ProjectID       string `json:"project_id"`
	Dir             string `json:"dir"`
	BaseCommit      string `json:"base_commit"`
	ControlPlaneURL string `json:"control_plane_url"`
	// Files maps each base file to the SHA-256 of its content.
	Files map[string]string `json:"files"`
}

// ledgerCLI is one invocation's connection and local state.
type ledgerCLI struct {
	api      circusv1connect.LedgerChangesetServiceClient
	url      string
	stateDir string
	out, err io.Writer
	// waited records that a busy server was reported.
	waited bool
}

func ledgerCommand(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("ledger", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() { fmt.Fprintln(errOut, ledgerUsage); fs.PrintDefaults() }
	credentials := fs.String("credentials", "", "worker credential file (default $XDG_CONFIG_HOME/circus/worker.json)")
	ca := fs.String("ca-file", "", "PEM CA bundle (default credential's CA file)")
	dir := fs.String("dir", "", "open: directory for the changeset's files")
	commit := fs.String("commit", "", "merge: the code commit merged with it (40 hex digits)")
	resolved := fs.String("resolved", "", "merge: directory with the final content of conflicting files")
	reason := fs.String("reason", "", "discard: why the changeset is discarded")
	state := fs.String("state", "", "list: open, merged or discarded")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New(ledgerUsage)
	}
	command, arg := pos[0], pos[1]
	switch command {
	case "open", "list":
	case "check", "merge", "discard":
		if !ledger.ValidChangesetID(arg) {
			return fmt.Errorf("invalid changeset id %q", arg)
		}
	default:
		return errors.New(ledgerUsage)
	}
	if *commit != "" && !store.IsLedgerCommitID(*commit) {
		return errors.New("--commit must be a full commit id: 40 lowercase hexadecimal digits")
	}
	if command == "discard" && strings.TrimSpace(*reason) == "" {
		return errors.New("discard requires --reason TEXT")
	}
	var listState circusv1.Changeset_State
	if command == "list" {
		switch *state {
		case "":
		case "open":
			listState = circusv1.Changeset_STATE_OPEN
		case "merged":
			listState = circusv1.Changeset_STATE_MERGED
		case "discarded":
			listState = circusv1.Changeset_STATE_DISCARDED
		default:
			return errors.New("--state must be open, merged or discarded")
		}
	}
	cli, err := newLedgerCLI(*credentials, *ca, out, errOut)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch command {
	case "open":
		return cli.open(ctx, arg, *dir)
	case "check":
		_, err := cli.check(ctx, arg, true)
		return err
	case "merge":
		return cli.merge(ctx, arg, *commit, *resolved)
	case "discard":
		return cli.discard(ctx, arg, *reason)
	default:
		return cli.list(ctx, arg, listState)
	}
}

func newLedgerCLI(credentials, ca string, out, errOut io.Writer) (*ledgerCLI, error) {
	path := credentials
	if path == "" {
		var err error
		if path, err = credfile.Path(credfile.RoleWorker); err != nil {
			return nil, err
		}
	}
	f, err := credfile.Load(path)
	if err != nil {
		return nil, err
	}
	if f.Role != credfile.RoleWorker {
		return nil, fmt.Errorf("%s holds a %s credential; circus ledger needs the worker credential", path, f.Role)
	}
	if ca == "" {
		ca = f.CAFile
	}
	u, err := url.Parse(f.ControlPlaneURL)
	if err != nil || u.Fragment != "" {
		return nil, errors.New("invalid control_plane_url in the credential file")
	}
	if _, err := checkBaseURL(u); err != nil {
		return nil, fmt.Errorf("control plane URL: %w", err)
	}
	base := u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/")
	h, err := httpClient(u.Scheme+"://"+u.Host, ca)
	if err != nil {
		return nil, err
	}
	// A check or merge of a large changeset reads and writes many files.
	h.Timeout = 2 * time.Minute
	stateDir, err := circushome.State("", filepath.Join("ledger", "cli"))
	if err != nil {
		return nil, err
	}
	token := f.Token
	auth := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
	c := &ledgerCLI{url: base, stateDir: stateDir, out: out, err: errOut}
	// The server neither accepts nor sends compressed messages.
	c.api = circusv1connect.NewLedgerChangesetServiceClient(h, base, connect.WithInterceptors(auth, c.retryBusy()),
		connect.WithReadMaxBytes(64<<20))
	return c, nil
}

// busyRetry is how long a call refused for want of a server slot is tried
// again, with backoff from busyBackoff up to busyBackoffMax. Tests
// shorten it.
var busyRetry = 2 * time.Minute

const (
	busyBackoff    = 250 * time.Millisecond
	busyBackoffMax = 5 * time.Second
)

// retryBusy sends a call again while the server refuses it for want of a
// slot, as when another agent of the user checks or merges at the same
// time, with jittered exponential backoff for up to busyRetry. Every call
// of the service is safe to send again: a merge that went through returns
// the same result. It says once that it waits.
func (c *ledgerCLI) retryBusy() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			deadline := time.Now().Add(busyRetry)
			backoff := busyBackoff
			for {
				res, err := next(ctx, req)
				if err == nil || !busy(err) || time.Now().After(deadline) {
					return res, err
				}
				if !c.waited {
					c.waited = true
					fmt.Fprintf(c.err, "the ledger is busy with other checks or merges; retrying for up to %s\n", busyRetry)
				}
				wait := backoff/2 + rand.N(backoff/2+1)
				select {
				case <-ctx.Done():
					return nil, err
				case <-time.After(wait):
				}
				backoff = min(2*backoff, busyBackoffMax)
			}
		}
	}
}

// busy reports a refusal for want of a server slot.
func busy(err error) bool {
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		return false
	}
	for _, d := range ce.Details() {
		if v, e := d.Value(); e == nil {
			if info, ok := v.(*errdetails.ErrorInfo); ok && info.GetReason() == "LEDGER_BUSY" {
				return true
			}
		}
	}
	return false
}

func (c *ledgerCLI) recordPath(id string) string { return filepath.Join(c.stateDir, id+".json") }

func (c *ledgerCLI) readRecord(id string) (ledgerRecord, error) {
	data, err := os.ReadFile(c.recordPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return ledgerRecord{}, fmt.Errorf("no local copy of changeset %s on this machine (%s); open it here with circus ledger open", id, c.recordPath(id))
	}
	if err != nil {
		return ledgerRecord{}, err
	}
	var r ledgerRecord
	if err := json.Unmarshal(data, &r); err != nil || r.ChangesetID != id || !filepath.IsAbs(r.Dir) {
		return ledgerRecord{}, fmt.Errorf("corrupt changeset record %s", c.recordPath(id))
	}
	return r, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err := errors.Join(err, tmp.Chmod(mode), tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// localPath turns a ledger path into a path below dir, refusing any that
// would leave it.
func localPath(dir, p string) (string, error) {
	native := filepath.FromSlash(p)
	if p == "" || strings.Contains(p, "\\") && filepath.Separator == '\\' || !filepath.IsLocal(native) {
		return "", fmt.Errorf("refusing ledger path %q", p)
	}
	return filepath.Join(dir, native), nil
}

func (c *ledgerCLI) open(ctx context.Context, project, dir string) error {
	res, err := c.api.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: project}))
	if err != nil {
		return c.fail(err)
	}
	cs := res.Msg.GetChangeset()
	id := cs.GetChangesetId()
	if !ledger.ValidChangesetID(id) {
		return fmt.Errorf("the server answered an invalid changeset id %q", id)
	}
	written, err := c.writeBase(ctx, cs, dir)
	if err != nil {
		// Without a local copy the changeset is of no use.
		_, discardErr := c.api.DiscardChangeset(context.WithoutCancel(ctx), connect.NewRequest(&circusv1.DiscardChangesetRequest{
			ChangesetId: id, Reason: "circus ledger open could not write the local copy"}))
		return errors.Join(fmt.Errorf("changeset %s: %w", id, err), discardErr)
	}
	fmt.Fprintf(c.out, "changeset: %s\ndirectory: %s\n", id, written)
	return nil
}

// writeBase writes the base's files to dir and the record beside them.
func (c *ledgerCLI) writeBase(ctx context.Context, cs *circusv1.Changeset, dir string) (string, error) {
	id := cs.GetChangesetId()
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		return "", err
	}
	if dir == "" {
		dir = filepath.Join(c.stateDir, id)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return "", fmt.Errorf("%s is not empty", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	record := ledgerRecord{ChangesetID: id, ProjectID: cs.GetProjectId(), Dir: dir, BaseCommit: cs.GetBaseCommit(),
		ControlPlaneURL: c.url, Files: map[string]string{}}
	token := ""
	for {
		page, err := c.api.ReadChangesetBase(ctx, connect.NewRequest(&circusv1.ReadChangesetBaseRequest{ChangesetId: id, PageToken: token}))
		if err != nil {
			return "", c.fail(err)
		}
		for _, f := range page.Msg.GetFiles() {
			target, err := localPath(dir, f.GetPath())
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(target, []byte(f.GetContent()), 0o644); err != nil {
				return "", err
			}
			record.Files[f.GetPath()] = hashContent([]byte(f.GetContent()))
		}
		if token = page.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	return dir, writeFileAtomic(c.recordPath(id), append(data, '\n'), 0o600)
}

func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// collect compares the directory with the base's manifest: new and
// changed files with their content, removed files as deleted.
func collect(r ledgerRecord) ([]*circusv1.FileChange, error) {
	var out []*circusv1.FileChange
	seen := make(map[string]bool, len(r.Files))
	total := 0
	err := filepath.WalkDir(r.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(r.Dir, path)
		if err != nil {
			return err
		}
		p := filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file; the ledger holds only files", path)
		}
		seen[p] = true
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(f, ledger.MaxChangesetBytes+1))
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
		if hashContent(data) == r.Files[p] {
			return nil
		}
		if total += len(data); total > ledger.MaxChangesetBytes {
			return fmt.Errorf("the changed files exceed %d bytes", ledger.MaxChangesetBytes)
		}
		out = append(out, &circusv1.FileChange{Path: p, Change: &circusv1.FileChange_Content{Content: string(data)}})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, p := range slices.Sorted(maps.Keys(r.Files)) {
		if !seen[p] {
			out = append(out, &circusv1.FileChange{Path: p, Change: &circusv1.FileChange_Deleted{Deleted: true}})
		}
	}
	return out, nil
}

// check sends the local changes and prints the outcome. It returns the
// result even when it found problems or conflicts, with a callExit of 3.
// readOmitted reads the contents of conflicts the check left out.
func (c *ledgerCLI) check(ctx context.Context, id string, readOmitted bool) (*circusv1.CheckChangesetResponse, error) {
	r, err := c.readRecord(id)
	if err != nil {
		return nil, err
	}
	changes, err := collect(r)
	if err != nil {
		return nil, err
	}
	res, err := c.api.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id, Changes: changes}))
	if err != nil {
		return nil, c.fail(err)
	}
	m := res.Msg
	fmt.Fprintf(c.out, "changeset: %s\nchanged files: %d\n", id, len(changes))
	for _, p := range m.GetProblems() {
		fmt.Fprintf(c.out, "problem: %s: %s\n", p.GetPath(), p.GetMessage())
	}
	if m.GetFastForward() {
		fmt.Fprintln(c.out, "fast-forward: yes (the base is the main line's head)")
	} else {
		fmt.Fprintf(c.out, "fast-forward: no (the main line is at %s; merging needs the integrator grant)\n", m.GetMainCommit())
	}
	for _, w := range m.GetWarnings() {
		fmt.Fprintf(c.out, "warning: %s: %s\n", w.GetPath(), w.GetMessage())
	}
	if err := c.writeConflicts(ctx, r, m, readOmitted); err != nil {
		return nil, err
	}
	if len(m.GetProblems()) > 0 || len(m.GetConflicts()) > 0 {
		return m, callExit{code: 3}
	}
	return m, nil
}

// conflictsDir is where check writes the conflicts' versions: in the state
// directory, beside the record, so it is always the CLI's own.
func (c *ledgerCLI) conflictsDir(id string) string { return filepath.Join(c.stateDir, id+".conflicts") }

// writeConflicts writes each conflict's versions that exist to
// <id>.conflicts/<path>.base, .changeset and .main. The check leaves out
// contents beyond its response budget; unless readOmitted is false, those
// are read in pages with ReadChangesetConflicts. A failure there leaves
// only their versions unwritten, with the reason.
func (c *ledgerCLI) writeConflicts(ctx context.Context, r ledgerRecord, m *circusv1.CheckChangesetResponse, readOmitted bool) error {
	dir := c.conflictsDir(r.ChangesetID)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	var full map[string]*circusv1.LedgerConflict
	unread := "not read: the merge sends the --resolved files"
	if readOmitted && slices.ContainsFunc(m.GetConflicts(), (*circusv1.LedgerConflict).GetContentOmitted) {
		full, unread = c.readConflicts(ctx, r.ChangesetID, m.GetMainCommit())
	}
	for _, cf := range m.GetConflicts() {
		p := cf.GetPath()
		fmt.Fprintf(c.out, "conflict: %s\n", p)
		if cf.GetContentOmitted() {
			if full[p] == nil {
				fmt.Fprintf(c.out, "  versions %s\n", unread)
				continue
			}
			cf = full[p]
		}
		for _, v := range []struct {
			name    string
			exists  bool
			content string
		}{{"base", cf.GetBaseExists(), cf.GetBase()}, {"changeset", cf.GetChangesetExists(), cf.GetChangeset()}, {"main", cf.GetMainExists(), cf.GetMain()}} {
			if !v.exists {
				fmt.Fprintf(c.out, "  %s: absent\n", v.name)
				continue
			}
			target, err := localPath(dir, p+"."+v.name)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(target, []byte(v.content), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(c.out, "  %s: %s\n", v.name, target)
		}
	}
	return nil
}

// readConflicts reads every conflict of the changeset's last check with
// its contents, against mainCommit. On failure it returns why, for the
// versions it could not write.
func (c *ledgerCLI) readConflicts(ctx context.Context, id, mainCommit string) (map[string]*circusv1.LedgerConflict, string) {
	out := make(map[string]*circusv1.LedgerConflict)
	for token := ""; ; {
		page, err := c.api.ReadChangesetConflicts(ctx, connect.NewRequest(&circusv1.ReadChangesetConflictsRequest{
			ChangesetId: id, MainCommit: mainCommit, PageToken: token}))
		if err != nil {
			// Pages already read may come from an earlier state: none is
			// written.
			switch precondition(err) {
			case ledger.ViolationMainMoved:
				return nil, "not written: the main line moved after the check; check again"
			case ledger.ViolationChangesetChanged:
				return nil, "not written: the changeset was checked again meanwhile; check again"
			}
			return nil, "not written: they could not be read: " + connect.CodeOf(err).String() + ": " + errorMessage(err)
		}
		for _, cf := range page.Msg.GetConflicts() {
			out[cf.GetPath()] = cf
		}
		if token = page.Msg.GetNextPageToken(); token == "" {
			return out, ""
		}
	}
}

// precondition returns the type of a FAILED_PRECONDITION refusal's first
// violation, or "".
func precondition(err error) string {
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		return ""
	}
	for _, d := range ce.Details() {
		if v, e := d.Value(); e == nil {
			if f, ok := v.(*errdetails.PreconditionFailure); ok && len(f.GetViolations()) > 0 {
				return f.GetViolations()[0].GetType()
			}
		}
	}
	return ""
}

func errorMessage(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

func (c *ledgerCLI) merge(ctx context.Context, id, code, resolvedDir string) error {
	// A merged changeset merges again to the same result, which reports
	// the numbers once more, as after a lost response.
	current, err := c.api.GetChangeset(ctx, connect.NewRequest(&circusv1.GetChangesetRequest{ChangesetId: id}))
	if err != nil {
		return c.fail(err)
	}
	if current.Msg.GetChangeset().GetState() == circusv1.Changeset_STATE_MERGED {
		return c.sendMerge(ctx, &circusv1.MergeChangesetRequest{ChangesetId: id, CodeCommit: code})
	}
	// The files in --resolved are what the merge sends: it needs no
	// contents the check left out.
	checked, err := c.check(ctx, id, resolvedDir == "")
	var exit callExit
	if err != nil && !(errors.As(err, &exit) && exit.code == 3 && len(checked.GetProblems()) == 0) {
		return err
	}
	req := &circusv1.MergeChangesetRequest{ChangesetId: id, CodeCommit: code}
	if resolvedDir != "" && !checked.GetFastForward() {
		for _, cf := range checked.GetConflicts() {
			final, err := localPath(resolvedDir, cf.GetPath())
			if err != nil {
				return err
			}
			data, err := os.ReadFile(final)
			if err == nil {
				req.Resolved = append(req.Resolved, &circusv1.FileChange{Path: cf.GetPath(), Change: &circusv1.FileChange_Content{Content: string(data)}})
				continue
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if _, err := os.Stat(final + ".deleted"); err == nil {
				req.Resolved = append(req.Resolved, &circusv1.FileChange{Path: cf.GetPath(), Change: &circusv1.FileChange_Deleted{Deleted: true}})
			}
		}
	}
	return c.sendMerge(ctx, req)
}

func (c *ledgerCLI) sendMerge(ctx context.Context, req *circusv1.MergeChangesetRequest) error {
	res, err := c.api.MergeChangeset(ctx, connect.NewRequest(req))
	if err != nil {
		return c.fail(err)
	}
	cs := res.Msg.GetChangeset()
	fmt.Fprintf(c.out, "merged: %s\n", cs.GetMergedCommit())
	numbers := res.Msg.GetNumbers()
	for _, temporary := range slices.SortedFunc(maps.Keys(numbers), func(a, b string) int { return cmp.Compare(numbers[a], numbers[b]) }) {
		fmt.Fprintf(c.out, "%s -> D%d\n", temporary, numbers[temporary])
	}
	return nil
}

func (c *ledgerCLI) discard(ctx context.Context, id, reason string) error {
	res, err := c.api.DiscardChangeset(ctx, connect.NewRequest(&circusv1.DiscardChangesetRequest{ChangesetId: id, Reason: reason}))
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.out, "discarded: %s\n", res.Msg.GetChangeset().GetChangesetId())
	return nil
}

func (c *ledgerCLI) list(ctx context.Context, project string, state circusv1.Changeset_State) error {
	token := ""
	for {
		res, err := c.api.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: project, State: state, PageSize: 100, PageToken: token}))
		if err != nil {
			return c.fail(err)
		}
		for _, cs := range res.Msg.GetChangesets() {
			line := fmt.Sprintf("%s\t%s\t%s\tbase %s\topener %s", cs.GetChangesetId(),
				strings.ToLower(strings.TrimPrefix(cs.GetState().String(), "STATE_")),
				time.UnixMilli(cs.GetCreatedAtUnixMs()).UTC().Format(time.RFC3339), short(cs.GetBaseCommit()), cs.GetOpenerWorkerId())
			switch cs.GetState() {
			case circusv1.Changeset_STATE_MERGED:
				line += "\tmerged " + short(cs.GetMergedCommit())
				if cs.GetCodeCommit() != "" {
					line += "\tcode " + short(cs.GetCodeCommit())
				}
			case circusv1.Changeset_STATE_DISCARDED:
				line += "\treason " + strings.ReplaceAll(cs.GetDiscardReason(), "\n", " ")
			}
			fmt.Fprintln(c.out, line)
		}
		if token = res.Msg.GetNextPageToken(); token == "" {
			return nil
		}
	}
}

func short(commit string) string {
	if commit == "" {
		return "-"
	}
	return commit[:min(len(commit), 12)]
}

// fail explains a refusal: what the integrator grant would allow, the
// problems and conflicts that stop a merge, or the field that is wrong.
func (c *ledgerCLI) fail(err error) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return fmt.Errorf("%w%s", err, tlsHint(err))
	}
	for _, d := range ce.Details() {
		v, e := d.Value()
		if e != nil {
			continue
		}
		switch v := v.(type) {
		case *errdetails.ErrorInfo:
			if v.GetReason() == "INTEGRATOR_REQUIRED" {
				return fmt.Errorf("%s\nThis worker needs the integrator grant: turn it on for this worker on the Web /devices page", ce.Message())
			}
		case *errdetails.PreconditionFailure:
			for _, violation := range v.GetViolations() {
				switch violation.GetType() {
				case ledger.ViolationConflict:
					fmt.Fprintf(c.err, "unresolved conflict: %s\n", violation.GetSubject())
				case ledger.ViolationState:
					fmt.Fprintf(c.err, "%s\n", violation.GetDescription())
				default:
					fmt.Fprintf(c.err, "problem: %s: %s\n", violation.GetSubject(), violation.GetDescription())
				}
			}
		case *errdetails.BadRequest:
			for _, f := range v.GetFieldViolations() {
				fmt.Fprintf(c.err, "invalid %s: %s\n", f.GetField(), f.GetDescription())
			}
		}
	}
	if ce.Code() == connect.CodeUnauthenticated {
		return fmt.Errorf("%s: the worker credential was not accepted; run circus join again if it was revoked", ce.Message())
	}
	return errors.New(ce.Message())
}
