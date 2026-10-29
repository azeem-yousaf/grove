package gh

import "testing"

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
