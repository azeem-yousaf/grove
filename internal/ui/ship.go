package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/gh"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/open"
	"github.com/charmbracelet/x/ansi"
)

// The Ship menu (s) finishes a worktree's work without leaving grove:
// commit, push, open a pull request, and catch up with the base branch.
// Each runs git (or gh) with the terminal handed over, like the : prompt,
// so hooks, signing, credential prompts and gh's questions all work.

// shipMenu is the Ship menu for one worktree, while open.
type shipMenu struct {
	path, name, branch string
	loading            bool
	up                 git.Upstream
	err                error
	merging            bool
	mergeMsg           string
	baseRemote         string // remote to fetch the base from, when it is a remote branch
	baseBranch         string // the base's name on that remote, or the local base
}

type shipLoadedMsg struct {
	path       string
	up         git.Upstream
	err        error
	merging    bool
	mergeMsg   string
	baseRemote string
	baseBranch string
}

// shipItem is one line of the menu.
type shipItem struct {
	key, title, detail string
	ok                 bool // can run now; otherwise detail says why not
	run                func() tea.Cmd
}

func (m *Model) openShip() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	s := m.snap.States[r.wt]
	if s.Prunable || s.Bare || s.Err != nil {
		return m.notify("That worktree has no folder to commit or push from", toastInfo)
	}
	m.ship = &shipMenu{path: s.Path, name: s.Name(), branch: s.Branch, loading: true}
	path, base := s.Path, m.base
	load := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		msg := shipLoadedMsg{path: path, baseBranch: base}
		msg.up, msg.err = git.UpstreamOf(ctx, path)
		if merging, err := git.MergeInProgress(ctx, path); err == nil && merging {
			msg.merging, msg.mergeMsg = true, git.MergeMessage(ctx, path)
		}
		if base != "" {
			if remote, branch, ok := git.RemoteBranch(ctx, path, base); ok {
				msg.baseRemote, msg.baseBranch = remote, branch
			}
		}
		return msg
	}
	return tea.Batch(load, m.pollPRs(true))
}

func (m *Model) onShipLoaded(msg shipLoadedMsg) tea.Cmd {
	sm := m.ship
	if sm == nil || sm.path != msg.path {
		return nil
	}
	sm.loading = false
	sm.up, sm.err = msg.up, msg.err
	sm.merging, sm.mergeMsg = msg.merging, msg.mergeMsg
	sm.baseRemote, sm.baseBranch = msg.baseRemote, msg.baseBranch
	return nil
}

// shipState is the worktree the menu is for, from the latest scan.
func (m *Model) shipState() (git.State, bool) {
	if m.ship == nil || m.snap == nil {
		return git.State{}, false
	}
	for _, s := range m.snap.States {
		if git.SamePath(s.Path, m.ship.path) {
			return s, true
		}
	}
	return git.State{}, false
}

// commitFiles are the files a commit can take: everything with work not
// committed yet.
func commitFiles(s git.State, mode git.Mode) []git.FileChange {
	var out []git.FileChange
	for _, f := range s.Files {
		if mode == git.ModeUncommitted || f.Uncommitted {
			out = append(out, f)
		}
	}
	return out
}

