package ledger

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func TestFormatRoundTrip(t *testing.T) {
	titles := []string{
		"<<", "中文 title", "quoted: # title", "2026-01-02", "null", "true", "000", " leading ", "[list]", "title\x00",
		"---", "--- x", "- a", "? x", ": x", "#x", "'x", "\"x", "%x", "@x", "`x", "!x", "&x", "*x", "|", ">", "{", "[", "~",
		"Null", "NULL", "yes", "no", "on", "off", "y", "n", "Y", "N", "1e3", "0x1F", "0o17", "017", ".inf", "-.inf", ".nan", "1_000",
		"12:30:00", "190:20:30", "2001-12-14t21:59:43.10-05:00", "2001-12-14 21:59:43.10 -5", "=",
		"\t", "a\tb", "a\vb", "a\fb", "\ufeffbom", "a\u00a0", "\u200b", "a #b", "a: b", "a:b", "x\x7f", "x\u0080",
		"\\", "\\n", "'", "''", "\"", strings.Repeat("word ", 102) + "xy", strings.Repeat("a", 512), strings.Repeat("--- ", 128),
		"...", "... x", "%YAML 1.2", "!!str x", "! x", "\U0001F600", "a\u202eb",
	}
	for _, title := range titles {
		t.Run(title, func(t *testing.T) {
			want := Decision{Title: title, Body: []byte("first\r\n\r\n---\n正文\xff\x00")}
			rendered, err := Render(want)
			if err != nil {
				t.Fatal(err)
			}
			for _, crlf := range []bool{false, true} {
				if crlf {
					header, body, _ := bytes.Cut(rendered, []byte("\n\n"))
					rendered = append(bytes.ReplaceAll(header, []byte("\n"), []byte("\r\n")), []byte("\r\n\r\n")...)
					rendered = append(rendered, body...)
				}
				got, err := Parse(rendered)
				if err != nil || got.Title != want.Title || got.Number != want.Number || !bytes.Equal(got.Body, want.Body) {
					t.Fatalf("Parse(%q) = %+v, %v", rendered, got, err)
				}
			}
		})
	}
}

func TestFormatYAMLSignificantTitles(t *testing.T) {
	alphabet := []rune("<=-.:?!&*|>#'\"%@`~{}[],01aeEnNyYtTf \t")
	var visit func([]rune)
	visit = func(prefix []rune) {
		if len(prefix) > 0 {
			d := Decision{Title: string(prefix)}
			data, err := Render(d)
			check(t, err)
			got, err := Parse(data)
			if err != nil || got.Title != d.Title {
				t.Fatalf("title %q parsed as %q: %v", d.Title, got.Title, err)
			}
		}
		if len(prefix) < 3 {
			for _, r := range alphabet {
				visit(append(prefix, r))
			}
		}
	}
	visit(nil)
}

