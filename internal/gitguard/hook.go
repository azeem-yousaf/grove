package gitguard

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/azeem-yousaf/grove/internal/fslock"
)

// grant is one update of a protected ref the user approved.
type grant struct {
	ID       string    `json:"id"`
	Ref      string    `json:"ref"`
	Worktree string    `json:"worktree,omitempty"` // "" matches any
	Expires  time.Time `json:"expires"`
}

// Grant lets one update of ref (a branch name or a full ref) from worktree
// through the guard within ttl. It covers one ref transaction, or one ref of
// a push, where ref is the remote branch. An empty worktree matches any.
func (g Guard) Grant(ref, worktree string, ttl time.Duration) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	gr := grant{ID: hex.EncodeToString(b[:]), Ref: fullRef(ref), Worktree: worktree, Expires: time.Now().Add(ttl)}
	err := g.updateGrants(func(list []grant) []grant { return append(list, gr) })
	if err != nil {
		return "", err
	}
	return gr.ID, nil
}

func fullRef(ref string) string {
	if strings.HasPrefix(ref, "refs/") {
		return ref
	}
	return "refs/heads/" + ref
}

// updateGrants rewrites grants.json under its lock, dropping expired grants.
func (g Guard) updateGrants(change func([]grant) []grant) error {
	unlock, err := fslock.Lock(filepath.Join(g.Dir, grantsLock))
	if err != nil {
		return err
	}
	defer unlock()
	p := filepath.Join(g.Dir, grantsFile)
	var list []grant
	if data, err := os.ReadFile(p); err == nil {
		json.Unmarshal(data, &list)
	}
	now := time.Now()
	list = slices.DeleteFunc(list, func(gr grant) bool { return !now.Before(gr.Expires) })
	list = change(list)
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeIfChanged(p, data, 0o644)
}

// consume takes one grant for each ref, all or none, and returns the refs
// left without one.
func (g Guard) consume(refs []string, worktree string) ([]string, error) {
	var missing []string
	err := g.updateGrants(func(list []grant) []grant {
		used := make([]bool, len(list))
		missing = nil
		for _, ref := range refs {
			found := false
			for i, gr := range list {
				if !used[i] && gr.Ref == ref && (gr.Worktree == "" || worktree != "" && samePath(gr.Worktree, worktree)) {
					used[i], found = true, true
					break
				}
			}
			if !found {
				missing = append(missing, ref)
			}
		}
		if len(missing) > 0 {
			return list
		}
		var keep []grant
		for i, gr := range list {
			if !used[i] {
				keep = append(keep, gr)
			}
		}
		return keep
	})
	return missing, err
}

// Hook is `grove git-guard <hook> --dir <guard folder> <git's arguments>`,
// which the guard's hook scripts run. It returns the exit status: 0 lets
// git go on.
func Hook(name string, args []string, stdin io.Reader, stderr io.Writer, env []string) int {
	fs := flag.NewFlagSet("git-guard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "the guard's hooks folder")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(stderr, "grove git-guard: --dir is missing")
		return 2
	}
	g := Guard{Dir: *dir}
	s := readSettings(g.Dir)
	var refs []string
	var err error
	switch name {
	case "reference-transaction":
		if fs.Arg(0) != "prepared" {
			io.Copy(io.Discard, stdin)
			return 0
		}
		refs = guardedUpdates(stdin, s.Protected)
	case "pre-push":
		refs, err = guardedPushes(stdin, s.Protected, env)
	default:
		fmt.Fprintf(stderr, "grove git-guard: unknown hook %q\n", name)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "grove:", err)
		return 1
	}
	if len(refs) == 0 {
		return 0
	}
	missing, err := g.consume(refs, toplevel(env))
	if err != nil {
		fmt.Fprintln(stderr, "grove: checking approvals:", err)
		return 1
	}
	for _, ref := range missing {
		fmt.Fprintln(stderr, refusal(ref))
	}
	if len(missing) > 0 {
		return 1
	}
	return 0
}

// protectedRef reports whether a full ref is guarded.
func protectedRef(ref string, protected []string) bool {
	if ref == ProbeRef {
		return true
	}
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		return false
	}
	for _, p := range protected {
		if m, err := path.Match(p, branch); m && err == nil || p == branch {
			return true
		}
	}
	return false
}

// guardedUpdates reads reference-transaction's "<old> <new> <ref>" lines
// and returns the protected refs among them.
func guardedUpdates(r io.Reader, protected []string) []string {
	var refs []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 3 && protectedRef(f[2], protected) && !slices.Contains(refs, f[2]) {
			refs = append(refs, f[2])
		}
	}
	return refs
}

// guardedPushes reads pre-push's "<local ref> <local sha> <remote ref>
// <remote sha>" lines and returns the remote refs that need a grant: those
// of protected branches, deletions and pushes that aren't fast-forwards.
func guardedPushes(r io.Reader, protected []string, env []string) ([]string, error) {
	var refs []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 4 {
			continue
		}
		local, remote, remoteSHA := f[1], f[2], f[3]
		need := protectedRef(remote, protected) || zero(local)
		if !need && !zero(remoteSHA) && known(remoteSHA, env) && !ancestor(remoteSHA, local, env) {
			need = true
		}
		if need && !slices.Contains(refs, remote) {
			refs = append(refs, remote)
		}
	}
	return refs, sc.Err()
}

func zero(sha string) bool { return strings.Trim(sha, "0") == "" }

func gitCmd(env []string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"--no-pager"}, args...)...)
	cmd.Env = append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	return cmd
}

func known(sha string, env []string) bool {
	return gitCmd(env, "cat-file", "-e", sha+"^{commit}").Run() == nil
}

func ancestor(a, b string, env []string) bool {
	return gitCmd(env, "merge-base", "--is-ancestor", a, b).Run() == nil
}

// toplevel is the worktree git runs the hook in (its working folder), or ""
// in a bare repository.
func toplevel(env []string) string {
	out, err := gitCmd(env, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
