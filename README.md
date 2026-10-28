# grove

**See every git worktree at once: what each one changed, and where they collide.**

grove is a terminal UI for working on several features in parallel with
`git worktree`. It lists every worktree in a repository side by side, with
per-file `+`/`−` counts, a syntax-highlighted diff, and a warning whenever two
worktrees touch the same file, which is where your merge conflicts will come
from.

```
 grove  shop  ·  changes since main                     ⚠ 1 overlap  ·  3 worktrees  ·  ● live · just now
╭─ Worktrees ─────────────── 6 files  +34 −4 ─╮╭─ src/api/client.ts ───────────────────────────── +3 −2 ─╮
│ ▾ ○ main  base                              ││ feature/auth · modified               enter open in code │
│     ~\…\shop · 4m ago                       ││ ⚠ Also changed in feature/cart +4 −0                     │
│     ✓ no uncommitted changes                ││──────────────────────────────────────────────────────────│
│                                             ││  1   1 │   import { retry } from './retry'               │
│ ▾ ● feature/auth  ◆ here          +19 −3    ││  2   2 │                                                 │
│     ~\…\shop-auth · 3 files · ↑1 · 4m ago   ││  3     │ - const TIMEOUT = 5000                          │
│     M src/api/client.ts      ⚠  +3 −2 ■■■·· ││      3 │ + const TIMEOUT = 10_000                        │
│     M src/auth/login.ts    ●    +6 −1 ■■■■· ││      4 │ + const MAX_RETRIES = 3                         │
│     ? src/auth/session.ts  ●   +10 −0 ■■■■■ ││  4   5 │                                                 │
│                                             ││  5   6 │   export async function get(path: string) {     │
│ ▾ ● feature/cart                  +15 −1    ││                                                          │
│     M src/api/client.ts    ● ⚠  +4 −0 ■■■·· ││                                                          │
│     A src/cart/cart.ts          +9 −0 ■■■■■ ││                                                          │
╰─────────────────────────────────────────────╯╰──────────────────────────────────────────────────────────╯
 enter open  f folder  y copy  e editor  tab diff  m mode  / filter  ? help  q quit
```

## Features

- **Every worktree in one view**, each with its branch, folder, commits
  ahead/behind the base branch, and last commit.
- **Per-file `+`/`−` counts** with a small diffstat bar, plus untracked files
  with their line counts.
- **Overlap warnings**: files changed in more than one worktree are flagged
  with `⚠`, counted in the header, and listed in each worktree's summary.
  Press `o` to show only those files.
- **Two modes** (toggle with `m`):
  - *branch*: everything a worktree changed since it left the base branch,
    committed or not. A worktree with five commits never looks "clean".
  - *uncommitted*: only work that isn't committed yet.
- **Syntax-highlighted diffs** with line numbers, word-level change
  highlighting and hunk jumping (`n` / `N`).
- **Open anything**: `enter` opens a file in its system default program, `f`
  shows it in Explorer/Finder, `e` opens the whole worktree in your editor and
  `y` copies the path.
- **Live**: rescans every few seconds while the terminal is focused, slows
  down when it isn't, and refreshes the moment you come back. Background scans
  never take git's index lock, so they can't get in the way of your own git
  commands.
- Light and dark themes chosen from your terminal's background, mouse
  support, a fuzzy file filter (`/`), and a layout that adapts down to 50×12.

## Install

