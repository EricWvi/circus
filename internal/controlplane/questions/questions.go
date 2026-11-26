// Package questions validates the model's AskUserQuestion input, carried by
// a question approval, and the user's answers to it, and writes the input
// that gives the answers to the model.
package questions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// Tool is the name of the tool a question approval carries.
const Tool = "AskUserQuestion"

// Bounds of an AskUserQuestion input (the whole input is bounded by
// store.MaxQuestionInputBytes) and of the answers.
const (
	MinQuestions = 1
	MaxQuestions = 4
	MinOptions   = 2
	MaxOptions   = 4
	// MaxQuestionBytes bounds a question's text, and so the question an
	// answer names.
	MaxQuestionBytes = 4 << 10
	// MaxHeaderBytes bounds a question's header (a short chip).
	MaxHeaderBytes = 256
	// MaxLabelBytes bounds an option's label.
	MaxLabelBytes = 1 << 10
	// MaxOtherBytes bounds the user's own answer.
	MaxOtherBytes = 4 << 10
)

// Option is one choice of a question.
type Option struct {
	Label string
}

// Question is one question of an AskUserQuestion input.
type Question struct {
	Question    string
	Header      string
	Options     []Option
	MultiSelect bool
}

// Parse validates an AskUserQuestion input (as the CLI gave it to the
// worker's canUseTool) and returns its questions: 1-4 questions, each with
// a nonempty question (unique in the input) and header, 2-4 options with
// nonempty labels unique in the question, and a boolean multiSelect.
// Other fields (an option's description and preview, fields the tool may
// add) are kept in the stored input but not read here. Errors describe
// the first problem.
func Parse(inputJSON string) ([]Question, error) {
	if !utf8.ValidString(inputJSON) {
		return nil, errors.New("the input is not valid UTF-8")
	}
	var raw struct {
		Questions []struct {
			Question    *string `json:"question"`
			Header      *string `json:"header"`
			MultiSelect *bool   `json:"multiSelect"`
			Options     []struct {
				Label       *string `json:"label"`
				Description any     `json:"description"`
				Preview     any     `json:"preview"`
			} `json:"options"`
		} `json:"questions"`
	}
	dec := json.NewDecoder(strings.NewReader(inputJSON))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("the input is not an AskUserQuestion input: %v", err)
	}
	if dec.More() {
		return nil, errors.New("the input has data after its JSON object")
	}
	if n := len(raw.Questions); n < MinQuestions || n > MaxQuestions {
		return nil, fmt.Errorf("the input needs %d-%d questions, not %d", MinQuestions, MaxQuestions, n)
	}
	out := make([]Question, 0, len(raw.Questions))
	seen := map[string]bool{}
	for i, rq := range raw.Questions {
		var q Question
		switch {
		case rq.Question == nil || *rq.Question == "":
			return nil, fmt.Errorf("question %d has no question text", i+1)
		case len(*rq.Question) > MaxQuestionBytes:
			return nil, fmt.Errorf("question %d is longer than %d bytes", i+1, MaxQuestionBytes)
		case seen[*rq.Question]:
			return nil, fmt.Errorf("question %d repeats an earlier question", i+1)
		case rq.Header == nil || *rq.Header == "":
			return nil, fmt.Errorf("question %d has no header", i+1)
		case len(*rq.Header) > MaxHeaderBytes:
			return nil, fmt.Errorf("question %d has a header longer than %d bytes", i+1, MaxHeaderBytes)
		case rq.MultiSelect == nil:
			return nil, fmt.Errorf("question %d has no boolean multiSelect", i+1)
		case len(rq.Options) < MinOptions || len(rq.Options) > MaxOptions:
			return nil, fmt.Errorf("question %d needs %d-%d options, not %d", i+1, MinOptions, MaxOptions, len(rq.Options))
		}
		seen[*rq.Question] = true
		q.Question, q.Header, q.MultiSelect = *rq.Question, *rq.Header, *rq.MultiSelect
		labels := map[string]bool{}
		for j, ro := range rq.Options {
			switch {
			case ro.Label == nil || *ro.Label == "":
				return nil, fmt.Errorf("option %d of question %d has no label", j+1, i+1)
			case len(*ro.Label) > MaxLabelBytes:
				return nil, fmt.Errorf("option %d of question %d has a label longer than %d bytes", j+1, i+1, MaxLabelBytes)
			case labels[*ro.Label]:
				return nil, fmt.Errorf("option %d of question %d repeats a label", j+1, i+1)
			}
			if _, ok := ro.Description.(string); ro.Description != nil && !ok {
				return nil, fmt.Errorf("option %d of question %d has a description that is not a string", j+1, i+1)
			}
			if _, ok := ro.Preview.(string); ro.Preview != nil && !ok {
				return nil, fmt.Errorf("option %d of question %d has a preview that is not a string", j+1, i+1)
			}
			labels[*ro.Label] = true
			q.Options = append(q.Options, Option{Label: *ro.Label})
		}
		out = append(out, q)
	}
	return out, nil
}

