package fakeworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/google/uuid"
)

// ledgerOp is one operation of `/fake ledger <json>` on a file of the
// session's ledger snapshot. Paths are relative to the snapshot.
//
//	{"op":"write","path":"...","content":"..."}  Write the whole file.
//	{"op":"edit","path":"...","old":"...","new":"..."}  Edit: replace old, which must occur once.
//	{"op":"move","from":"...","to":"..."}  Bash: mkdir -p the target's directory, then mv.
//	{"op":"delete","path":"..."}  Bash: rm -f.
type ledgerOp struct {
	Op      string `json:"op"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Old     string `json:"old"`
	New     string `json:"new"`
	From    string `json:"from"`
	To      string `json:"to"`
}

func (op ledgerOp) String() string {
	if op.Op == "move" {
		return "move " + op.From + " -> " + op.To
	}
	return op.Op + " " + op.Path
}

// parseLedgerOps decodes and checks the JSON array of `/fake ledger`.
func parseLedgerOps(arg string) ([]ledgerOp, error) {
	dec := json.NewDecoder(strings.NewReader(arg))
	dec.DisallowUnknownFields()
	var ops []ledgerOp
	if err := dec.Decode(&ops); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the operations")
	}
	if len(ops) == 0 {
		return nil, errors.New("no operations")
	}
	for i, op := range ops {
		var paths []string
		switch op.Op {
		case "write", "delete":
			paths = []string{op.Path}
		case "edit":
			paths = []string{op.Path}
			if op.Old == op.New {
				return nil, fmt.Errorf("operation %d: old and new are the same", i+1)
			}
		case "move":
			paths = []string{op.From, op.To}
		default:
			return nil, fmt.Errorf("operation %d: unknown op %q (write, edit, move or delete)", i+1, op.Op)
		}
		for _, p := range paths {
			if err := checkLedgerPath(p); err != nil {
				return nil, fmt.Errorf("operation %d (%s): %w", i+1, op.Op, err)
			}
		}
	}
	return ops, nil
}

// checkLedgerPath accepts a path to a file inside the ledger snapshot.
func checkLedgerPath(p string) error {
	switch {
	case p == "":
		return errors.New("missing path")
	case path.IsAbs(p):
		return fmt.Errorf("path %q is absolute, not relative to the ledger", p)
	case path.Clean(p) == ".":
		return fmt.Errorf("path %q names the ledger directory", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path %q leaves the ledger", p)
		}
	}
	return nil
}

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ledgerCall is the device tool use that carries out op on the session's
// ledger snapshot; op has been checked by parseLedgerOps.
func (s *session) ledgerCall(op ledgerOp) toolCall {
	file := func(p string) string { return path.Join(s.ledgerPath, p) }
	switch op.Op {
	case "write":
		abs := file(op.Path)
		call := &pb.DeviceCall{CallId: uuid.NewString(), SessionId: s.id, Tool: &pb.DeviceCall_Write{
			Write: &pb.WriteRequest{Path: abs, Content: op.Content, Cwd: s.cwd},
		}}
		return toolCall{call: call, id: "toolu_" + uuid.NewString(), name: "Write", input: object{"file_path": abs, "content": op.Content}}
	case "edit":
		abs := file(op.Path)
		call := &pb.DeviceCall{CallId: uuid.NewString(), SessionId: s.id, Tool: &pb.DeviceCall_Edit{
			Edit: &pb.EditRequest{Path: abs, OldString: op.Old, NewString: op.New, Cwd: s.cwd},
		}}
		input := object{"file_path": abs, "old_string": op.Old, "new_string": op.New}
		return toolCall{call: call, id: "toolu_" + uuid.NewString(), name: "Edit", input: input}
	case "move":
		from, to := file(op.From), file(op.To)
		return s.newToolCall("bash", "mkdir -p "+shellQuote(path.Dir(to))+" && mv "+shellQuote(from)+" "+shellQuote(to))
	default: // delete
		return s.newToolCall("bash", "rm -f "+shellQuote(file(op.Path)))
	}
}

// ledger runs `/fake ledger <json>`: one device tool call per operation, in
// order, each in an API message of its own. The first error result ends the
// turn with a text naming the operation. Without a snapshot, or with
// operations it refuses, it answers with a text and calls nothing.
func (s *session) ledger(t *turn, arg string) error {
	if s.ledgerPath == "" {
		return s.stream(t, "no ledger snapshot", s.w.opts.StreamDelay, plainText)
	}
	ops, err := parseLedgerOps(arg)
	if err != nil {
		return s.stream(t, "/fake ledger refused: "+err.Error(), s.w.opts.StreamDelay, plainText)
	}
	for i, op := range ops {
		u := s.ledgerCall(op)
		if err := s.announce(t, u); err != nil {
			return err
		}
		_, failed, err := s.callTool(t, u, "", false)
		if err != nil {
			return err
		}
		if failed {
			text := fmt.Sprintf("Ledger operation %d of %d (%s) failed.", i+1, len(ops), op)
			return s.stream(t, text, s.w.opts.StreamDelay, plainText)
		}
	}
	text := fmt.Sprintf("Applied %d ledger operations.", len(ops))
	if len(ops) == 1 {
		text = "Applied 1 ledger operation."
	}
	return s.stream(t, text, s.w.opts.StreamDelay, plainText)
}
