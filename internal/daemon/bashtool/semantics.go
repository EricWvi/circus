package bashtool

import (
	"strings"
	"syscall"
)

// Exit status 1 is not a failure when the last command of the line is one of
// these: grep, egrep and fgrep (no match), rg, find, diff, test and [. The
// result then has no "Exit code" line and is_error is false. Other names,
// including zgrep, ugrep, cmp, pgrep, which, expr and paths such as
// /usr/bin/grep, keep the ordinary failure; so do statuses other than 1.
var quietStatusOneCommands = map[string]bool{
	"grep": true, "egrep": true, "fgrep": true, "rg": true,
	"find": true, "diff": true, "test": true, "[": true,
}

func exitFailed(command string, code int) bool {
	return code != 0 && (code != 1 || !quietStatusOneCommands[lastCommandName(command)])
}

// lastCommandName returns the literal first word of the last command in a
// shell command line. Commands are separated by unquoted ;, newline, &&, ||, |,
// |& and a backgrounding &; >&, <& and &> are redirections, not separators.
// Each unquoted parenthesis is a segment of its own, so "(cd x && grep y)"
// ends with ")" while "{ grep y; }" ends with "}". Command and arithmetic
// substitutions ($( and $(( ) and process substitutions (<( and >( ) belong to
// the current word up to their matching parenthesis, so "test $(echo 1) = 2"
// ends with test. Quotes, including ANSI-C $'...' quoting where \' escapes,
// and backslashes stay in the word: "\grep" is not grep. A backslash-newline
// joins lines. Comments are ignored. A <<WORD or <<-WORD heredoc operator is
// dropped together with the rest of its line and its body, so
// "grep x <<EOF && echo hit" counts as grep while "cat <<EOF | grep x" and
// "true <<EOF; grep x" count as cat and true; a body without its terminator
// runs to the end. With more than one heredoc operator on a top-level line
// native's choice was not pinned, so lastCommandName then returns "" and
// status 1 stays an error; operators inside $( ) are not counted, as native
// refuses such commands before running them. Inside $( ) and process substitutions, comments, case patterns and
// heredoc bodies do not close the substitution.
// Inferred: shapes beyond the measured ones follow these rules.
func lastCommandName(command string) string {
	last := ""
	var segment strings.Builder
	flush := func() {
		if text := strings.TrimSpace(segment.String()); text != "" {
			last = text
		}
		segment.Reset()
	}
	atWordStart := func() bool {
		s := segment.String()
		return s == "" || strings.ContainsRune(" \t", rune(s[len(s)-1]))
	}
	// ambiguous records a line with more than one heredoc operator.
	ambiguous := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		next := byte(0)
		if i+1 < len(command) {
			next = command[i+1]
		}
		switch {
		case c == '\\' && next == '\n':
			i++
		case c == '\\':
			segment.WriteByte(c)
			if next != 0 {
				segment.WriteByte(next)
				i++
			}
		case c == '\'':
			end := skipQuoted(command, i, false)
			segment.WriteString(command[i:end])
			i = end - 1
		case c == '$' && next == '\'':
			end := skipQuoted(command, i+1, true)
			segment.WriteString(command[i:end])
			i = end - 1
		case c == '"':
			end := skipDoubleQuoted(command, i)
			segment.WriteString(command[i:end])
			i = end - 1
		case (c == '$' || c == '<' || c == '>') && next == '(':
			end := skipParens(command, i+1)
			segment.WriteString(command[i:end])
			i = end - 1
		case c == '(' && next == '(':
			// An arithmetic command: << inside it is a shift, not a heredoc.
			end := skipParens(command, i)
			segment.WriteString(command[i:end])
			i = end - 1
		case strings.HasPrefix(command[i:], "<<<"):
			segment.WriteString("<<<")
			i += 2
		case c == '<' && next == '<':
			j := i + 2
			dash := j < len(command) && command[j] == '-'
			if dash {
				j++
			}
			for j < len(command) && (command[j] == ' ' || command[j] == '\t') {
				j++
			}
			word, _ := heredocWord(command, j)
			lineEnd := strings.IndexByte(command[i:], '\n')
			if lineEnd < 0 {
				// An operator on the last line has no body to skip.
				i = len(command)
				continue
			}
			lineEnd += i
			if strings.Count(strings.ReplaceAll(command[i+2:lineEnd], "<<<", ""), "<<") > 0 {
				ambiguous = true
			}
			flush()
			i = lineEnd
			if word != "" {
				i = skipHeredocBody(command, lineEnd+1, word, dash)
			}
		case c == '#' && atWordStart():
			if end := strings.IndexByte(command[i:], '\n'); end >= 0 {
				i += end - 1
			} else {
				i = len(command)
			}
		case c == '\n' || c == ';':
			flush()
		case c == '|':
			flush()
			if next == '|' || next == '&' {
				i++
			}
		case c == '&':
			s := segment.String()
			switch {
			case next == '&':
				flush()
				i++
			case next == '>' || strings.HasSuffix(s, ">") || strings.HasSuffix(s, "<"):
				segment.WriteByte(c)
			default:
				flush()
			}
		case c == '(' || c == ')':
			flush()
			segment.WriteByte(c)
			flush()
		default:
			segment.WriteByte(c)
		}
	}
	flush()
	if ambiguous {
		return ""
	}
	if fields := strings.Fields(last); len(fields) > 0 {
		return fields[0]
	}
	return ""
}