// Summary is a question approval's display: its first question.
func Summary(qs []Question) string {
	if len(qs) == 0 {
		return ""
	}
	return qs[0].Question
}

// CheckAnswers validates the answers to qs: exactly one per question, in
// order, each naming its question verbatim; selected labels are the
// question's option labels, without repeats, more than one only when it is
// multiSelect; selected and other are not both empty; other is at most
// MaxOtherBytes.
func CheckAnswers(qs []Question, answers []*circusv1.QuestionAnswer) error {
	if len(answers) != len(qs) {
		return fmt.Errorf("%d answers for %d questions", len(answers), len(qs))
	}
	for i, a := range answers {
		q := qs[i]
		if a.GetQuestion() != q.Question {
			return fmt.Errorf("answer %d does not name question %d verbatim", i+1, i+1)
		}
		if len(a.GetSelected()) == 0 && a.GetOther() == "" {
			return fmt.Errorf("answer %d selects nothing and gives no answer of its own", i+1)
		}
		if len(a.GetSelected()) > 1 && !q.MultiSelect {
			return fmt.Errorf("answer %d selects %d options of a single-choice question", i+1, len(a.GetSelected()))
		}
		if len(a.GetOther()) > MaxOtherBytes {
			return fmt.Errorf("answer %d is longer than %d bytes", i+1, MaxOtherBytes)
		}
		chosen := map[string]bool{}
		for _, label := range a.GetSelected() {
			if chosen[label] {
				return fmt.Errorf("answer %d selects an option twice", i+1)
			}
			chosen[label] = true
			found := false
			for _, o := range q.Options {
				if o.Label == label {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("answer %d selects a label that is not an option of question %d", i+1, i+1)
			}
		}
	}
	return nil
}

// AnswerText is the input that gives checked answers to the model, after
// the tool result Claude Code writes for an answered AskUserQuestion:
//
//	The user answered your questions: "<question>"="<answer>", .... Continue with these answers in mind.
//
// An answer is its selected labels joined with ", ", then the user's own
// answer after them. Each question and answer is quoted as a JSON string,
// so quotes, backslashes and control characters in the user's text cannot
// break the format.
func AnswerText(answers []*circusv1.QuestionAnswer) string {
	var b strings.Builder
	b.WriteString("The user answered your questions: ")
	for i, a := range answers {
		if i > 0 {
			b.WriteString(", ")
		}
		parts := append([]string(nil), a.GetSelected()...)
		if a.GetOther() != "" {
			parts = append(parts, a.GetOther())
		}
		b.WriteString(quote(a.GetQuestion()))
		b.WriteByte('=')
		b.WriteString(quote(strings.Join(parts, ", ")))
	}
	b.WriteString(". Continue with these answers in mind.")
	return b.String()
}

// quote is s as a JSON string, without HTML escaping.
func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return strings.TrimSuffix(buf.String(), "\n")
}
