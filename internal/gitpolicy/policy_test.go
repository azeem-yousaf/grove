package gitpolicy

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLookup answers from fixed tables.
type fakeLookup struct {
	branches map[string]string // dir → branch ("" detached)
	aliases  map[string]string
	refs     map[string]bool
	pushed   map[string]bool
}

func (f fakeLookup) CurrentBranch(dir string) (string, error) {
	for d, b := range f.branches {
		if inside(dir, d) {
			return b, nil
		}
	}
	return "", errors.New("not a repository")
}

func (f fakeLookup) Alias(name string) (string, bool) {
	v, ok := f.aliases[name]
	return v, ok
}

func (f fakeLookup) RefExists(ref string) bool { return f.refs[ref] }

func (f fakeLookup) Pushed(branch string) bool { return f.pushed[branch] }

type fixture struct {
	root                        string
	wt, main, mate, task, else_ string
}

func newFixture(t *testing.T) fixture {
	root := t.TempDir()
	return fixture{
		root:  root,
		wt:    filepath.Join(root, "app-feature"),
		main:  filepath.Join(root, "app"),
		mate:  filepath.Join(root, "app-mate"),
		task:  filepath.Join(root, "app-task"),
		else_: filepath.Join(root, "elsewhere"),
	}
}

func (f fixture) scope() Scope {
	return Scope{
		Worktree:  f.wt,
		Branch:    "feature/x",
		Readable:  []string{f.main, f.mate},
		Protected: []string{"main", "master", "release/*"},
		Guarded:   true,
	}
}

func (f fixture) lookup() fakeLookup {
	return fakeLookup{
		branches: map[string]string{
			f.wt:   "feature/x",
			f.main: "master",
			f.mate: "feature/mate",
			f.task: "feature/task",
		},
		aliases: map[string]string{"st": "status", "yolo": "push -f origin master"},
		refs: map[string]bool{
			"refs/heads/master": true, "refs/heads/main": true, "refs/heads/feature/x": true,
			"refs/heads/feature/y": true, "refs/heads/feature/pushed": true, "refs/heads/release/1.0": true,
			"refs/remotes/origin/feature/remote": true, "HEAD~1": true, "HEAD": true, "abc123": true,
			"origin/master": true, "v1.0": true,
		},
		pushed: map[string]bool{"feature/pushed": true, "master": true},
	}
}

type tc struct {
	cmd         string
	want        Verdict
	protected   bool
	destructive bool
}

func check(t *testing.T, sh Shell, sc Scope, lk Lookup, cases []tc) {
	t.Helper()
	for _, c := range cases {
		d := Classify(c.cmd, sh, sc, lk)
		if d.Verdict != c.want || c.protected && !d.Protected || c.destructive && !d.Destructive {
			t.Errorf("%s: %q = %v (%q, protected %v, destructive %v), want %v protected %v destructive %v",
				sh, c.cmd, d.Verdict, d.Reason, d.Protected, d.Destructive, c.want, c.protected, c.destructive)
		}
		if d.Verdict == User && d.Reason == "" {
			t.Errorf("%s: %q needs the user but gives no reason", sh, c.cmd)
		}
	}
}

// both runs the same cases in Bash and PowerShell.
func both(t *testing.T, sc Scope, lk Lookup, cases []tc) {
	t.Helper()
	check(t, Bash, sc, lk, cases)
	check(t, PowerShell, sc, lk, cases)
}