func (m *Model) shipItems() []shipItem {
	sm := m.ship
	s, ok := m.shipState()
	if !ok {
		return nil
	}
	checking := "checking…"
	var items []shipItem

	// Commit.
	files := commitFiles(s, m.snap.Mode)
	commit := shipItem{key: "c", title: "Commit", run: m.openCommit}
	switch {
	case conflictedFiles(s) > 0:
		commit.detail = "resolve the conflicts first (c)"
	case len(files) == 0 && !sm.merging:
		commit.detail = "nothing to commit"
	case sm.merging:
		commit.detail, commit.ok = "finish the merge", true
	default:
		commit.detail, commit.ok = plural(len(files), "file")+" not committed yet", true
	}
	items = append(items, commit)

	// Push.
	push := shipItem{key: "p", title: "Push", run: m.push}
	switch {
	case sm.loading:
		push.detail = checking
	case sm.err != nil:
		push.detail = sm.err.Error()
	case s.Branch == "":
		push.detail = "no branch checked out"
	case sm.up.Remote == "":
		push.detail = "the repository has no remote"
	case sm.up.Name == "":
		push.detail, push.ok = "first push, to "+sm.up.Remote+"/"+s.Branch, true
	case sm.up.Ahead > 0 && sm.up.Behind > 0:
		push.detail, push.ok = fmt.Sprintf("%s to push, but %s has %d new; pull first", plural(sm.up.Ahead, "commit"), sm.up.Name, sm.up.Behind), true
	case sm.up.Ahead > 0:
		push.detail, push.ok = plural(sm.up.Ahead, "commit")+" not pushed yet", true
	case sm.up.Behind > 0:
		push.detail = fmt.Sprintf("nothing to push; %s has %d new", sm.up.Name, sm.up.Behind)
	default:
		push.detail = "up to date with " + sm.up.Name
	}
	items = append(items, push)

	// Pull request.
	pr := shipItem{key: "r", title: "Pull request"}
	existing, has := m.prs[s.Branch]
	switch {
	case m.ghPath == "":
		pr.detail = "install the GitHub CLI (gh) to open one"
	case s.Branch == "":
		pr.detail = "no branch checked out"
	case has:
		pr.detail, pr.ok = existing.Label()+" · open in the browser", true
		url := existing.URL
		pr.run = func() tea.Cmd { return m.openURL(url) }
	case s.Branch == m.baseName():
		pr.detail = "this is the base branch"
	case m.prErr != nil:
		pr.detail = m.prErr.Error()
	default:
		pr.detail, pr.ok = "create one with gh", true
		pr.run = m.createPR
	}
	items = append(items, pr)

	// Catch up with the base branch.
	if m.base != "" && s.Branch != "" && s.Branch != m.base && s.Branch != m.baseName() {
		merge := shipItem{key: "m", title: "Merge " + m.base + " in", run: func() tea.Cmd { return m.updateFromBase(false) }}
		rebase := shipItem{key: "b", title: "Rebase onto " + m.base, run: func() tea.Cmd { return m.updateFromBase(true) }}
		switch {
		case sm.merging:
			merge.detail, rebase.detail = "a merge is in progress", "a merge is in progress"
		case s.Behind == 0 && sm.baseRemote == "":
			merge.detail, rebase.detail = "up to date", "up to date"
		default:
			behind := "fetches first"
			if s.Behind > 0 {
				behind = plural(s.Behind, "commit") + " behind"
			}
			merge.detail, merge.ok = behind, true
			rebase.detail, rebase.ok = behind+" · rewrites this branch's commits", true
		}
		items = append(items, merge, rebase)
	}
	return items
}

// baseName is the base branch without its remote: main for origin/main.
func (m *Model) baseName() string {
	if m.ship != nil && m.ship.baseRemote != "" {
		return m.ship.baseBranch
	}
	if i := strings.Index(m.base, "/"); i > 0 && strings.HasPrefix(m.base, "origin/") {
		return m.base[i+1:]
	}
	return m.base
}

func (m *Model) onShipKey(msg tea.KeyPressMsg) tea.Cmd {
	switch k := msg.String(); k {
	case "esc", "q", "s":
		m.ship = nil
		return nil
	case "ctrl+c":
		return m.quitNow()
	default:
		for _, it := range m.shipItems() {
			if it.key != k {
				continue
			}
			if !it.ok {
				return m.notify(it.title+": "+it.detail, toastInfo)
			}
			cmd := it.run()
			m.ship = nil
			return cmd
		}
	}
	return nil
}

func (m *Model) push() tea.Cmd {
	sm := m.ship
	args := []string{"push"}
	if sm.up.Name == "" {
		args = []string{"push", "-u", sm.up.Remote, sm.branch}
	}
	return m.runSteps(sm.name, sm.path, "git "+strings.Join(args, " "),
		[]termStep{{args: args, display: "git " + strings.Join(args, " ")}}, nil,
		func(bool) tea.Cmd { return m.pollPRs(true) })
}

