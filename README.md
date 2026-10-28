# grove

**See every git worktree at once: what each one changed, and where they collide.**

grove is a terminal UI for working on several features in parallel with
`git worktree`. It lists every worktree in a repository side by side, with
per-file `+`/`−` counts, a syntax-highlighted diff, and a warning whenever two
worktrees touch the same file, which is where your merge conflicts will come
from.

![grove showing three worktrees, with src/api/client.ts changed in both feature branches](docs/screenshot.svg)

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

```powershell
go install github.com/azeem-yousaf/grove@latest
```

That puts `grove` in Go's bin folder (`%USERPROFILE%\go\bin` on Windows,
`~/go/bin` elsewhere), which the Go installer adds to your PATH. Run the same
command again to update.

From a clone:

```powershell
go install .              # install your local build
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

To start a feature, press `n` and type a branch name:

```
 New worktree

 Branch    feature/receipts
           ✓ new branch
 From      Development
 Folder    C:\src\my-app-feature-receipts

 [x] Copy .env, appsettings.Development.json
 [x] Run setup: dotnet restore
 [x] Open in code
```

- A new branch starts from your base branch and doesn't track it, so
  `git pull` and `git push` in the worktree never touch the base.
- An existing branch is checked out as is, and a branch that only exists on
  the remote (`feature/x` or `origin/feature/x`) gets a local branch that
  tracks it. A branch already checked out in another worktree is refused,
  with that worktree's name.
- The folder is suggested next to the main worktree and follows the branch
  name until you type your own.
- Files git ignores, which a new worktree wouldn't otherwise get, are
  copied from the main worktree. By default that's `.env` and `.env.local`;
  set your own under `[new] copy`. An optional `[new] setup` command runs in
  the new folder afterwards.

Plain `git worktree add` works too; grove picks new worktrees up on its own.

To run git in one worktree without leaving grove, select it (or any of its
files) and press `:`, then type the command, with or without the leading
`git`:

```
 feature/auth  git commit -m "Add session refresh"
```

grove hands the terminal to git while it runs, so editors, pagers and
prompts behave normally (`commit` without `-m`, `log`, `rebase -i`, `push`
asking for credentials). Press enter afterwards to return; the worktree is
rescanned straight away. `↑` brings back earlier commands.

### An AI agent per worktree

Press `a` on a worktree to give it an agent: Claude Code or Codex, whichever
is installed and signed in on your PC. grove never asks for an API key; it
runs the tool you already use. If both are installed, grove asks once and
remembers (`ctrl+a` switches later; it's saved as `[ai] agent` in the
config).

```
╭─ ✦ feature/auth · Claude Code ─────────────────── $0.04 ─╮
│ you  Add a remember-me checkbox to the login page        │
│      ↳ Read src/auth/login.ts                            │
│      ↳ Edit src/auth/login.ts                            │
│                                                          │
│ ✦    Added the checkbox. When you're happy:              │
│        git commit -am "Add remember me"                  │
│──────────────────────────────────────────────────────────│
│ Claude Code wants to run                                 │
│ npm test -- auth                                         │
│ enter allow  esc deny                                    │
╰──────────────────────────────────────────────────────────╯
```

- Each worktree has its own agent and conversation, and they can work at
  the same time. The worktree's dot in the list shows what its agent is
  doing (spinning while it works, amber `✦` when it needs you), files it
  changed get a `✦`, and a toast tells you when an agent in another
  worktree finishes or needs you.
- **An agent edits only its own worktree.** It runs in that folder, is told
  the paths of the other worktrees and to keep out of them, and:
  - with Claude Code, edits inside the folder are accepted, while shell
    commands and anything outside the folder are refused unless you allow
    them. Refused commands come back as cards (`enter` allow, `esc` deny);
    files outside the worktree are simply blocked.
  - with Codex, its `workspace-write` sandbox limits writes to the folder.
    Codex can still read outside it, so isolation is stronger with Claude
    Code.
- **Git changes stay yours.** Agents are asked not to commit, check out,
  rebase or push. They suggest those commands instead, which appear as
  cards you can run (`enter`), edit in the `:` prompt (`e`) or skip.
- **Undo.** Before an agent's first turn grove takes a checkpoint of the
  worktree (a git tree object; your index, branch and files are untouched).
  `ctrl+z` in the chat, or `u` in the list, shows every file changed since
  and puts them back byte for byte.
- `ctrl+x` stops a working agent. Quitting while agents work asks first.
- Creating a worktree with `n` has a **Task** field: fill it in and the new
  worktree starts with its agent already at work (after the setup command,
  if there is one).

The agent reads your project's `CLAUDE.md` / `AGENTS.md` as usual. It
sends your code to Anthropic or OpenAI just as the tool would on its own.

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
| `→` / `←` | into the diff from a file; back to the list from the diff's left edge |
| `enter` | open the file (or fold a worktree) |
| `f` | show in Explorer / Finder |
| `e` | open the worktree in your editor |
| `y` | copy the path |
| `n` | create a worktree (in the list) |
| `a` | open the worktree's AI agent |
| `u` | undo everything the worktree's agent changed |
| `d` | remove the worktree (asks first) |
| `:` | run a git command in the selected worktree |
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

[new]                # new worktrees (key n)
copy = [".env", ".env.local", "appsettings.Development.json"]  # copied from the main worktree
setup = "dotnet restore"                                       # run in the new folder; "" = none

[ai]                 # the worktree agents (key a)
agent = "claude"     # "claude" or "codex"; "" = ask when both are installed
model = ""           # passed to the agent; "" = its default

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

The README screenshot is rendered from the real UI. To update it after a UI
change:

```powershell
$env:GROVE_SHOT = "$PWD\docs\screenshot.svg"
go test ./internal/ui -run ReadmeShot -count=1
```

Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea),
[Lip Gloss](https://github.com/charmbracelet/lipgloss),
[Bubbles](https://github.com/charmbracelet/bubbles) and
[Chroma](https://github.com/alecthomas/chroma).
