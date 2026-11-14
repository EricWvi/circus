package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"syscall"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/workerclient"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
)

const callUsage = `usage:
  circus call --device NAME [--call-id ID] [--session ID] [--cwd DIR] [--wait] bash <command>
  circus call --device NAME [--call-id ID] [--session ID] [--cwd DIR] [--wait] read <path> [--offset N --limit N]
  circus call --device NAME [--call-id ID] [--session ID] [--cwd DIR] [--wait] write <path>  (content from stdin)
  circus call --device NAME [--call-id ID] [--session ID] [--cwd DIR] [--wait] edit <path> --old S --new S [--replace-all]
  circus call query <call-id>
  circus call cancel <call-id>

Uses worker credentials from circus join. All forms accept --credentials PATH
and --ca-file FILE. Updates are JSON lines on stderr; tool text goes to stdout.
Cwd is empty unless --cwd is given: relative file paths then use the daemon's
start folder; bash requires --cwd. --wait follows a submission past Unknown until terminal or interrupt.
Closing never cancels a call. The server admits at most two worker-credential
connections (worker or CLI) at a time; HTTP 503 means both are taken, the
server could not check the credential, or it is shutting down. Cancel never gets
a reply; its following Query drives the exit code and prints unknown call id
when the ID does not exist.

Exit codes: 0 = tool result (including is_error), or cancel answered Cancelled
or Dispatched (intent recorded); 1 = rejected/lost/offline/cancelled before
dispatch, unknown call ID, usage, or another local error; 3 = outcome unknown
(Unknown, or transport failure/interrupt after Submit was sent), always with
the call ID; 4 = query found the call still Accepted or Dispatched.`

type callExit struct {
	code int
	err  error
}

func (e callExit) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("call outcome (exit %d)", e.code)
}
func (e callExit) Unwrap() error { return e.err }