func TestFormatCanonicalFrontMatter(t *testing.T) {
	for _, tc := range []struct{ name, header, problem string }{
		{"flow", "{number: D0, title: sample}\n", "flow mapping"},
		{"inline comment", "number: D0 # comment\ntitle: sample\n", "comments"},
		{"line comment", "number: D0\ntitle: sample\n# comment\n", "comments"},
		{"tag", "number: D0\ntitle: !!str sample\n", "tags"},
		{"anchor", "number: D0\ntitle: &label sample\n", "anchors"},
		{"alias", "number: D0\ntitle: *label\n", "aliases"},
		{"document end", "number: D0\ntitle: sample\n...\n", "document-end"},
		{"block", "number: D0\ntitle: >-\n  sample\n", "block scalar"},
		{"quoted keys", "\"number\": D0\n' title': sample\n", "unquoted keys"},
		{"key order", "title: sample\nnumber: D0\n", "in that order"},
		{"trailing comment", "number: D0\ntitle: sample # comment\n", "comments"},
		{"comment after quoted value", "number: D0\ntitle: \"sample\" # comment\n", "comments"},
		{"tab after key", "number: D0\ntitle:\tsample\n", `use one space, not a tab, after "title:"`},
		{"no space after key", "number:D0\ntitle: sample\n", `put one space after "number:"`},
		{"two spaces after key", "number: D0\ntitle:  sample\n", `use exactly one space after "title:"`},
		{"missing value", "number: D0\ntitle:\n", "title has no value"},
		{"blank value", "number: D0\ntitle: \t\n", "title has no value"},
		{"trailing space", "number: D0 \ntitle: sample\n", "remove the whitespace after the number value"},
		{"trailing tab", "number: D0\ntitle: \"sample\"\t\n", "remove the whitespace after the title value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := "D0-sample.md"
			expectProblem(t, ValidateTree(map[string][]byte{p: []byte("---\n" + tc.header + "---\n\n")}), p, tc.problem)
		})
	}
	p := "D0-sample.md"
	expectProblem(t, ValidateTree(map[string][]byte{p: []byte("\ufeff---\nnumber: D0\ntitle: sample\n---\n\n")}), p, "byte order mark")
	for _, title := range []string{"sample", "'sample'", "\"sample\""} {
		d, err := Parse([]byte("---\nnumber: D0\ntitle: " + title + "\n---\n\n"))
		if err != nil || d.Title != "sample" {
			t.Fatalf("canonical title %q: %+v, %v", title, d, err)
		}
	}
}

func TestFileName(t *testing.T) {
	for _, tc := range []struct{ title, name string }{
		{"worker 设计取舍", "worker-设计取舍"},
		{"..-first /\\:*?\"<>|\x01\t\u2002 --second.-.", "first-second"},
		{strings.Repeat("界", 61), strings.Repeat("界", 60)},
		{"---..", ""},
		{strings.Repeat("a", 59) + " b", strings.Repeat("a", 59)},
		{strings.Repeat("a", 58) + "..b", strings.Repeat("a", 58)},
		{strings.Repeat("a", 60) + "b", strings.Repeat("a", 60)},
		{"ab\u200b\u202ecd", "ab-cd"},
		{" interior... punctuation ", "interior...-punctuation"},
	} {
		if got, want := FileName(0, tc.title), "D0-"+tc.name+".md"; got != want {
			t.Errorf("FileName(%q) = %q, want %q", tc.title, got, want)
		}
	}
}

func decisionFiles(t *testing.T, decisions ...Decision) map[string][]byte {
	t.Helper()
	files := map[string][]byte{indexName: RenderIndex(decisions)}
	for _, d := range decisions {
		b, err := Render(d)
		if err != nil {
			t.Fatal(err)
		}
		files[decisionPath(d)] = b
	}
	return files
}

func expectProblem(t *testing.T, problems []Problem, path, msg string) {
	t.Helper()
	for _, p := range problems {
		if p.Path == path && strings.Contains(p.Message, msg) {
			return
		}
	}
	t.Fatalf("missing problem for %q containing %q: %+v", path, msg, problems)
}