// createPR runs gh pr create, which asks for the title and body itself and
// offers to push the branch first.
func (m *Model) createPR() tea.Cmd {
	sm := m.ship
	args := []string{"pr", "create"}
	if base := m.baseName(); base != "" {
		args = append(args, "--base", base)
	}
	display := "gh " + strings.Join(args, " ")
	return m.runSteps(sm.name, sm.path, display, []termStep{{prog: m.ghPath, args: args, display: display}}, nil,
		func(bool) tea.Cmd { return m.pollPRs(true) })
}

func (m *Model) openURL(url string) tea.Cmd {
	return func() tea.Msg {
		if err := open.URL(url); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: "Opened " + url}
	}
}

// updateFromBase merges the base branch in, or rebases onto it, fetching it
// first when it is a remote branch. --autostash keeps work not committed
// yet out of the way.
func (m *Model) updateFromBase(rebase bool) tea.Cmd {
	sm := m.ship
	var steps []termStep
	if sm.baseRemote != "" {
		args := []string{"fetch", sm.baseRemote, sm.baseBranch}
		steps = append(steps, termStep{args: args, display: "git " + strings.Join(args, " ")})
	}
	args := []string{"merge", "--autostash", "--no-edit", m.base}
	if rebase {
		args = []string{"rebase", "--autostash", m.base}
	}
	steps = append(steps, termStep{args: args, display: "git " + strings.Join(args, " ")})
	name := sm.name
	return m.runSteps(name, sm.path, steps[len(steps)-1].display, steps, nil, func(ok bool) tea.Cmd {
		switch {
		case ok:
			return nil
		case rebase:
			return m.notify("The rebase of "+name+" stopped; finish it with : rebase --continue, or : rebase --abort", toastErr)
		default:
			return m.notify("The merge into "+name+" stopped; if it left conflicts, press c on it to resolve them", toastErr)
		}
	})
}

// commitDialog picks the files to commit and the message.
type commitDialog struct {
	path, name string
	files      []commitFile
	cursor     int
	onFiles    bool // focus on the file list rather than the message
	message    textarea.Model
	merging    bool
}

type commitFile struct {
	f  git.FileChange
	on bool
}

func (m *Model) openCommit() tea.Cmd {
	sm := m.ship
	s, ok := m.shipState()
	if !ok {
		return nil
	}
	ta := textarea.New()
	ta.Prompt = ""
	ta.Placeholder = "what this commit does"
	ta.ShowLineNumbers = false
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = 6
	ta.MaxContentHeight = 500
	ta.SetStyles(chatInputStyles(m.dark))
	ta.SetWidth(commitWidth - 2)
	c := &commitDialog{path: s.Path, name: s.Name(), merging: sm.merging, message: ta}
	for _, f := range commitFiles(s, m.snap.Mode) {
		c.files = append(c.files, commitFile{f: f, on: true})
	}
	msg := sm.mergeMsg
	if msg == "" {
		msg = m.suggestedCommitMessage(s.Path)
	}
	c.message.SetValue(msg)
	m.commit = c
	m.ship = nil
	return c.message.Focus()
}

const commitWidth = 64

// suggestedCommitMessage is the message of the newest git commit the
// worktree's agent suggested, else its plan task's title.
func (m *Model) suggestedCommitMessage(path string) string {
	if ses := m.sessionFor(path); ses != nil {
		var cmds []string
		for _, e := range ses.entries {
			if e.role == roleAgent {
				cmds = append(cmds, suggestedCommands(e.text)...)
			}
		}
		for i := len(cmds) - 1; i >= 0; i-- {
			if msg := commitMessageOf(cmds[i]); msg != "" {
				return msg
			}
		}
	}
	if _, t := m.taskFor(path); t != nil {
		return t.Title
	}
	return ""
}