func TestRoutineOnFeatureBranch(t *testing.T) {
	f := newFixture(t)
	both(t, f.scope(), f.lookup(), []tc{
		{"git status", Allow, false, false},
		{"git diff --stat master...HEAD", Allow, false, false},
		{"git log --oneline -5", Allow, false, false},
		{"git show HEAD:README.md", Allow, false, false},
		{"git branch", Allow, false, false},
		{"git branch -a --contains abc123", Allow, false, false},
		{"git branch --show-current", Allow, false, false},
		{"git tag -l", Allow, false, false},
		{"git remote -v", Allow, false, false},
		{"git stash list", Allow, false, false},
		{"git worktree list --porcelain", Allow, false, false},
		{"git config --get user.name", Allow, false, false},
		{"git config user.email", Allow, false, false},
		{"git reflog show", Allow, false, false},
		{"git rev-parse --abbrev-ref HEAD", Allow, false, false},
		{"git ls-files -z", Allow, false, false},
		{"git grep -n TODO", Allow, false, false},
		{"git merge-base HEAD master", Allow, false, false},
		{"git cat-file -p HEAD", Allow, false, false},
		{"git --version", Allow, false, false},
		{"git add -A", Allow, false, false},
		{"git add internal/x.go", Allow, false, false},
		{"git rm --cached old.txt", Allow, false, false},
		{"git mv a.go b.go", Allow, false, false},
		{"git commit -m 'Add the thing'", Allow, false, false},
		{"git commit -am \"Fix it\"", Allow, false, false},
		{"git commit --amend --no-edit", Allow, false, false},
		{"git switch feature/y", Allow, false, false},
		{"git switch -c feature/new", Allow, false, false},
		{"git checkout -b feature/new2 master", Allow, false, false},
		{"git checkout feature/y", Allow, false, false},
		{"git checkout feature/remote", Allow, false, false},
		{"git checkout --detach", Allow, false, false},
		{"git branch feature/z", Allow, false, false},
		{"git branch -m feature/renamed", Allow, false, false},
		{"git branch -d feature/y", Allow, false, false},
		{"git merge master", Allow, false, false},
		{"git merge --no-ff origin/master -m 'Merge'", Allow, false, false},
		{"git merge --abort", Allow, false, false},
		{"git rebase master", Allow, false, false},
		{"git rebase --continue", Allow, false, false},
		{"git cherry-pick abc123", Allow, false, false},
		{"git revert --no-edit abc123", Allow, false, false},
		{"git stash", Allow, false, false},
		{"git stash push -u -m wip", Allow, false, false},
		{"git stash show -p", Allow, false, false},
		{"git fetch", Allow, false, false},
		{"git fetch origin", Allow, false, false},
		{"git fetch origin master", Allow, false, false},
		{"git fetch --prune --all", Allow, false, false},
		{"git pull", Allow, false, false},
		{"git pull --rebase origin master", Allow, false, false},
		{"git push", Allow, false, false},
		{"git push -u origin HEAD", Allow, false, false},
		{"git push origin feature/x", Allow, false, false},
		{"git push origin feature/y", Allow, false, false},
		{"git push origin HEAD:feature/x", Allow, false, false},
		{"git push --dry-run origin master", Allow, false, false},
		{"git reset", Allow, false, false},
		{"git reset HEAD file.txt", Allow, false, false},
		{"git reset -- file.txt", Allow, false, false},
		{"git reset --soft HEAD~1", Allow, false, false},
		{"git restore --staged file.txt", Allow, false, false},
		{"git clean -n", Allow, false, false},
		{"git apply --check fix.patch", Allow, false, false},
		{"git apply fix.patch", Allow, false, false},
		{"git -C sub status", Allow, false, false},
		{"git --no-pager log -1", Allow, false, false},
		{"git format-patch --stdout -1", Allow, false, false},
		{"git branch --set-upstream-to=origin/feature/x", Allow, false, false},
	})
}

