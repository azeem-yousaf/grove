package gh

import (
	"fmt"
	"strings"
	"testing"
)

func TestNewest(t *testing.T) {
	got := newest([]PR{
		{Number: 3, State: "CLOSED", Branch: "a"},
		{Number: 5, State: "OPEN", Branch: "a"},
		{Number: 7, State: "MERGED", Branch: "a"},
		{Number: 2, State: "MERGED", Branch: "b"},
		{Number: 4, State: "CLOSED", Branch: "b"},
	})
	if got["a"].Number != 5 || got["b"].Number != 4 {
		t.Errorf("newest = %+v", got)
	}
}

func TestLabel(t *testing.T) {
	for _, c := range []struct {
		pr   PR
		want string
	}{
		{PR{Number: 12, State: "OPEN"}, "#12 open"},
		{PR{Number: 12, State: "OPEN", Draft: true}, "#12 draft"},
		{PR{Number: 9, State: "MERGED"}, "#9 merged"},
	} {
		if got := c.pr.Label(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.pr, got, c.want)
		}
	}
}

func TestParseReview(t *testing.T) {
	r, n, err := parseView([]byte(`{"number":7,"state":"OPEN",
		"reviews":[{"id":"R1","author":{"login":"ann"},"body":"","state":"COMMENTED"},
			{"id":"R2","author":{"login":"ann"},"body":"Rename it.","state":"CHANGES_REQUESTED"}],
		"comments":[{"id":"C1","author":{"login":"bob"},"body":"Looks good otherwise"},{"id":"C2","author":{"login":"bob"},"body":" "}]}`))
	if err != nil || n != 7 || r.State != "OPEN" {
		t.Fatalf("parseView: %v %d %+v", err, n, r)
	}
	err = r.addLineComments([]byte(`[{"id":11,"user":{"login":"ann"},"body":"Off by one","path":"a.go","line":12}]
[{"id":12,"user":{"login":"ann"},"body":"Outdated","path":"b.go","line":0,"original_line":3}]`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range r.Feedback {
		got = append(got, fmt.Sprintf("%s %s %s:%d %q %s", f.ID, f.Author, f.Path, f.Line, f.Body, f.State))
	}
	want := []string{`review:R2 ann :0 "Rename it." CHANGES_REQUESTED`, `comment:C1 bob :0 "Looks good otherwise" `,
		`line:11 ann a.go:12 "Off by one" `, `line:12 ann b.go:3 "Outdated" `}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("feedback =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
