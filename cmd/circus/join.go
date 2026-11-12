package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"golang.org/x/term"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/machineid"
)

const joinUsage = `usage: circus join [-|<link>] [--role daemon|worker|both] [--name NAME] [--ca-file FILE] [--force]

The link is the one printed by "circus server enroll-token create":
https://host:port/join#<token> enrolls one role (daemon unless --role worker),
https://host:port/join#<daemonToken>.<workerToken> enrolls both roles; there
--role daemon or --role worker enrolls only that one.

Without a link argument, or with "-", the link is read from stdin: pasted at a
prompt without echo on a terminal, or piped in. Prefer that on a shared
machine: a link given as an argument is visible to other local users in the
process list while join runs, and stays in the shell history. When you do
pass it as an argument, quote it: the shell must not cut it at '#'.`

// maxLinkBytes bounds a join link read from stdin.
const maxLinkBytes = 4096

// readLink reads the join link from in. On a terminal it prompts on errOut
// and reads without echo; otherwise it reads the first line.
func readLink(in io.Reader, errOut io.Writer) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(errOut, "Paste the join link (input is hidden): ")
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(errOut)
		if err != nil {
			return "", fmt.Errorf("read join link: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	line, err := bufio.NewReader(io.LimitReader(in, maxLinkBytes)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read join link: %w", err)
	}
	if line = strings.TrimSpace(line); line == "" {
		return "", errors.New("no join link on stdin")
	}
	return line, nil
}

// joinLink is a parsed join link: one token for one role, or a daemon and a
// worker token (enroll-token create --role both).
type joinLink struct {
	base   string
	single string // the token of a one-token link
	daemon string // the tokens of a two-token link
	worker string
}

// enrollment is one role to enroll and the token for it.
type enrollment struct {
	role  credfile.Role
	token string
}

// plan picks the roles to enroll. On a one-token link the role is --role
// (default daemon); on a two-token link --role, when given, selects one.
func (l joinLink) plan(roleFlag string) ([]enrollment, error) {
	if l.single != "" {
		switch roleFlag {
		case "", "daemon":
			return []enrollment{{credfile.RoleDaemon, l.single}}, nil
		case "worker":
			return []enrollment{{credfile.RoleWorker, l.single}}, nil
		case "both":
			return nil, errors.New("this link holds one token for one role; --role both needs a link made with `enroll-token create --role both`")
		}
	} else {
		switch roleFlag {
		case "", "both":
			return []enrollment{{credfile.RoleDaemon, l.daemon}, {credfile.RoleWorker, l.worker}}, nil
		case "daemon":
			return []enrollment{{credfile.RoleDaemon, l.daemon}}, nil
		case "worker":
			return []enrollment{{credfile.RoleWorker, l.worker}}, nil
		}
	}
	return nil, fmt.Errorf("unknown role %q: use daemon, worker or both", roleFlag)
}