func TestProtected(t *testing.T) {
	f := newFixture(t)
	sc, lk := f.scope(), f.lookup()
	both(t, sc, lk, []tc{
		{"git switch master", User, true, false},
		{"git checkout master", User, true, false},
		{"git checkout main", User, true, false},
		{"git checkout -b master", User, true, false},
		{"git switch -c release/2.0", User, true, false},
		{"git checkout refs/heads/master", User, true, false},
		{"git checkout heads/master", User, true, false},
		{"git checkout Master", User, true, false},
		{"git fetch origin master:master", User, true, false},
		{"git fetch origin +refs/heads/master:refs/heads/master", User, true, false},
		{"git pull origin master:master", User, true, false},
		{"git push origin master", User, true, false},
		{"git push origin HEAD:master", User, true, false},
		{"git push origin HEAD:refs/heads/master", User, true, false},
		{"git push origin feature/x:release/1.0", User, true, false},
		{"git push origin :master", User, true, true},
		{"git push --delete origin master", User, true, true},
		{"git push origin +feature/x:master", User, true, true},
		{"git branch -f master abc123", User, true, true},
		{"git branch -D master", User, true, true},
		{"git branch -d main", User, true, true},
		{"git branch -m master old-master", User, true, false},
		{"git branch -M feature/x master", User, true, false},
		{"git update-ref refs/heads/master abc123", User, true, false},
		{"git update-ref -d refs/heads/master", User, true, true},
		{"git symbolic-ref HEAD refs/heads/master", User, true, false},
		{"git rebase master release/1.0", User, true, false},
		{"git -C ../app commit -m x", User, false, false},
		{"git -C ../app merge feature/x", User, false, false},
		{"git -C ../app-mate commit -m x", User, false, false},
	})

	// On master itself, HEAD resolves to master.
	lk.branches[f.wt] = "master"
	both(t, sc, lk, []tc{
		{"git commit -m x", User, true, false},
		{"git commit --amend -m x", User, true, false},
		{"git merge feature/x", User, true, false},
		{"git rebase feature/x", User, true, false},
		{"git cherry-pick abc123", User, true, false},
		{"git revert abc123", User, true, false},
		{"git am fix.patch", User, true, false},
		{"git pull", User, true, false},
		{"git reset HEAD~1", User, true, false},
		{"git reset --hard origin/master", User, true, true},
		{"git push", User, true, false},
		{"git push origin HEAD", User, true, false},
		{"git push -u origin @", User, true, false},
		{"git add .", User, true, false},
		{"git branch -m renamed", User, true, false},
		{"git status", Allow, false, false},
		{"git switch feature/y", Allow, false, false},
		{"git switch -c feature/new", Allow, false, false},
		{"git fetch", Allow, false, false},
	})
}

func TestDestructive(t *testing.T) {
	f := newFixture(t)
	both(t, f.scope(), f.lookup(), []tc{
		{"git reset --hard", User, false, true},
		{"git reset --hard HEAD~1", User, false, true},
		{"git reset --keep HEAD~1", User, false, true},
		{"git reset --merge", User, false, true},
		{"git checkout -- file.txt", User, false, true},
		{"git checkout .", User, false, true},
		{"git checkout HEAD file.txt", User, false, true},
		{"git checkout no-such-thing", User, false, true},
		{"git checkout -f feature/y", User, false, true},
		{"git restore file.txt", User, false, true},
		{"git restore --staged --worktree file.txt", User, false, true},
		{"git clean -fd", User, false, true},
		{"git clean -fdx", User, false, true},
		{"git stash drop", User, false, true},
		{"git stash clear", User, false, true},
		{"git branch -D feature/y", User, false, true},
		{"git branch -d -f feature/y", User, false, true},
		{"git branch -f feature/y abc123", User, false, true},
		{"git worktree remove ../app-mate", User, false, true},
		{"git worktree prune", User, false, true},
		{"git push -f", User, false, true},
		{"git push --force origin feature/x", User, false, true},
		{"git push --force-with-lease origin feature/x", User, false, true},
		{"git push --forc origin feature/x", User, false, true},
		{"git push origin +feature/x", User, false, true},
		{"git push origin :feature/y", User, false, true},
		{"git push --delete origin feature/y", User, false, true},
		{"git push --mirror", User, false, true},
		{"git push --prune origin", User, false, true},
		{"git gc --prune=now", User, false, true},
		{"git reflog expire --expire=now --all", User, false, true},
		{"git reflog delete 'HEAD@{1}'", User, false, true},
		{"git filter-branch --tree-filter 'rm x' HEAD", User, false, true},
		{"git replace abc123 def456", User, false, true},
		{"git rm -f file.txt", User, false, true},
		{"git switch --discard-changes feature/y", User, false, true},
		{"git switch -C feature/y", User, false, true},
		{"git checkout -B feature/y", User, false, true},
		{"git rebase feature/x feature/pushed", User, false, true},
		{"git fetch origin +other:feature/y", User, false, true},
	})
}