Requires git and [Go](https://go.dev/dl/). grove builds with Go 1.26; any Go
from 1.21 on downloads that toolchain automatically.

From this folder:

```powershell
go install .              # puts grove.exe in %USERPROFILE%\go\bin
# or
go build -o grove.exe .
```

## Use

Run it anywhere inside a repository:

```powershell
grove                     # the repo in the current folder
grove C:\src\my-app       # or point at one
grove C:\src\my-app\.git  # a .git folder, any subfolder or a file works too
grove --mode uncommitted  # start in uncommitted mode
grove --base develop      # compare against a different branch
grove --plain             # print a text summary instead of the UI
```

A typical two-feature setup:

```powershell
git worktree add ..\my-app-feature-a -b feature/a
git worktree add ..\my-app-feature-b -b feature/b
grove
```

When a feature is done, select its worktree and press `d`. grove deletes the
folder and keeps the branch unless you tick *Also delete branch*, which also
shows how many commits aren't in the base branch yet. A worktree with
uncommitted changes is only removed after you tick *Discard uncommitted
changes*, and git double-checks, so a stale screen can't lose work. Locked
worktrees work the same way. The main worktree can't be removed.

### Keys

| Key | Action |
|---|---|
| `↑` `↓` / `j` `k` | move |
| `←` `→` / `h` `l` | fold / unfold a worktree |
| `[` `]` | previous / next worktree |
| `tab` | switch between the list and the diff |
| `enter` | open the file (or fold a worktree) |
| `f` | show in Explorer / Finder |
| `e` | open the worktree in your editor |
| `y` | copy the path |
| `d` | remove the worktree (asks first) |
| `m` | toggle branch / uncommitted mode |
| `o` | show only files changed in several worktrees |
| `/` | filter files |
| `r` | refresh now |
| `n` `N` | next / previous hunk (diff pane) |
| `?` | all shortcuts |
| `q` | quit |

Double-click a file to open it.

### Legend

| Mark | Meaning |
|---|---|
| `●` (amber, next to a file) | has uncommitted edits |
| `⚠` | also changed in another worktree |
| `↑3 ↓1` | commits ahead of / behind the base branch |
| `■■■··` | size of the change; green for additions, red for deletions |
| `◆ here` | the worktree grove was started from |

## Configuration

Everything is optional. Create a starter file with:

```powershell
grove --init-config
```

It lives at `%AppData%\grove\config.toml` on Windows,
`~/Library/Application Support/grove/config.toml` on macOS and
`~/.config/grove/config.toml` on Linux.

```toml
base = ""            # branch to compare with; empty = origin/HEAD, then main, then master
mode = "branch"      # or "uncommitted"
refresh = "3s"       # how often to rescan while focused
editor = ""          # for `e`; empty = first of code, cursor, zed, subl on PATH
theme = "auto"       # "auto", "dark" or "light"

[open]               # per-extension override for `enter`
".log" = "notepad"
".md"  = "default"   # "default" = the system default program
```

**About `.ts` files on Windows:** Windows often registers `.ts` as an MPEG
video stream, so the "default program" for TypeScript can be a media player.
When an editor is available, grove sends `.ts`, `.mts` and `.cts` files to it
instead. Map them to `"default"` under `[open]` to turn that off.

## How it works

grove calls the real `git` CLI rather than a reimplementation, so it always
agrees with `git status` and `git diff`:

| What | Command |
|---|---|
| worktrees | `git worktree list --porcelain -z` |
| base branch | `git symbolic-ref refs/remotes/origin/HEAD`, else `main` / `master` |
| changed files | `git diff --numstat` and `--name-status` against the merge-base (branch mode) or `HEAD` |
| uncommitted / untracked | `git status --porcelain=v2 --untracked-files=all` |
| ahead / behind | `git rev-list --left-right --count base...HEAD` |

Every command runs with `GIT_OPTIONAL_LOCKS=0`, and worktrees are scanned in
parallel.

## Development

```powershell
go test ./...
go vet ./...
```

The UI tests drive the Bubble Tea model directly. To render a real
repository to text files for a visual check:

```powershell
$env:GROVE_DEMO = "C:\path\to\repo"; $env:GROVE_DUMP = "C:\tmp\shots"
go test ./internal/ui -run Dump -count=1
```

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea),
[Lip Gloss](https://github.com/charmbracelet/lipgloss),
[Bubbles](https://github.com/charmbracelet/bubbles) and
[Chroma](https://github.com/alecthomas/chroma).