func joinCommand(args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("join", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() { fmt.Fprintln(errOut, joinUsage); fs.PrintDefaults() }
	roleFlag := fs.String("role", "", "role to enroll: daemon, worker or both (default: what the link holds; daemon for a one-token link)")
	name := fs.String("name", "", "identity name (default: this host's name)")
	caFile := fs.String("ca-file", "", "PEM CA bundle that verifies the control plane instead of the system roots")
	force := fs.Bool("force", false, "replace existing credential files")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	var raw string
	switch {
	case len(pos) > 1:
		return errors.New(joinUsage)
	case len(pos) == 0 || pos[0] == "-":
		if raw, err = readLink(in, errOut); err != nil {
			return err
		}
	default:
		raw = pos[0]
	}
	link, err := parseJoinLink(raw)
	if err != nil {
		return err
	}
	plan, err := link.plan(*roleFlag)
	if err != nil {
		return err
	}
	paths := map[credfile.Role]string{}
	for _, e := range plan {
		path, err := credfile.Path(e.role)
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); err == nil && !*force {
			return fmt.Errorf("%s already exists: this machine has joined as %s. To join again pass --force; "+
				"to keep the same name, first have an administrator run `circus server %s revoke <name>` (it frees the name), "+
				"otherwise choose another --name", path, e.role, adminNoun(e.role))
		}
		paths[e.role] = path
	}
	if *caFile != "" {
		if *caFile, err = filepath.Abs(*caFile); err != nil {
			return err
		}
	}
	client, err := httpClient(link.base, *caFile)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	if *name == "" {
		*name = auth.DefaultName(hostname)
	}
	if err := auth.ValidateName(*name); err != nil {
		return err
	}
	machine, err := machineid.Get()
	if err != nil {
		return err
	}

	// Each role is enrolled on its own (enroll.proto: no cross-role
	// atomicity). A role that succeeded keeps its credential file even when
	// a later one fails.
	enroll := circusv1connect.NewEnrollServiceClient(client, link.base)
	var joined []credfile.Role
	for _, e := range plan {
		protoRole := circusv1.Role_ROLE_DAEMON
		if e.role == credfile.RoleWorker {
			protoRole = circusv1.Role_ROLE_WORKER
		}
		resp, err := enroll.Enroll(context.Background(), connect.NewRequest(&circusv1.EnrollRequest{
			OneTimeToken: e.token, Role: protoRole, Name: *name, Hostname: hostname, MachineId: machine,
			Os: runtime.GOOS, Arch: runtime.GOARCH, ClientVersion: version,
		}))
		if err != nil {
			taken := connect.CodeOf(err) == connect.CodeAlreadyExists
			err = joinError(err, e.role)
			if taken {
				// For the MACHINE column of `circus server device list`.
				err = fmt.Errorf("%w\nThis machine's id begins with %s.", err, machine[:min(12, len(machine))])
			}
			return partial(joined, e.role, err)
		}
		f := credfile.File{
			ControlPlaneURL: link.base, Role: e.role, PrincipalID: resp.Msg.GetPrincipalId(),
			Token: resp.Msg.GetLongTermToken(), CAFile: *caFile,
		}
		if err := credfile.Save(paths[e.role], f); err != nil {
			return partial(joined, e.role, fmt.Errorf("enrolled as %s %q (%s), but saving the credential failed: %w; "+
				"have an administrator run `circus server %s revoke %s` (it frees the name), then join again with a new link",
				e.role, *name, f.PrincipalID, err, adminNoun(e.role), *name))
		}
		if u := resp.Msg.GetControlPlaneUrl(); u != "" && u != link.base {
			fmt.Fprintf(errOut, "note: the control plane calls itself %s; this machine keeps using %s from the link\n", u, link.base)
		}
		fmt.Fprintf(out, "Joined as %s %q (%s).\nCredential saved to %s.\n", e.role, *name, f.PrincipalID, paths[e.role])
		joined = append(joined, e.role)
	}
	if slices.Contains(joined, credfile.RoleDaemon) {
		fmt.Fprintln(out, "Start it with: circus daemon")
	}
	return nil
}

// adminNoun names a role as the server CLI does: the daemon role is a
// "device" (`circus server device revoke`).
func adminNoun(r credfile.Role) string {
	if r == credfile.RoleDaemon {
		return "device"
	}
	return string(r)
}

// partial reports the failure of role's enrollment, naming the roles that
// joined before it and keep their credentials.
func partial(joined []credfile.Role, role credfile.Role, err error) error {
	if len(joined) == 0 {
		return err
	}
	return fmt.Errorf("joined as %v, but enrolling the %s role failed (the %v credential is kept): %w", joined, role, joined, err)
}

// parseJoinLink parses https://host:port/join#<token> or
// .../join#<daemonToken>.<workerToken>. http:// is accepted for loopback
// hosts only.
func parseJoinLink(raw string) (joinLink, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return joinLink{}, fmt.Errorf("join link: %w", err)
	}
	fragment := u.Fragment
	u.Fragment, u.RawFragment = "", ""
	if strings.TrimSuffix(u.Path, "/") != "/join" {
		return joinLink{}, errors.New("join link must look like https://host:port/join#<token>")
	}
	u.Path, u.RawPath = "", ""
	base, err := checkBaseURL(u)
	if err != nil {
		return joinLink{}, fmt.Errorf("join link: %w", err)
	}
	if fragment == "" {
		return joinLink{}, errors.New("join link has no token after '#'; quote the link so the shell keeps it")
	}
	tokens := strings.Split(fragment, ".")
	for _, t := range tokens {
		if !auth.WellFormed(t) {
			return joinLink{}, errors.New("join link token is malformed; copy the whole link again")
		}
	}
	switch len(tokens) {
	case 1:
		return joinLink{base: base, single: tokens[0]}, nil
	case 2:
		return joinLink{base: base, daemon: tokens[0], worker: tokens[1]}, nil
	}
	return joinLink{}, errors.New("join link holds more than two tokens; copy the whole link again")
}

func joinError(err error, role credfile.Role) error {
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		return fmt.Errorf("the control plane refused the link: it is unknown, already used, expired, or not issued for role %s; ask for a new link", role)
	case connect.CodeAlreadyExists, connect.CodeInvalidArgument:
		var ce *connect.Error
		if errors.As(err, &ce) {
			return errors.New(ce.Message())
		}
	}
	return fmt.Errorf("enroll: %w%s", err, tlsHint(err))
}