func TestOtherNeedsUser(t *testing.T) {
	f := newFixture(t)
	both(t, f.scope(), f.lookup(), []tc{
		{"git config user.name x", User, false, false},
		{"git config --unset user.name", User, false, false},
		{"git config --global alias.co checkout", User, false, false},
		{"git -c alias.st=status st", User, false, false},
		{"git -c core.hooksPath=/dev/null commit -m x", User, false, false},
		{"git -c core.worktree=../app status", User, false, false},
		{"git --git-dir=../app/.git log", User, false, false},
		{"git --work-tree=../app status", User, false, false},
		{"git --namespace=x push", User, false, false},
		{"git --exec-path=/tmp status", User, false, false},
		{"git st", User, false, false},
		{"git yolo", User, false, false},
		{"git frobnicate", User, false, false},
		{"git lfs pull", User, false, false},
		{"git log --output=/tmp/x", User, false, false},
		{"git diff --output=x.diff", User, false, false},
		{"git format-patch -o out -3", User, false, false},
		{"git format-patch -1", User, false, false},
		{"git grep -O TODO", User, false, false},
		{"git rebase -i HEAD~3", User, false, false},
		{"git rebase -x 'make test' master", User, false, false},
		{"git rebase --update-refs master", User, false, false},
		{"git commit --amend -m x", Allow, false, false},
		{"git tag v2.0", User, false, false},
		{"git push --tags", User, false, false},
		{"git push origin v1.0", User, false, false},
		{"git push --no-verify", User, false, false},
		{"git push --no-verify origin feature/x", User, false, false},
		{"git push --no-verif origin feature/x", User, false, false},
		{"git commit --no-verify -m x", User, false, false},
		{"git commit -n -m x", User, false, false},
		{"git commit -nm x", User, false, false},
		{"git merge --no-verify master", User, false, false},
		{"git --config-env=core.hooksPath=HOOKS push", User, false, false},
		{"git -c core.hooksPath= push", User, false, false},
		{"git push origin refs/heads/*:refs/heads/*", User, false, false},
		{"git push --all", User, false, false},
		{"git remote add up https://x", User, false, false},
		{"git worktree add ../x", User, false, false},
		{"git switch -", User, false, false},
		{"git checkout -", User, false, false},
		{"git checkout @{-1}", User, false, false},
		{"git branch -m feature/y feature/q", User, false, false},
		{"git update-ref refs/heads/feature/y abc123", User, false, false},
		{"git fetch --upload-pack=evil origin", User, false, false},
		{"git add -p", User, false, false},
		{"git stash pop", User, false, false},
		{"git stash apply", User, false, false},
		{"git stash branch feature/new", User, false, false},
		{"git init", User, false, false},
		{"git clone https://x", User, false, false},
		{"git-receive-pack .", User, false, false},
		{"git status --bogus=x", Allow, false, false},
		{"git --bogus status", User, false, false},
		{"git -C ../elsewhere status", User, false, false},
		{"git -C ../app status", Allow, false, false},
		{"git -C ../app-mate log", Allow, false, false},
	})

	// Rewriting pushed history.
	lk := f.lookup()
	lk.branches[f.wt] = "feature/pushed"
	both(t, f.scope(), lk, []tc{
		{"git commit --amend --no-edit", User, false, false},
		{"git rebase master", User, false, true},
		{"git reset --soft HEAD~1", User, false, false},
		{"git branch -m feature/new", User, false, false},
		{"git commit -m x", Allow, false, false},
		{"git push", Allow, false, false},
	})
}

func TestUnguarded(t *testing.T) {
	f := newFixture(t)
	sc := f.scope()
	sc.Guarded = false
	both(t, sc, f.lookup(), []tc{
		{"git status", Allow, false, false},
		{"git log -3", Allow, false, false},
		{"git commit -m x", User, false, false},
		{"git push", User, false, false},
		{"git add .", User, false, false},
		{"git fetch", User, false, false},
	})
	d := Classify("git commit -m x", Bash, sc, f.lookup())
	if d.Reason != "git guard not active in this worktree" {
		t.Errorf("reason %q", d.Reason)
	}
}

