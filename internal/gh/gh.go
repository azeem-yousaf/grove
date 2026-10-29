// Package gh reads pull requests through the GitHub CLI, when it is
// installed and signed in. grove never handles GitHub tokens itself.
package gh

import (
	"bytes"
	"cmp"
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

// command runs gh in dir and returns its output, with gh's own message as
// the error when it fails.
func command(ctx context.Context, dir, stdin string, args ...string) ([]byte, error) {
	bin := Path()
	if bin == "" {
		return nil, errors.New("gh isn't installed")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		what := "gh " + strings.Join(args[:min(2, len(args))], " ")
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%s: %s", what, firstLine(msg))
		}
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return stdout.Bytes(), nil
}

// Open finds the open pull request from branch, if there is one.
func Open(ctx context.Context, dir, branch string) (PR, bool, error) {
	out, err := command(ctx, dir, "", "pr", "list", "--state", "open", "--head", branch, "--limit", "1",
		"--json", "number,state,isDraft,url,headRefName")
	if err != nil {
		return PR{}, false, err
	}
	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return PR{}, false, fmt.Errorf("gh pr list: %w", err)
	}
	if len(prs) == 0 {
		return PR{}, false, nil
	}
	return prs[0], true, nil
}

// Create opens a pull request from head into base and returns its URL. The
// body goes through stdin.
func Create(ctx context.Context, dir, base, head, title, body string) (string, error) {
	out, err := command(ctx, dir, body+"\n", "pr", "create", "--base", base, "--head", head, "--title", title, "--body-file", "-")
	if err != nil {
		return "", err
	}
	// gh prints the URL last, after any notes.
	lines := strings.Fields(string(out))
	if len(lines) == 0 {
		return "", errors.New("gh pr create printed no URL")
	}
	return lines[len(lines)-1], nil
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

// Feedback is one thing a reviewer said on a pull request: a review, a
// comment, or a comment on a line of the code.
type Feedback struct {
	ID     string // unique within the pull request, e.g. "review:123"
	Author string
	Body   string
	State  string // a review's: APPROVED, CHANGES_REQUESTED or COMMENTED
	Path   string // a line comment's file
	Line   int    // and line, when GitHub knows it
}

// Review is a pull request's state and everything said on it, oldest
// first.
type Review struct {
	State    string // OPEN, CLOSED or MERGED
	Feedback []Feedback
}

// ReadReview reads the state of the pull request at url (or number), its
// reviews, comments and line comments.
func ReadReview(ctx context.Context, dir, pr string) (Review, error) {
	view, err := command(ctx, dir, "", "pr", "view", pr, "--json", "number,state,reviews,comments")
	if err != nil {
		return Review{}, err
	}
	r, number, err := parseView(view)
	if err != nil {
		return Review{}, err
	}
	// Line comments are only in the REST API.
	lines, err := command(ctx, dir, "", "api", "--paginate", fmt.Sprintf("repos/{owner}/{repo}/pulls/%d/comments", number))
	if err != nil {
		return r, err
	}
	err = r.addLineComments(lines)
	return r, err
}

// parseView reads `gh pr view --json number,state,reviews,comments`.
func parseView(out []byte) (Review, int, error) {
	type author struct {
		Login string `json:"login"`
	}
	var v struct {
		Number  int    `json:"number"`
		State   string `json:"state"`
		Reviews []struct {
			ID     string `json:"id"`
			Author author `json:"author"`
			Body   string `json:"body"`
			State  string `json:"state"`
		} `json:"reviews"`
		Comments []struct {
			ID     string `json:"id"`
			Author author `json:"author"`
			Body   string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Review{}, 0, fmt.Errorf("gh pr view: %w", err)
	}
	r := Review{State: v.State}
	for _, c := range v.Reviews {
		// A review that only holds line comments has no body of its own.
		if strings.TrimSpace(c.Body) != "" || c.State == "APPROVED" || c.State == "CHANGES_REQUESTED" {
			r.Feedback = append(r.Feedback, Feedback{ID: "review:" + c.ID, Author: c.Author.Login, Body: c.Body, State: c.State})
		}
	}
	for _, c := range v.Comments {
		if strings.TrimSpace(c.Body) != "" {
			r.Feedback = append(r.Feedback, Feedback{ID: "comment:" + c.ID, Author: c.Author.Login, Body: c.Body})
		}
	}
	return r, v.Number, nil
}

// addLineComments adds the line comments from `gh api --paginate
// repos/…/pulls/N/comments`, which prints one JSON array per page.
func (r *Review) addLineComments(out []byte) error {
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var page []struct {
			ID   int64 `json:"id"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			Body         string `json:"body"`
			Path         string `json:"path"`
			Line         int    `json:"line"`
			OriginalLine int    `json:"original_line"`
		}
		if err := dec.Decode(&page); err != nil {
			return fmt.Errorf("gh api: %w", err)
		}
		for _, c := range page {
			r.Feedback = append(r.Feedback, Feedback{ID: fmt.Sprintf("line:%d", c.ID), Author: c.User.Login, Body: c.Body, Path: c.Path, Line: cmp.Or(c.Line, c.OriginalLine)})
		}
	}
	return nil
}
