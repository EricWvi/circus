package questions_test

import (
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/questions"
)

func TestParse(t *testing.T) {
	qs, err := questions.Parse(`{"questions":[{"question":"Q?","header":"H","multiSelect":true,"extra":1,` +
		`"options":[{"label":"A","description":"a","preview":"p"},{"label":"B"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Question != "Q?" || qs[0].Header != "H" || !qs[0].MultiSelect || len(qs[0].Options) != 2 || qs[0].Options[1].Label != "B" {
		t.Fatalf("questions %+v", qs)
	}
	if questions.Summary(qs) != "Q?" {
		t.Fatal(questions.Summary(qs))
	}
	for _, bad := range []string{
		"", "null", `[]`, `{"questions":null}`, "{\"questions\":[]}\xff",
		`{"questions":[{"question":"Q?","header":"H","multiSelect":false,"options":[{"label":"A","description":1},{"label":"B"}]}]}`,
		`{"questions":[{"question":"Q?","header":"H","multiSelect":false,"options":[{"label":"A","preview":{}},{"label":"B"}]}]}`,
		`{"questions":[{"question":"` + strings.Repeat("q", questions.MaxQuestionBytes+1) + `","header":"H","multiSelect":false,"options":[{"label":"A"},{"label":"B"}]}]}`,
		`{"questions":[{"question":"Q?","header":"H","multiSelect":false,"options":[{"label":"` + strings.Repeat("l", questions.MaxLabelBytes+1) + `"},{"label":"B"}]}]}`,
	} {
		if _, err := questions.Parse(bad); err == nil {
			t.Fatalf("parsed %.80q", bad)
		}
	}
}

func TestAnswerText(t *testing.T) {
	got := questions.AnswerText([]*circusv1.QuestionAnswer{
		{Question: "Which <color>?", Selected: []string{"Blue"}},
		{Question: `Say "hi"`, Other: "tab\there\u2028"},
		{Question: "Which features?", Selected: []string{"A", "B"}, Other: "and C"},
	})
	want := `The user answered your questions: "Which <color>?"="Blue", "Say \"hi\""="tab\there\u2028", ` +
		`"Which features?"="A, B, and C". Continue with these answers in mind.`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