func TestWritableAndCd(t *testing.T) {
	f := newFixture(t)
	sc := f.scope()
	sc.Writable = []string{f.task}
	both(t, sc, f.lookup(), []tc{
		{"git -C ../app-task commit -m x", Allow, false, false},
		{"git -C ../app-task merge feature/x", Allow, false, false},
		{"cd ../app-task && git commit -m x", Allow, false, false},
		{"cd ../app && git commit -m x", User, true, false},
		{"cd ../app && git status", Allow, false, false},
		{"cd ../elsewhere && git status", User, false, false},
		{"cd ../app-mate && git commit -m x", User, false, false},
		{"cd sub; git add .", Allow, false, false},
		{"cd; git status", User, false, false},
		{"cd - && git status", User, false, false},
		{"pushd ../app-task && git push", Allow, false, false},
	})
	check(t, Bash, sc, f.lookup(), []tc{
		{"cd ~ && git status", User, false, false},
		{"cd $DIR && git status", User, false, false},
		{"cd ../app | git commit -m x", Allow, false, false},
	})
	check(t, PowerShell, sc, f.lookup(), []tc{
		{"Set-Location ../app-task; git commit -m x", Allow, false, false},
		{"Set-Location -Path ../app; git commit -m x", User, true, false},
		{"sl $dir; git status", User, false, false},
	})

	// Starting somewhere else.
	sc.Dir = f.task
	both(t, sc, f.lookup(), []tc{
		{"git commit -m x", Allow, false, false},
	})
	sc.Dir = f.main
	both(t, sc, f.lookup(), []tc{
		{"git commit -m x", User, true, false},
		{"git log", Allow, false, false},
	})
}

func TestChains(t *testing.T) {
	f := newFixture(t)
	both(t, f.scope(), f.lookup(), []tc{
		{"git status && git push origin master", User, true, false},
		{"x; git reset --hard", User, false, true},
		{"go test ./... && git add -A && git commit -m 'Tests pass'", Allow, false, false},
		{"git add . ; git commit -m x || echo failed", Allow, false, false},
		{"git log --oneline | head -5", Allow, false, false},
		{"git status\ngit push -f", User, false, true},
		{"echo hi & git push origin master", User, true, false},
		{"git switch feature/y && git commit -m x", Allow, false, false},
		{"git switch -c tmp && git branch -f master tmp", User, true, true},
		{"git diff | grep x", Allow, false, false},
		{"git status 2>&1", Allow, false, false},
		{"git log > log.txt", Allow, false, false},
	})
}

