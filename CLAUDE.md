# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

grove is a terminal UI (Go, Bubble Tea v2) that shows every git worktree of a repository, the files each changed with +/- counts, a syntax-highlighted diff, and files changed in more than one worktree ("overlaps"). Module path: `github.com/azeem-yousaf/grove`. Primary platform is Windows; macOS and Linux are supported.

## Commands

```powershell
go build -o grove.exe .                      # local build
go install -ldflags "-s -w -X main.version=0.1.0-$(git rev-parse --short HEAD)" .   # install to %USERPROFILE%\go\bin
go vet ./...
gofmt -l .
go test ./... -count=1
go test ./internal/ui -run TestRemoveDialog -count=1    # single test
grove <repo> --plain                         # text summary; quickest end-to-end check of the git layer
```

go.mod requires Go 1.26; an older installed Go downloads that toolchain automatically. `main.version` is only set through `-ldflags`, so a plain `go install` reports `dev`.

Opt-in tests, skipped unless their environment variable is set:
- `GROVE_DEMO=<repo> GROVE_DUMP=<dir> go test ./internal/ui -run Dump` renders a real repository to `.txt`/`.ans` screen dumps for visual checks.
- `GROVE_SHOT=docs/screenshot.svg go test ./internal/ui -run ReadmeShot` regenerates the README screenshot. Rerun it after visible UI changes. The README uses an SVG because GitHub's code font lacks glyphs like ⚠ ● ■ ▾, which misaligns text screenshots.

## Architecture

`main.go` parses flags (they may come before or after the path), calls `git.Open`, then either prints `--plain` output or runs `ui.New` in a `tea.Program`.

### internal/git: everything goes through the real git CLI

- `run`/`runCodes` in `git.go` are the only way git is executed. They add `-C dir --no-pager -c core.quotepath=off -c color.ui=never` and set `GIT_OPTIONAL_LOCKS=0`, `GIT_TERMINAL_PROMPT=0` and `LC_ALL=C`. `GIT_OPTIONAL_LOCKS=0` matters: background scans must never take `index.lock` from the user's own git commands. Output is parsed from `-z` / porcelain formats.
- `Repo{Here, CommonDir, Name}`: `Here` is the worktree grove was launched from (it can be `""`, e.g. for a bare repo or after that worktree was removed). Commands that are not about a specific worktree use `CommonDir`. `Open` accepts a worktree, any subfolder or file, a `.git` folder, `.git/worktrees/<name>`, or a bare repo.
- `Scan` → `Snapshot`: `Worktrees` lists the worktrees, and `Inspect` runs for each one in parallel and produces a `State`. `Snapshot.Overlaps` maps a repo-relative path to the indexes of the states that touch it.
- Modes: `ModeBranch` diffs against the merge-base with the base branch (`State.Rev`) and merges in `git status` (uncommitted and untracked files). `ModeUncommitted` diffs against `HEAD`. `FileChange.Uncommitted` marks files with working-tree edits in either mode.
- `DetectBase`: an explicit override wins; otherwise `origin/HEAD`, then `main`/`master`/`trunk`/`develop`, each local before `origin/`.
- `RemoveWorktree` handles the Windows case where git forgets the worktree but cannot delete a folder another process holds open (`FolderLeft`). It also `chdir`s out of the target, because child processes inherit grove's working directory.

### internal/ui: one Bubble Tea model

- All git work runs in `tea.Cmd`s and comes back as messages (`scanMsg`, `diffMsg`, `removeMsg`, `gitDoneMsg`, `actionMsg`). Stale results are dropped by sequence number: `scanSeq` for scans and `diffSeq` for the 60 ms diff debounce. Diffs are cached by `diffKey` (path + rev + file), and the cache is cleared on every scan.
- Refresh is polling, not file watching: every 3 s while focused, slower when the terminal is blurred, and immediately on `FocusMsg`. File watchers were avoided because a watched folder on Windows can block deleting it. While a removal is running (`m.removing`), `startScan` is a no-op; a removal requested during a scan is queued and started from `onScan`.
- `Update` runs `ensureCursorVisible` and `refreshDiffView` after every message. `refreshDiffView` re-renders the viewport only when its `renderedFor` cache key changes.
- The selection survives rescans through `selKey` (worktree path + NUL + file path) and `rebuildRows`/`findRow`, not by row index.
- Key routing order in `onKey`: remove dialog (`m.confirm`) → new-worktree dialog (`m.creating`) → `:` git prompt → help overlay → filter input → global keys → pane-specific keys (`onListKey` / `onDiffKey`). New modal states need a branch here, an overlay case in `render()`, and a matching footer case in `view.go`. Keys that mean something else in the diff pane (such as `n`, next hunk) belong in `onListKey`, not the global switch.
- Overlays (help, remove confirmation) are composited over the screen in `render()` and clipped to the terminal. Layout switches to one pane at a time below 100 columns, and the minimum size is 50×12.
- `:` (`gitcmd.go`) runs git through `tea.Exec` with a custom `ExecCommand` (`gitRun`). Git gets the real terminal, so editors, pagers and prompts work. `gitRun` waits for Enter before grove redraws.
- Every colour comes from `theme.go` (`palette` / `styles`), with separate dark and light sets. `fill()` re-applies a row's background after each ANSI reset so styled segments don't punch holes in highlighted rows. Diff syntax highlighting uses Chroma (`catppuccin-mocha` / `github`).

### internal/open and internal/config

- `open` picks per-OS implementations through `open_windows.go` / `open_darwin.go` / `open_other.go`: `ShellExecute` on Windows (falling back to the "Open with" dialog when no program is associated), `open` on macOS, `xdg-open` elsewhere. On Windows, `.ts`/`.mts`/`.cts` go to the editor instead of the system default, because Windows often maps `.ts` to video.
- `config` reads optional TOML from `os.UserConfigDir()/grove/config.toml` (`grove --init-config` writes a starter).

## Tests

- UI tests drive the model directly with no terminal: `fakeModel` builds a hand-made `Snapshot`, `press()` sends keys, and `plain()` renders with ANSI stripped. `TestRenderFitsScreen` checks every screen at several sizes; add new overlays or screens to it.
- Git tests build real temporary repositories (`testRepo`: main plus `feature/a` and `feature/b` worktrees that both touch `shared.txt`) and skip if git is missing.

## Gotchas

- Write Go source with the Write/Edit tools, not shell heredocs. Bash in this environment turns `\x00` and `\n` inside heredoc text into real NUL bytes and newlines, which corrupts Go string literals.
- Commit messages in this repo have no `Co-Authored-By` trailer.