// skipQuoted returns the index after the single-quoted string starting at
// start. In ANSI-C quoting a backslash escapes the next character.
func skipQuoted(s string, start int, ansiC bool) int {
	for i := start + 1; i < len(s); i++ {
		switch {
		case ansiC && s[i] == '\\':
			i++
		case s[i] == '\'':
			return i + 1
		}
	}
	return len(s)
}

// skipDoubleQuoted returns the index after the double-quoted string at start.
func skipDoubleQuoted(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(s)
}

// skipParens returns the index after the parenthesis matching the one at
// open, honoring nested parentheses, quotes, backslashes, comments, heredoc
// bodies and the ) that ends a case pattern. Within arithmetic (( )) a << is
// a shift.
func skipParens(s string, open int) int {
	arithmetic := open+1 < len(s) && s[open+1] == '('
	const (
		caseWord    = iota // after "case", before "in"
		casePattern        // before a pattern's closing )
		caseCommand        // in a pattern's commands
	)
	var cases []int
	type heredoc struct {
		word string
		dash bool
	}
	var pending []heredoc
	wordStart := func(i int) bool {
		return i == 0 || strings.ContainsRune(" \t\n;|&()", rune(s[i-1]))
	}
	keyword := func(i int, word string) bool {
		end := i + len(word)
		return wordStart(i) && strings.HasPrefix(s[i:], word) &&
			(end == len(s) || strings.ContainsRune(" \t\n;|&()", rune(s[end])))
	}
	depth := 0
	for i := open; i < len(s); i++ {
		mode := -1
		if len(cases) > 0 {
			mode = cases[len(cases)-1]
		}
		switch c := s[i]; {
		case c == '\\':
			i++
		case c == '\'':
			i = skipQuoted(s, i, i > 0 && s[i-1] == '$') - 1
		case c == '"':
			i = skipDoubleQuoted(s, i) - 1
		case c == '#' && wordStart(i):
			if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
				i += end - 1
			} else {
				i = len(s)
			}
		case arithmetic:
			// Only parentheses matter in arithmetic.
			switch c {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		case strings.HasPrefix(s[i:], "<<<"):
			i += 2
		case c == '<' && i+1 < len(s) && s[i+1] == '<':
			j := i + 2
			dash := j < len(s) && s[j] == '-'
			if dash {
				j++
			}
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			word, end := heredocWord(s, j)
			if word != "" {
				pending = append(pending, heredoc{word, dash})
			}
			i = end - 1
		case c == '\n' && len(pending) > 0:
			pos := i + 1
			for _, doc := range pending {
				pos = skipHeredocBody(s, pos, doc.word, doc.dash) + 1
			}
			pending = nil
			i = pos - 2
		case keyword(i, "case") && mode != caseWord && mode != casePattern:
			cases = append(cases, caseWord)
			i += 3
		case keyword(i, "in") && mode == caseWord:
			cases[len(cases)-1] = casePattern
			i++
		case keyword(i, "esac") && mode >= casePattern:
			cases = cases[:len(cases)-1]
			i += 3
		case c == ';' && i+1 < len(s) && s[i+1] == ';' && mode == caseCommand:
			cases[len(cases)-1] = casePattern
			i++
		case c == '(' && mode == casePattern:
			// An optional ( before a case pattern does not nest.
		case c == ')' && mode == casePattern:
			cases[len(cases)-1] = caseCommand
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(s)
}

// heredocWord reads the delimiter word at start, removing its quotes, and
// returns it with the index after it.
func heredocWord(s string, start int) (string, int) {
	var word strings.Builder
	i := start
	for i < len(s) && !strings.ContainsRune(" \t\n;|&<>()", rune(s[i])) {
		switch s[i] {
		case '\'', '"':
			end := strings.IndexByte(s[i+1:], s[i])
			if end < 0 {
				return "", len(s)
			}
			word.WriteString(s[i+1 : i+1+end])
			i += end + 2
		case '\\':
			if i+1 < len(s) {
				word.WriteByte(s[i+1])
			}
			i += 2
		default:
			word.WriteByte(s[i])
			i++
		}
	}
	return word.String(), min(i, len(s))
}

// skipHeredocBody returns the index of the LF ending the delimiter line of a
// body starting at start, or the end of the command when it is unterminated.
func skipHeredocBody(s string, start int, word string, dash bool) int {
	for start < len(s) {
		end := strings.IndexByte(s[start:], '\n')
		line := s[start:]
		if end >= 0 {
			line = s[start : start+end]
		}
		if dash {
			line = strings.TrimLeft(line, "\t")
		}
		if end < 0 {
			return len(s)
		}
		if line == word {
			return start + end
		}
		start += end + 1
	}
	return len(s)
}

// signalStatus is the status reported for a shell that a signal ended on its
// own, as with "kill -TERM $$": 144 for SIGTERM and 1 for every other signal.
// Statuses of commands that the shell itself reports, such as "exit 143",
// are unchanged.
func signalStatus(signal syscall.Signal) int {
	if signal == syscall.SIGTERM {
		return 144
	}
	return 1
}