func TestShellTricks(t *testing.T) {
	f := newFixture(t)
	sc, lk := f.scope(), f.lookup()
	check(t, Bash, sc, lk, []tc{
		{"echo $(git rev-parse HEAD)", User, false, false},
		{"git log $(git merge-base HEAD master)..", User, false, false},
		{"git checkout `git rev-parse master`", User, false, false},
		{"echo `git status`", User, false, false},
		{"(git push origin master)", User, false, false},
		{"(cd ../app && git commit -m x)", User, false, false},
		{"{ git status; }", User, false, false},
		{"eval git push origin master", User, false, false},
		{"eval \"$CMD\"", NotGit, false, false},
		{"bash -c 'git push origin master'", User, false, false},
		{"sh -c \"git status\"", User, false, false},
		{"echo git push origin master | sh", User, false, false},
		{"bash <<EOF\ngit push origin master\nEOF", User, false, false},
		{"git branch | xargs git branch -D", User, false, false},
		{"find . -name x -exec git rm {} \\;", User, false, false},
		{"python3 -c 'import os; os.system(\"git push\")'", User, false, false},
		{"GIT_DIR=../app/.git git log", User, false, false},
		{"GIT_WORK_TREE=../app git status", User, false, false},
		{"GIT_CONFIG_GLOBAL=/tmp/x git st", User, false, false},
		{"GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.hooksPath GIT_CONFIG_VALUE_0= git push", User, false, false},
		{"GIT_CONFIG_PARAMETERS=\"'core.hooksPath'=''\" git push", User, false, false},
		{"export GIT_CONFIG_COUNT=1; git push", User, false, false},
		{"export GIT_DIR=../app/.git; git commit -m x", User, false, false},
		{"GIT_DIR=x; export GIT_DIR; git log", User, false, false},
		{"env GIT_DIR=x git log", User, false, false},
		{"env -i git log", User, false, false},
		{"LC_ALL=C git status", Allow, false, false},
		{"PATH=/tmp:$PATH git status", User, false, false},
		{"alias g=git; g push", User, false, false},
		{"g=git; $g push origin master", User, false, false},
		{"git $SUB", User, false, false},
		{"git push origin $BRANCH", User, false, false},
		{"git push origin {feature/x,master}", User, false, false},
		{"git push origin feat*", User, false, false},
		{"git add *.go", Allow, false, false},
		{"command git status", Allow, false, false},
		{"command git push origin master", User, true, false},
		{"\\git status", Allow, false, false},
		{"\\git push origin master", User, true, false},
		{"/usr/bin/git push origin master", User, true, false},
		{"git.exe push origin master", User, true, false},
		{"nohup git push origin master", User, true, false},
		{"time git commit -m x", Allow, false, false},
		{"command -v git", NotGit, false, false},
		{"echo x > .git/HEAD", User, false, false},
		{"echo x >> .git/refs/heads/master", User, false, false},
		{"cp evil .git/hooks/pre-push", User, false, false},
		{"rm -rf .git", User, false, false},
		{"cat .git/HEAD", NotGit, false, false},
		{"echo 'git push origin master'", NotGit, false, false},
		{"git commit -m 'it'\"'\"'s done'", Allow, false, false},
		{"git commit -m \"$(cat <<'EOF'\nFix the parser\n\nMore text with $dollar.\nEOF\n)\"", Allow, false, false},
		{"git commit -m \"$(cat <<EOF\nPlain message\nEOF\n)\"", Allow, false, false},
		{"git commit -m \"$(cat <<EOF\nUses $HOME\nEOF\n)\"", User, false, false},
		{"git commit -m \"$(date)\"", User, false, false},
		{"git commit -m 'unterminated", User, false, false},
		{"echo 'unterminated", NotGit, false, false},
		{"git status # git push origin master", Allow, false, false},
		{"git push origin \\\n  master", User, true, false},
		{"git push origin 'mas''ter'", User, true, false},
		{"git push origin \"mas\"ter", User, true, false},
		{"diff <(git show HEAD:a) a", User, false, false},
		{"f() { git push origin master; }; f", User, false, false},
		{"sudo git status", User, false, false},
		{"if git diff --quiet; then git commit -m x; fi", Allow, false, false},
		{"if git diff --quiet; then git push -f; fi", User, false, true},
		{"for b in a c; do git push origin $b; done", User, false, false},
		{"! git diff --quiet && git commit -am x", Allow, false, false},
	})
	check(t, PowerShell, sc, lk, []tc{
		{"echo $(git rev-parse HEAD)", User, false, false},
		{"git log (git merge-base HEAD master)", User, false, false},
		{"$x = git push origin master", User, true, false},
		{"$x = git status", Allow, false, false},
		{"$env:GIT_DIR = '../app/.git'; git log", User, false, false},
		{"${env:GIT_WORK_TREE}='x'; git status", User, false, false},
		{"$env:LC_ALL = 'C'; git status", Allow, false, false},
		{"$env:GIT_CONFIG_COUNT = 1; git push", User, false, false},
		{"$env:GIT_CONFIG_PARAMETERS = \"'core.hooksPath'=''\"; git push", User, false, false},
		{"Set-Item env:GIT_DIR x; git status", User, false, false},
		{"Invoke-Expression 'git push origin master'", User, false, false},
		{"iex \"git status\"", User, false, false},
		{"pwsh -Command \"git push\"", User, false, false},
		{"powershell -c 'git push'", User, false, false},
		{"cmd /c git push", User, false, false},
		{"Start-Process git -ArgumentList 'push'", User, false, false},
		{"1..3 | ForEach-Object { git push }", User, false, false},
		{"& git push origin master", User, true, false},
		{"& 'C:\\Program Files\\Git\\cmd\\git.exe' push origin master", User, true, false},
		{"& \"C:\\Program Files\\Git\\cmd\\git.exe\" status", Allow, false, false},
		{"git push origin `master", User, true, false},
		{"git push origin \"ma`ster\"", User, true, false},
		{"git push origin \"mas`ter\"", User, false, false}, // `t is a tab: "mas\tter"
		{"git push origin 'feature/x',master", User, true, false},
		{"git push origin $branch", User, false, false},
		{"git push origin @args", User, false, false},
		{"git push origin \"$(Get-Branch)\"", User, false, false},
		{"git push origin “master”", User, true, false},
		{"git push --% origin master", User, false, false},
		{"git status; git push -f", User, false, true},
		{"git status && git push origin master", User, true, false},
		{"Set-Alias g git; g push", User, false, false},
		{"function g { git push }; g", User, false, false},
		{"git commit -m @'\nMulti-line\nmessage\n'@", Allow, false, false},
		{"git commit -m @\"\nHi $name\n\"@", User, false, false},
		{"Get-Content x | git apply", Allow, false, false},
		{"Remove-Item -Recurse .git", User, false, false},
		{"Get-Content .git/HEAD", NotGit, false, false},
		{"git status <# git push -f #>", Allow, false, false},
		{"if (git diff --quiet) { git commit -m x }", User, false, false},
	})
}

