// Package gh reads pull requests through the GitHub CLI, when it is
// installed and signed in. grove never handles GitHub tokens itself.
package gh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// PR is a pull request, as much of it as grove shows.
type PR struct {
	Number int    `json:"number"`
	State  string `json:"state"` // OPEN, CLOSED or MERGED
	Draft  bool   `json:"isDraft"`
	URL    string `json:"url"`
	Branch string `json:"headRefName"`
}

// Label is the PR's number and state in words: "#12 open", "#12 draft".
func (p PR) Label() string {
	state := strings.ToLower(p.State)
	if p.Draft && p.State == "OPEN" {
		state = "draft"
	}
	return fmt.Sprintf("#%d %s", p.Number, state)
}

// Path is the gh executable, or "" when it isn't installed.
func Path() string {
	p, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	return p
}

// List returns the newest pull request of each branch, open ones first, for
// the repository dir is in.
func List(ctx context.Context, dir string) (map[string]PR, error) {
	bin := Path()
	if bin == "" {
		return nil, errors.New("gh isn't installed")
	}
	cmd := exec.CommandContext(ctx, bin, "pr", "list", "--state", "all", "--limit", "200",
		"--json", "number,state,isDraft,url,headRefName")
	cmd.Dir = dir
	// Never wait for input or print update notices into the JSON.
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("gh pr list: %s", firstLine(msg))
		}
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []PR
	if err := json.Unmarshal(stdout.Bytes(), &prs); err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	return newest(prs), nil
}

// newest keeps one PR per branch: an open one if there is, otherwise the
// highest number.
func newest(prs []PR) map[string]PR {
	out := map[string]PR{}
	for _, p := range prs {
		cur, ok := out[p.Branch]
		switch {
		case !ok:
			out[p.Branch] = p
		case (p.State == "OPEN") != (cur.State == "OPEN"):
			if p.State == "OPEN" {
				out[p.Branch] = p
			}
		case p.Number > cur.Number:
			out[p.Branch] = p
		}
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