func TestValidateTree(t *testing.T) {
	t.Run("reports independent problems", func(t *testing.T) {
		first := Decision{Title: "one", Dir: "a"}
		second := Decision{Title: "two", Dir: "b"}
		files := decisionFiles(t, first, second)
		files["README.md"] = []byte("unexpected")
		files["hidden/.dir/D3-wrong.md"] = []byte("---\nnumber: D3\ntitle: correct\n---\n\n")
		files["D4-extra.md"] = []byte("---\nnumber: D4\ntitle: extra\nstatus: done\n---\n\n")
		files["D5-repeat.md"] = []byte("---\nnumber: D5\nnumber: D5\n---\n\n")
		files["D6-type.md"] = []byte("---\nnumber: D6\ntitle: true\n---\n\n")
		files["D7-number.md"] = []byte("---\nnumber: D07\ntitle: number\n---\n\n")
		files["D8-delimiter.md"] = []byte("---\nnumber: D8\ntitle: delimiter\n")
		files["D9-body.md"] = []byte("---\nnumber: D9\ntitle: body\n---\n\n\xff")
		files["INDEX.md/folder/D10-collision.md"] = []byte("---\nnumber: D10\ntitle: collision\n---\n\n")
		files[indexName] = []byte("stale")
		problems := ValidateTree(files)
		for p, msg := range map[string]string{
			decisionPath(second): "also occurs", "README.md": "unexpected",
			"hidden/.dir/D3-wrong.md": "segments", "D4-extra.md": "exactly", "D5-repeat.md": "exactly",
			"D6-type.md": "YAML strings", "D7-number.md": "decimal", "D8-delimiter.md": "delimiter",
			"D9-body.md": "UTF-8", "INDEX.md/folder/D10-collision.md": "conflicts", indexName: "generated",
		} {
			expectProblem(t, problems, p, msg)
		}
		expectProblem(t, problems, "hidden/.dir/D3-wrong.md", "does not match")
		delete(files, indexName)
		expectProblem(t, ValidateTree(files), indexName, "missing")
	})
	t.Run("bounds", func(t *testing.T) {
		dir := strings.Join([]string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 61)}, "/")
		d := Decision{Dir: dir, Number: store.MaxLedgerNumber, Title: strings.Repeat("界", 170) + "ab"}
		header, err := Render(d)
		if err != nil {
			t.Fatal(err)
		}
		d.Body = bytes.Repeat([]byte("x"), MaxDecisionFileBytes-len(header))
		if problems := ValidateTree(decisionFiles(t, d)); len(problems) != 0 {
			t.Fatal(problems)
		}
		for _, tc := range []struct {
			mutate  func(*Decision)
			message string
		}{
			{func(d *Decision) { d.Body = append(d.Body, 'x') }, "file exceeds"},
			{func(d *Decision) { d.Title += "x" }, "512 bytes"},
			{func(d *Decision) { d.Title = "" }, "nonempty"},
			{func(d *Decision) { d.Title = "two\nlines" }, "single"},
			{func(d *Decision) { d.Dir += "x" }, "256 bytes"},
			{func(d *Decision) { d.Dir = strings.Repeat("x", 65) }, "64 bytes"},
			{func(d *Decision) { d.Dir = "a//b" }, "segments"},
			{func(d *Decision) { d.Dir = "a/./b" }, "segments"},
			{func(d *Decision) { d.Dir = "a/../b" }, "segments"},
			{func(d *Decision) { d.Dir = "a/" }, "segments"},
			{func(d *Decision) { d.Number++ }, "next number"},
		} {
			mutant := d
			tc.mutate(&mutant)
			expectProblem(t, ValidateTree(decisionFiles(t, mutant)), decisionPath(mutant), tc.message)
		}
	})
	t.Run("decision count", func(t *testing.T) {
		var decisions []Decision
		for i := range store.MaxLedgerDecisions + 1 {
			decisions = append(decisions, Decision{Number: uint32(i), Title: fmt.Sprint(i)})
		}
		if problems := ValidateTree(decisionFiles(t, decisions[:store.MaxLedgerDecisions]...)); len(problems) != 0 {
			t.Fatal(problems)
		}
		expectProblem(t, ValidateTree(decisionFiles(t, decisions...)), ".", "4096")
	})
}

func TestRenderIndex(t *testing.T) {
	got := RenderIndex([]Decision{{Number: 10, Dir: "a|b", Title: "slash\\pipe|"}, {Title: "root"}})
	want := "| Number | Directory | Title |\n| --- | --- | --- |\n| D0 |  | root |\n| D10 | a\\|b | slash\\\\pipe\\| |\n"
	if string(got) != want {
		t.Fatalf("index = %q, want %q", got, want)
	}
}