// commitMessageOf is the -m message of a git commit command line, joining
// several -m paragraphs as git does.
func commitMessageOf(line string) string {
	args := splitCommand(line)
	if len(args) < 2 || args[0] != "git" || args[1] != "commit" {
		return ""
	}
	var paras []string
	for i := 2; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-m" || a == "--message" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.HasSuffix(a, "m")):
			if i+1 < len(args) {
				paras = append(paras, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--message="):
			paras = append(paras, strings.TrimPrefix(a, "--message="))
		}
	}
	return strings.Join(paras, "\n\n")
}

func (m *Model) onCommitKey(msg tea.KeyPressMsg) tea.Cmd {
	c := m.commit
	switch msg.String() {
	case "esc":
		m.commit = nil
		return nil
	case "ctrl+c":
		return m.quitNow()
	case "tab", "shift+tab":
		if len(c.files) > 0 && !c.merging {
			c.onFiles = !c.onFiles
			if c.onFiles {
				c.message.Blur()
				return nil
			}
			return c.message.Focus()
		}
		return nil
	case "enter":
		return m.doCommit()
	}
	if !c.onFiles {
		var cmd tea.Cmd
		c.message, cmd = c.message.Update(msg)
		return cmd
	}
	switch msg.String() {
	case "up", "k":
		c.cursor = max(c.cursor-1, 0)
	case "down", "j":
		c.cursor = min(c.cursor+1, len(c.files)-1)
	case "space":
		c.files[c.cursor].on = !c.files[c.cursor].on
	case "a":
		all := true
		for _, f := range c.files {
			all = all && f.on
		}
		for i := range c.files {
			c.files[i].on = !all
		}
	}
	return nil
}

func (m *Model) doCommit() tea.Cmd {
	c := m.commit
	message := strings.TrimSpace(c.message.Value())
	var paths []string
	for _, f := range c.files {
		if f.on || c.merging {
			paths = append(paths, f.f.Path)
			if f.f.OldPath != "" {
				paths = append(paths, f.f.OldPath)
			}
		}
	}
	switch {
	case message == "":
		return m.notify("Write a commit message first", toastInfo)
	case len(paths) == 0 && !c.merging:
		return m.notify("Choose at least one file to commit (tab, then space)", toastInfo)
	}
	p, err := git.PlanCommit(c.path, paths, message, c.merging)
	if err != nil {
		return m.notify("Couldn't list the files for git: "+err.Error(), toastErr)
	}
	var steps []termStep
	if p.Add != nil {
		steps = append(steps, termStep{args: p.Add, display: "git add " + plural(len(paths), "path")})
	}
	if p.Remove != nil {
		steps = append(steps, termStep{args: p.Remove, display: "git rm --cached (files deleted from disk)"})
	}
	subject, _, _ := strings.Cut(message, "\n")
	display := fmt.Sprintf("git commit -m %q", ansi.Truncate(subject, 50, "…"))
	steps = append(steps, termStep{args: p.Commit, display: display})
	path := c.path
	m.commit = nil
	return m.runSteps(c.name, c.path, "git commit", steps, nil, func(ok bool) tea.Cmd {
		p.Cleanup()
		if ok {
			m.dropCommitCards(path)
		}
		return nil
	})
}

// dropCommitCards removes the agent's suggested git commit cards once the
// work is committed through the dialog.
func (m *Model) dropCommitCards(path string) {
	ses := m.sessionFor(path)
	if ses == nil {
		return
	}
	kept := ses.pending[:0]
	for _, p := range ses.pending {
		if p.kind == pendingCommand && commitMessageOf(p.text) != "" {
			continue
		}
		kept = append(kept, p)
	}
	ses.pending = kept
}

// prStyle colours a pull request by its state.
func prStyle(st styles, pr gh.PR) lipgloss.Style {
	switch {
	case pr.State == "MERGED":
		return st.renamed
	case pr.State == "CLOSED":
		return st.faint
	case pr.Draft:
		return st.muted
	}
	return st.add
}

// prsMsg carries a fresh list of pull requests.
type prsMsg struct {
	prs map[string]gh.PR
	err error
}