func callCommand(args []string, in io.Reader, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() { fmt.Fprintln(errOut, callUsage); fs.PrintDefaults() }
	device := fs.String("device", "", "device name")
	id := fs.String("call-id", "", "call ID (default generated UUID)")
	session := fs.String("session", "", "session ID (default generated UUID)")
	cwd := fs.String("cwd", "", "working directory on the daemon (required by bash; default empty)")
	wait := fs.Bool("wait", false, "keep following a submission past Unknown")
	credentials := fs.String("credentials", "", "worker credential file (default $XDG_CONFIG_HOME/circus/worker.json)")
	ca := fs.String("ca-file", "", "PEM CA bundle (default credential's CA file)")
	offset := fs.Uint64("offset", 0, "read starting line")
	limit := fs.Uint64("limit", 0, "read maximum lines")
	old := fs.String("old", "", "edit text to replace")
	replacement := fs.String("new", "", "replacement text")
	all := fs.Bool("replace-all", false, "replace every matching occurrence")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New(callUsage)
	}
	query, cancel := pos[0] == "query", pos[0] == "cancel"
	if (query || cancel) && !identifier.Valid(pos[1], false) {
		return errors.New("invalid call ID: use 1-128 ASCII letters, digits, '_' or '-'")
	}
	var call *circusv1.DeviceCall
	if !query && !cancel {
		if *device == "" {
			return errors.New("--device is required")
		}
		if *id == "" {
			*id = uuid.NewString()
		}
		if *session == "" {
			*session = uuid.NewString()
		}
		if !identifier.Valid(*id, false) {
			return errors.New("invalid call ID: use 1-128 ASCII letters, digits, '_' or '-'")
		}
		if !identifier.Valid(*session, false) {
			return errors.New("invalid session ID: use 1-128 ASCII letters, digits, '_' or '-'")
		}
		call = &circusv1.DeviceCall{CallId: *id, SessionId: *session}
		switch pos[0] {
		case "bash":
			if *cwd == "" {
				return errors.New("bash requires --cwd DIR")
			}
			call.Tool = &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: pos[1], Cwd: *cwd}}
		case "read":
			if *offset > math.MaxUint32 || *limit > math.MaxUint32 {
				return errors.New("offset and limit must fit uint32")
			}
			r := &circusv1.ReadRequest{Path: pos[1], Cwd: *cwd, Limit: uint32(*limit)}
			fs.Visit(func(f *flag.Flag) {
				if f.Name == "offset" {
					v := uint32(*offset)
					r.Offset = &v
				}
			})
			call.Tool = &circusv1.DeviceCall_Read{Read: r}
		case "write":
			// Bound stdin before allocating a protobuf tree; the server applies its
			// negotiated limit independently (including envelope overhead).
			data, err := io.ReadAll(io.LimitReader(in, 64<<20+1))
			if err != nil {
				return err
			}
			if len(data) > 64<<20 {
				return errors.New("write input exceeds 64 MiB")
			}
			call.Tool = &circusv1.DeviceCall_Write{Write: &circusv1.WriteRequest{Path: pos[1], Content: string(data), Cwd: *cwd}}
		case "edit":
			seen := map[string]bool{}
			fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
			if !seen["old"] || !seen["new"] {
				return errors.New("edit requires --old and --new")
			}
			call.Tool = &circusv1.DeviceCall_Edit{Edit: &circusv1.EditRequest{Path: pos[1], OldString: *old, NewString: *replacement, ReplaceAll: *all, Cwd: *cwd}}
		default:
			return errors.New(callUsage)
		}
	}
	path := *credentials
	if path == "" {
		path, err = credfile.Path(credfile.RoleWorker)
		if err != nil {
			return err
		}
	}
	f, err := credfile.Load(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := workerclient.Dial(ctx, workerclient.Options{Credentials: f, CAFile: *ca, ClientVersion: version})
	if err != nil {
		return err
	}
	defer client.Close()
	if query || cancel {
		if cancel {
			if err = client.Cancel(ctx, pos[1]); err != nil {
				return err
			}
		}
		u, err := client.Query(ctx, pos[1])
		if err != nil {
			return err
		}
		printErr := printCallUpdate(u, out, errOut)
		if cancel && u.GetCancelled() != nil {
			return nil
		}
		if printErr != nil {
			return printErr
		}
		if query && (u.GetAccepted() != nil || u.GetDispatched() != nil) {
			return callExit{code: 4}
		}
		return nil
	}
	updates, err := client.Submit(ctx, *device, call)
	if err != nil {
		if errors.Is(err, workerclient.ErrSubmitSent) {
			return callExit{code: 3, err: fmt.Errorf("call %s: %w; query this call ID after reconnecting", call.CallId, err)}
		}
		return fmt.Errorf("call %s: %w", call.CallId, err)
	}
	defer updates.Close()
	for {
		u, err := updates.Next(ctx)
		if err != nil {
			return callExit{code: 3, err: fmt.Errorf("call %s: %w; query this call ID after reconnecting", call.CallId, err)}
		}
		printErr := printCallUpdate(u, out, errOut)
		if *wait && u.GetUnknown() != nil {
			var outcome callExit
			if errors.As(printErr, &outcome) && outcome.code == 3 {
				continue
			}
		}
		if printErr != nil {
			return printErr
		}
		if wire.Terminal(u) {
			return nil
		}
	}
}
func printCallUpdate(u *circusv1.CallUpdate, out, errOut io.Writer) error {
	data, err := protojson.Marshal(u)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(errOut, string(data)); err != nil {
		return err
	}
	if r := u.GetResult(); r != nil {
		for _, block := range r.GetOutput().GetContent() {
			if _, err = fmt.Fprint(out, block.GetText()); err != nil {
				return err
			}
		}
		return nil
	}
	if u.GetUnknown() != nil {
		fmt.Fprintf(errOut, "Outcome unknown for call %s; run circus call query %s\n", u.CallId, u.CallId)
		return callExit{code: 3}
	}
	if u.GetRejected().GetCode() == circusv1.CallUpdate_Rejected_CODE_NOT_FOUND {
		fmt.Fprintln(errOut, "unknown call id")
		return callExit{code: 1}
	}
	if wire.Terminal(u) {
		return callExit{code: 1}
	}
	return nil
}
