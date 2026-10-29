package git

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Upstream is where a worktree's branch is pushed and how far apart they
// are.
type Upstream struct {
	Name          string // e.g. "origin/feature/a"; "" when the branch has none
	Remote        string // remote a push goes to; "" when the repository has none
	Ahead, Behind int    // commits not pushed yet, and commits on Name not here
}

// UpstreamOf reports the upstream of the branch checked out in dir. Without
// one, Remote is the remote a first push would use: origin, else the only
// remote there is.
func UpstreamOf(ctx context.Context, dir string) (Upstream, error) {
	var up Upstream
	out, err := run(ctx, dir, "remote")
	if err != nil {
		return up, err
	}
	remotes := splitLines(out)
	if out, err := runCodes(ctx, dir, []int{128}, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); err == nil {
		up.Name = strings.TrimSpace(string(out))
	}
	if up.Name == "" {
		switch {
		case slices.Contains(remotes, "origin"):
			up.Remote = "origin"
		case len(remotes) == 1:
			up.Remote = remotes[0]
		}
		return up, nil
	}
	up.Remote = remoteOf(up.Name, remotes)
	out, err = run(ctx, dir, "rev-list", "--left-right", "--count", "@{upstream}...HEAD")
	if err != nil {
		return up, err
	}
	if f := strings.Fields(string(out)); len(f) == 2 {
		up.Behind, _ = strconv.Atoi(f[0])
		up.Ahead, _ = strconv.Atoi(f[1])
	}
	return up, nil
}

// RemoteBranch splits a remote-tracking ref such as "origin/main" into its
// remote and branch, given the repository's remotes. ok is false for a
// local branch.
func RemoteBranch(ctx context.Context, dir, ref string) (remote, branch string, ok bool) {
	out, err := run(ctx, dir, "remote")
	if err != nil {
		return "", "", false
	}
	remote = remoteOf(ref, splitLines(out))
	if remote == "" || !refExists(ctx, dir, "refs/remotes/"+ref) {
		return "", "", false
	}
	return remote, strings.TrimPrefix(ref, remote+"/"), true
}

// remoteOf is the longest remote name that ref starts with, followed by a
// slash. Remote names may contain slashes themselves.
func remoteOf(ref string, remotes []string) string {
	best := ""
	for _, r := range remotes {
		if strings.HasPrefix(ref, r+"/") && len(r) > len(best) {
			best = r
		}
	}
	return best
}

// writePathspec writes paths to a temporary file for git's
// --pathspec-from-file with --pathspec-file-nul, which keeps long lists off
// the command line. The caller removes the file.
func writePathspec(paths []string) (string, error) {
	f, err := os.CreateTemp("", "grove-paths-*")
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(strings.Join(paths, "\x00") + "\x00")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// CommitPlan is the git commands that commit exactly some paths of a
// worktree. Run them in order, then call Cleanup.
type CommitPlan struct {
	Add    []string // stage the paths still on disk, new files included; nil when none
	Remove []string // stage the deletion of paths gone from disk; nil when none
	Commit []string
	files  []string
}

// Cleanup removes the pathspec files the commands read.
func (p CommitPlan) Cleanup() {
	for _, f := range p.files {
		os.Remove(f)
	}
}

// PlanCommit commits paths (relative to dir) with message, and nothing
// else: anything else that was staged stays staged. Pathspecs go through
// files, keeping long lists off the command line, and are literal, so
// names like [id].tsx are never read as patterns. A path gone from disk
// can't be added: its deletion is staged with rm --cached, which also
// accepts one already staged, like the old name of a git mv. During a
// merge git refuses partial commits, so the whole merge is committed.
func PlanCommit(dir string, paths []string, message string, merging bool) (CommitPlan, error) {
	var p CommitPlan
	var present, gone []string
	for _, path := range paths {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(path))); err == nil {
			present = append(present, path)
		} else {
			gone = append(gone, path)
		}
	}
	from := func(paths []string) ([]string, error) {
		spec, err := writePathspec(paths)
		if err != nil {
			return nil, err
		}
		p.files = append(p.files, spec)
		return []string{"--pathspec-from-file=" + spec, "--pathspec-file-nul"}, nil
	}
	if len(present) > 0 {
		args, err := from(present)
		if err != nil {
			p.Cleanup()
			return CommitPlan{}, err
		}
		p.Add = append([]string{"--literal-pathspecs", "add", "--all"}, args...)
	}
	if len(gone) > 0 {
		args, err := from(gone)
		if err != nil {
			p.Cleanup()
			return CommitPlan{}, err
		}
		p.Remove = append([]string{"--literal-pathspecs", "rm", "--cached", "--quiet", "--ignore-unmatch"}, args...)
	}
	p.Commit = []string{"--literal-pathspecs", "commit", "-m", message}
	if !merging && len(paths) > 0 {
		args, err := from(paths)
		if err != nil {
			p.Cleanup()
			return CommitPlan{}, err
		}
		p.Commit = append(p.Commit, args...)
	}
	return p, nil
}

// MergeMessage is the message git prepared for the merge in progress in
// dir, without its comment lines, or "".
func MergeMessage(ctx context.Context, dir string) string {
	out, err := run(ctx, dir, "rev-parse", "--git-path", "MERGE_MSG")
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var keep []string
	for l := range strings.SplitSeq(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(l, "#") {
			keep = append(keep, l)
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}