// pollPRs refreshes the pull requests, at most once a minute unless now.
func (m *Model) pollPRs(now bool) tea.Cmd {
	if m.ghPath == "" || m.prPolling || m.snap == nil || m.prOff && !now {
		return nil
	}
	if !now && m.now.Sub(m.lastPRPoll) < time.Minute {
		return nil
	}
	dir := m.repo.Here
	for _, s := range m.snap.States {
		if s.Main && !s.Bare && !s.Prunable {
			dir = s.Path
		}
	}
	if dir == "" {
		return nil
	}
	m.prPolling, m.lastPRPoll = true, m.now
	list := m.listPRs
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		prs, err := list(ctx, dir)
		return prsMsg{prs, err}
	}
}

func (m *Model) onPRs(msg prsMsg) tea.Cmd {
	m.prPolling = false
	m.prErr = msg.err
	if msg.err != nil {
		// Not signed in, or not a GitHub repository: stop polling, and
		// let the Ship menu say why.
		m.prOff = true
		return nil
	}
	m.prOff = false
	m.prs = msg.prs
	return nil
}

func (m *Model) shipModal() string {
	st := m.st
	sm := m.ship
	parts := []string{
		st.bold.Foreground(st.p.accent).Render("Ship ") + st.bold.Render(sm.name),
		st.faint.Render(shortPath(sm.path, commitWidth)),
		"",
	}
	for _, it := range m.shipItems() {
		title := fmt.Sprintf("%-22s", it.title)
		if it.ok {
			parts = append(parts, st.key.Render(it.key)+"  "+st.text.Render(title)+st.muted.Render(it.detail))
		} else {
			parts = append(parts, st.faint.Render(it.key+"  "+title+it.detail))
		}
	}
	parts = append(parts, "", st.faint.Render("Each runs in the terminal, so hooks and prompts work."), "", m.hints([][2]string{{"esc", "close"}}))
	for i, p := range parts {
		parts[i] = ansi.Truncate(p, min(commitWidth+16, m.width-10), "…")
	}
	return st.modal.Render(strings.Join(parts, "\n"))
}

func (m *Model) commitModal() string {
	st := m.st
	c := m.commit
	w := min(commitWidth, m.width-10)
	c.message.SetWidth(max(w-2, 10))
	title := "Commit"
	if c.merging {
		title = "Finish the merge"
	}
	parts := []string{
		st.bold.Foreground(st.p.accent).Render(title+" ") + st.bold.Render(c.name),
		"",
		st.muted.Render("Message"),
		c.message.View(),
		"",
	}
	if c.merging {
		parts = append(parts, st.muted.Render("Every file is committed: git can't commit part of a merge."))
	} else {
		on := 0
		for _, f := range c.files {
			if f.on {
				on++
			}
		}
		parts = append(parts, st.muted.Render(fmt.Sprintf("Files  %d of %d", on, len(c.files))))
		room := max(m.height-20, 3)
		start := max(min(c.cursor-room/2, len(c.files)-room), 0)
		for i := start; i < len(c.files) && i < start+room; i++ {
			f := c.files[i]
			box := st.faint.Render("[ ]")
			if f.on {
				box = st.accent.Bold(true).Render("[x]")
			}
			mark := "  "
			if c.onFiles && i == c.cursor {
				mark = st.accent.Render("▸ ")
			}
			line := mark + box + " " + statusStyle(st, f.f.Status).Bold(true).Render(string(f.f.Status)) + " " + st.text.Render(f.f.Path)
			parts = append(parts, ansi.Truncate(line, w, "…"))
		}
		if len(c.files) > room {
			parts = append(parts, st.faint.Render(fmt.Sprintf("  %d more; ↑↓ to scroll", len(c.files)-room)))
		}
	}
	hints := [][2]string{{"enter", "commit"}, {"shift+enter", "new line"}}
	if !c.merging && len(c.files) > 0 {
		if c.onFiles {
			hints = [][2]string{{"enter", "commit"}, {"space", "tick"}, {"a", "all"}, {"tab", "message"}}
		} else {
			hints = append(hints, [2]string{"tab", "files"})
		}
	}
	parts = append(parts, "", m.hints(append(hints, [2]string{"esc", "cancel"})))
	return st.modal.Render(strings.Join(parts, "\n"))
}