func TestNotGit(t *testing.T) {
	f := newFixture(t)
	both(t, f.scope(), f.lookup(), []tc{
		{"go test ./...", NotGit, false, false},
		{"ls -la", NotGit, false, false},
		{"npm run build", NotGit, false, false},
		{"echo hello; echo world", NotGit, false, false},
		{"cat .gitignore", NotGit, false, false},
		{"rm -rf node_modules", NotGit, false, false},
		{"gh pr list", NotGit, false, false},
		{"grep -r github .", NotGit, false, false},
		{"cd ../app", NotGit, false, false},
		{"", NotGit, false, false},
	})
}

func TestProtectedMatching(t *testing.T) {
	p := []string{"main", "refs/heads/master", "release/*", "heads/prod"}
	for _, b := range []string{"main", "refs/heads/main", "heads/main", "master", "release/1.0", "prod", "MAIN"} {
		if !isProtected(b, p) {
			t.Errorf("%s should be protected", b)
		}
	}
	for _, b := range []string{"feature/main", "mainline", "release", "", "origin/main/x"} {
		if isProtected(b, p) {
			t.Errorf("%s should not be protected", b)
		}
	}
}

func TestDecisionRefs(t *testing.T) {
	f := newFixture(t)
	d := Classify("git commit -m x && git push origin feature/x feature/y", Bash, f.scope(), f.lookup())
	if d.Verdict != Allow || strings.Join(d.Refs, ",") != "feature/x,feature/y" {
		t.Errorf("got %+v", d)
	}
	d = Classify("git push origin HEAD:master", Bash, f.scope(), f.lookup())
	if d.Reason != "pushes to protected branch master" || len(d.Refs) != 1 || d.Refs[0] != "master" {
		t.Errorf("got %+v", d)
	}
}

func TestBypassIsMarked(t *testing.T) {
	wt := t.TempDir()
	sc := Scope{Worktree: wt, Protected: []string{"main"}, Guarded: true}
	lk := fakeLookup{branches: map[string]string{wt: "feature/x"}}
	for cmd, want := range map[string]bool{
		"git -c core.hooksPath=/dev/null commit -m x": true,
		"git commit --no-verify -m x":                 true,
		"GIT_DIR=/other git commit -m x":              true,
		"git status && git commit -n -m x":            true,
		"git frobnicate":                              false,
		"git commit -m x":                             false,
	} {
		if d := Classify(cmd, Bash, sc, lk); d.Bypass != want {
			t.Errorf("%s: %+v, want Bypass %v", cmd, d, want)
		}
	}
}
