// Package ui is grove's terminal interface.
package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/open"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/store"
)

type focusArea uint8

const (
	focusList focusArea = iota
	focusDiff
	focusChat  // the worktree agent, shown in place of the diff
	focusGroup // a plan group's chat, shown in place of the diff
)

// Options configure a new Model.
type Options struct {
	Repo    *git.Repo
	Config  config.Config
	Mode    git.Mode
	Base    string // explicit base branch; empty means detect
	Version string
}

// Model is the root Bubble Tea model.
type Model struct {
	ctx     context.Context
	repo    *git.Repo
	cfg     config.Config
	opener  open.Opener
	keys    keyMap
	version string

	baseOverride string
	base         string
	baseReady    bool
	mode         git.Mode

	snap     *git.Snapshot
	scanning bool
	rescan   bool // another scan was requested while one was running
	scanErr  error
	lastScan time.Time
	scanSeq  int

	rows       []row
	cursor     int
	lastSel    string
	listOffset int
	collapsed  map[string]bool
	hereIdx    int

	filter       textinput.Model
	filterActive bool
	filterQuery  string
	overlapView  overlapView

	predictor      *git.Predictor
	predictions    []git.Prediction // pairs of worktrees, indexing m.snap.States
	predictSeq     int
	predicting     bool
	repredict      bool // another prediction was requested while one was running
	predictChecked bool // the git version has been checked
	predictOff     bool // git is too old to predict conflicts

	diff        viewport.Model
	diffCache   map[string]string
	diffErrs    map[string]error
	diffSeq     int
	diffWanted  string // key of the diff the selection needs
	shownKey    string // key of the diff currently in the viewport
	shownRaw    string
	renderedFor string
	parsed      parsedDiff

	focus       focusArea
	width       int
	height      int
	dark        bool
	themeForced bool
	st          styles
	showHelp    bool
	toast       toast
	toastID     int
	now         time.Time
	frame       int
	animating   bool
	termFocused bool

	lastClickRow int
	lastClickAt  time.Time

	confirm  *removal // the remove dialog, while open
	removing *removal // a removal git is working on

	gitCmd gitPrompt

	creating   *creation // the new-worktree dialog, while open
	selectPath string    // worktree to select once the next scan shows it

	sessions      map[string]*session    // worktree agents by folder
	store         *store.Store           // saved sessions and plans; nil without a git dir
	savedJSON     map[string][]byte      // each session as last written, by folder
	persistErr    string                 // the last save error shown
	chatPath      string                 // worktree whose agent the chat pane shows
	chatPlanner   bool                   // the chat pane shows the planner instead
	planner       *session               // the planner's conversation, once started
	plans         []*plan.Plan           // saved plans, newest first
	review        *planReview            // the plan being reviewed, while open
	creatingTask  map[string]bool        // plan tasks whose worktree is being made, by plan+task ID
	branchNames   []string               // existing branches, for the planner's briefing
	planBranches  git.Branches           // branches when the last plan started, for creating its worktrees
	groveBin      string                 // this program, which agents start for group tools
	groups        map[string]*groupState // group chats by folder
	groupChat     *groupChat             // the group chat pane
	pollingGroups bool
	lastGroupPoll time.Time
	conflictsTold map[string][]string // files each task's agent was told conflict with another worktree (conflictnotify.go)
	chatInput     textarea.Model
	chatView      viewport.Model
	chatFollow    bool // keep the chat scrolled to the newest line
	chatSel       chatSelection
	picker        *picker
	undo          *undoAsk
	quitAsk       bool
	conflicts     *conflictReview // the merge review, while open
	overview      bool            // the overview of every worktree, in place of both panes
	ovPath        string          // worktree under the overview's cursor
	ovStart       int             // first and past-last rows of the overview table on screen
	ovEnd         int
	ovRowsY       int // screen row of the first table row
	mergeSeq      int

	// Hooks for the agent, replaced in tests.
	installed func() []agent.Kind
	runAgent  func(context.Context, agent.Request, func(agent.Event))
	warmAgent func(agent.Request)
	stopAgent func(dir string) // waits until the folder is no longer in use
	saveAgent func(string) error
	saveModel func(agent, model string) error
}

type toastKind uint8

const (
	toastInfo toastKind = iota
	toastOK
	toastErr
)

type toast struct {
	text string
	kind toastKind
	id   int
}

type (
	baseMsg struct {
		base string
		err  error
	}
	scanMsg struct {
		seq  int
		snap *git.Snapshot
		err  error
	}
	diffMsg struct {
		key string
		raw string
		err error
	}
	diffDebounceMsg struct{ seq int }
	clockMsg        time.Time
	frameMsg        struct{}
	actionMsg       struct {
		text string
		err  error
	}
	toastClearMsg struct{ id int }
)

// New creates the root model.
func New(ctx context.Context, o Options) *Model {
	ti := textinput.New()
	ti.Prompt = "/ "
	ti.Placeholder = "filter files"

	agents := agent.NewRunner()
	m := &Model{
		ctx:          ctx,
		repo:         o.Repo,
		cfg:          o.Config,
		opener:       open.New(o.Config.Editor, o.Config.Open),
		keys:         defaultKeys(),
		version:      o.Version,
		baseOverride: o.Base,
		mode:         o.Mode,
		collapsed:    map[string]bool{},
		hereIdx:      -1,
		filter:       ti,
		diff:         viewport.New(),
		diffCache:    map[string]string{},
		diffErrs:     map[string]error{},
		dark:         true,
		now:          time.Now(),
		termFocused:  true,
		lastClickRow: -1,
		gitCmd:       newGitPrompt(),
		sessions:     map[string]*session{},
		savedJSON:    map[string][]byte{},
		creatingTask: map[string]bool{},
		groups:       map[string]*groupState{},
		groveBin:     groveExecutable(),
		predictor:    git.NewPredictor(),
		chatInput:    newChatInput(),
		chatView:     newChatView(),
		installed:    agent.Installed,
		runAgent:     agents.Run,
		warmAgent:    agents.Warm,
		stopAgent:    agents.Stop,
		saveAgent:    defaultSaveAgent,
		saveModel:    config.SaveModel,
	}
	if o.Repo.CommonDir != "" {
		m.store = store.Open(o.Repo.CommonDir)
	}
	switch o.Config.Theme {
	case "light":
		m.dark, m.themeForced = false, true
	case "dark":
		m.themeForced = true
	}
	m.applyTheme()
	return m
}

func (m *Model) applyTheme() {
	m.st = newStyles(m.dark)
	m.filter.SetStyles(textinput.DefaultStyles(m.dark))
	m.gitCmd.input.SetStyles(textinput.DefaultStyles(m.dark))
	m.chatInput.SetStyles(chatInputStyles(m.dark))
	m.renderedFor = ""
}

// Init starts base detection, the clock and background colour detection.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.detectBase(), clockTick(), m.startAnimating(), m.loadSessions(), m.loadPlans()}
	if !m.themeForced {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	m.scanning = true
	return tea.Batch(cmds...)
}

func (m *Model) detectBase() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		base, err := git.DetectBase(ctx, m.repo, m.baseOverride)
		return baseMsg{base, err}
	}
}

func clockTick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return clockMsg(t) })
}

func (m *Model) startAnimating() tea.Cmd {
	if m.animating {
		return nil
	}
	m.animating = true
	return tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg { return frameMsg{} })
}

// startScan rescans every worktree, or queues a rescan if one is running.
func (m *Model) startScan() tea.Cmd {
	if !m.baseReady || m.removing != nil {
		return nil
	}
	if m.scanning && m.scanSeq > 0 {
		m.rescan = true
		return nil
	}
	m.scanning = true
	m.scanSeq++
	seq, base, mode := m.scanSeq, m.base, m.mode
	scan := func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
		defer cancel()
		snap, err := git.Scan(ctx, m.repo, base, mode)
		return scanMsg{seq, snap, err}
	}
	return tea.Batch(scan, m.startAnimating())
}

func (m *Model) refreshInterval() time.Duration {
	d := m.cfg.Refresh.Duration
	if d <= 0 {
		d = 3 * time.Second
	}
	if !m.termFocused {
		d = max(d*5, 15*time.Second)
	}
	// Agents' edits should show up while they work.
	if m.anyAgentRunning() {
		d = min(d, time.Second)
	}
	return d
}

func (m *Model) notify(text string, kind toastKind) tea.Cmd {
	m.toastID++
	id := m.toastID
	m.toast = toast{text: text, kind: kind, id: id}
	d := 3 * time.Second
	if kind == toastErr {
		d = 6 * time.Second
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return toastClearMsg{id} })
}

// Update handles every message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.ensureCursorVisible()
	m.refreshDiffView()
	return m, cmd
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return nil

	case tea.BackgroundColorMsg:
		if !m.themeForced && msg.IsDark() != m.dark {
			m.dark = msg.IsDark()
			m.applyTheme()
		}
		return nil

	case tea.FocusMsg:
		m.termFocused = true
		// Coming back to the terminal is when stale data is most noticeable.
		return m.startScan()

	case tea.BlurMsg:
		m.termFocused = false
		return nil

	case baseMsg:
		m.baseReady = true
		m.base = msg.base
		m.scanning = false
		var cmds []tea.Cmd
		if msg.err != nil {
			cmds = append(cmds, m.notify(msg.err.Error()+"; comparing with HEAD", toastErr))
		}
		cmds = append(cmds, m.startScan())
		return tea.Batch(cmds...)

	case scanMsg:
		return m.onScan(msg)

	case predictMsg:
		return m.onPredict(msg)

	case conflictCheckMsg:
		return m.onConflictCheck(msg)

	case mergePlanMsg:
		return m.onMergePlan(msg)

	case clockMsg:
		m.now = time.Time(msg)
		cmds := []tea.Cmd{clockTick(), m.persistSessions(), m.pollGroupsEvery(m.now)}
		if m.baseReady && !m.scanning && m.now.Sub(m.lastScan) >= m.refreshInterval() {
			cmds = append(cmds, m.startScan())
		}
		return tea.Batch(cmds...)

	case frameMsg:
		m.frame++
		busy := m.scanning || m.diffPending() || m.removing != nil || m.anyAgentRunning() ||
			m.creating != nil && m.creating.running || m.undo != nil && m.undo.running ||
			m.conflicts != nil && m.conflicts.busy() ||
			m.review != nil && m.review.checking || len(m.creatingTask) > 0
		if !busy {
			m.animating = false
			return nil
		}
		return tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg { return frameMsg{} })

	case diffDebounceMsg:
		if msg.seq == m.diffSeq {
			return m.loadDiff(m.diffWanted)
		}
		return nil

	case diffMsg:
		if msg.err != nil {
			m.diffErrs[msg.key] = msg.err
		} else {
			delete(m.diffErrs, msg.key)
			m.diffCache[msg.key] = msg.raw
		}
		return nil

	case removeMsg:
		return m.onRemoved(msg)

	case gitDoneMsg:
		return m.onGitDone(msg)

	case branchesMsg:
		if c := m.creating; c != nil {
			c.branches, c.loadErr = &msg.b, msg.err
			m.syncFolder()
		}
		return nil

	case createdMsg:
		return m.onCreated(msg)

	case agentMsg:
		return m.onAgent(msg)

	case sessionsLoadedMsg:
		return m.onSessionsLoaded(msg)

	case plansLoadedMsg:
		return m.onPlansLoaded(msg)

	case branchNamesMsg:
		m.branchNames = msg.names
		return nil

	case planCheckedMsg:
		return m.onPlanChecked(msg)

	case taskCreatedMsg:
		return m.onTaskCreated(msg)

	case groupsReadMsg:
		return m.onGroupsRead(msg)

	case groupPostedMsg:
		return m.pollGroups()

	case persistMsg:
		return m.onPersisted(msg)

	case agentSavedMsg:
		if msg.err != nil {
			return m.notify("Couldn't save the choice: "+msg.err.Error(), toastErr)
		}
		return nil

	case undoReadyMsg:
		return m.onUndoReady(msg)

	case undoDoneMsg:
		return m.onUndoDone(msg)

	case quitNowMsg:
		return tea.Quit

	case actionMsg:
		if msg.err != nil {
			return m.notify(msg.err.Error(), toastErr)
		}
		return m.notify(msg.text, toastOK)

	case mergeDoneMsg:
		return m.onMergeDone(msg)

	case mergeAbortMsg:
		return m.onMergeAbort(msg)

	case toastClearMsg:
		if msg.id == m.toast.id {
			m.toast = toast{}
		}
		return nil

	case tea.KeyPressMsg:
		return m.onKey(msg)

	case tea.PasteMsg:
		return m.onPaste(msg)

	case tea.MouseClickMsg:
		return m.onClick(msg)

	case tea.MouseMotionMsg:
		return m.onMouseMotion(msg)

	case tea.MouseReleaseMsg:
		return m.onMouseRelease(msg)

	case tea.MouseWheelMsg:
		return m.onWheel(msg)
	}

	if m.gitCmd.active {
		var cmd tea.Cmd
		m.gitCmd.input, cmd = m.gitCmd.input.Update(msg)
		return cmd
	}
	if c := m.creating; c != nil && c.focus <= fieldTask {
		var cmd tea.Cmd
		c.inputs[c.focus], cmd = c.inputs[c.focus].Update(msg)
		return cmd
	}
	if m.focus == focusChat {
		var cmd tea.Cmd
		m.chatInput, cmd = m.chatInput.Update(msg)
		return cmd
	}
	if m.filterActive {
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		return cmd
	}
	return nil
}

// onPaste puts a bracketed paste into whichever text field has the
// keyboard, following the same order as onKey. Dialogs without a text field
// ignore it, so pasted text can never act as key presses.
func (m *Model) onPaste(msg tea.PasteMsg) tea.Cmd {
	var cmd tea.Cmd
	switch {
	case m.quitAsk, m.conflicts != nil, m.confirm != nil, m.picker != nil, m.undo != nil:
		return nil
	case m.review != nil:
		if e := m.review.edit; e != nil {
			return e.update(msg)
		}
		if m.review.naming != nil {
			*m.review.naming, cmd = m.review.naming.Update(msg)
		}
		return cmd
	case m.creating != nil:
		c := m.creating
		if c.running || c.focus > fieldTask {
			return nil
		}
		c.inputs[c.focus], cmd = c.inputs[c.focus].Update(msg)
		c.err = ""
		switch c.focus {
		case fieldBranch:
			m.syncFolder()
		case fieldFolder:
			c.folderEdited = c.inputs[fieldFolder].Value() != c.autoFolder
		}
		return cmd
	case m.gitCmd.active:
		m.gitCmd.input, cmd = m.gitCmd.input.Update(msg)
		return cmd
	case m.focus == focusGroup:
		if m.groupChat != nil {
			m.groupChat.input, cmd = m.groupChat.input.Update(msg)
		}
		return cmd
	case m.focus == focusChat:
		if s := m.chatSession(); s != nil && s.menu == nil && len(s.pending) == 0 {
			m.chatInput, cmd = m.chatInput.Update(msg)
			m.chatFollow = true
		}
		return cmd
	case m.filterActive:
		m.filter, cmd = m.filter.Update(msg)
		if m.filter.Value() != m.filterQuery {
			m.setFilter(m.filter.Value())
			return tea.Batch(cmd, m.selectionChanged(false))
		}
		return cmd
	}
	return nil
}

func (m *Model) onScan(msg scanMsg) tea.Cmd {
	if msg.seq != m.scanSeq {
		return nil
	}
	m.scanning = false
	m.lastScan = time.Now()
	var cmds []tea.Cmd
	if msg.err != nil {
		if m.scanErr == nil || m.scanErr.Error() != msg.err.Error() {
			cmds = append(cmds, m.notify("Scan failed: "+msg.err.Error(), toastErr))
		}
		m.scanErr = msg.err
	} else {
		m.scanErr = nil
		m.predictions = remapPredictions(m.predictions, m.snap, msg.snap)
		m.snap = msg.snap
		if m.repo.Here != "" {
			m.hereIdx = m.snap.IndexOf(m.repo.Here)
		}
		// Files may have changed underneath the cached diffs.
		m.diffCache = map[string]string{}
		m.diffErrs = map[string]error{}
		if m.selectPath != "" {
			if i := m.snap.IndexOf(m.selectPath); i >= 0 {
				delete(m.collapsed, m.snap.States[i].Path)
				m.lastSel = m.snap.States[i].Path
				m.selectPath = ""
			}
		}
		m.rebuildRows()
		cmds = append(cmds, m.selectionChanged(true), m.schedule(), m.startPredict())
	}
	if m.removing != nil && m.removing.queued {
		cmds = append(cmds, m.runRemoval())
	} else if m.rescan {
		m.rescan = false
		cmds = append(cmds, m.startScan())
	}
	return tea.Batch(cmds...)
}

// selected returns the selected row, if any.
func (m *Model) selected() (row, bool) {
	if m.snap == nil || m.cursor < 0 || m.cursor >= len(m.rows) {
		return row{}, false
	}
	return m.rows[m.cursor], true
}

func (m *Model) selectedFile() (git.State, git.FileChange, bool) {
	r, ok := m.selected()
	if !ok || r.kind != rowFile {
		return git.State{}, git.FileChange{}, false
	}
	st := m.snap.States[r.wt]
	return st, st.Files[r.file], true
}

func diffKey(st git.State, f git.FileChange) string {
	return st.Path + "\x00" + st.Rev + "\x00" + f.Path
}

// selectionChanged queues loading the diff for the newly selected file.
// immediate skips the debounce, used after a refresh.
func (m *Model) selectionChanged(immediate bool) tea.Cmd {
	m.syncSel()
	st, f, ok := m.selectedFile()
	if !ok {
		m.diffWanted = ""
		return nil
	}
	k := diffKey(st, f)
	m.diffWanted = k
	if _, ok := m.diffCache[k]; ok {
		return nil
	}
	if immediate {
		return m.loadDiff(k)
	}
	// Debounce so holding an arrow key doesn't spawn a git process per row.
	m.diffSeq++
	seq := m.diffSeq
	return tea.Batch(
		tea.Tick(60*time.Millisecond, func(time.Time) tea.Msg { return diffDebounceMsg{seq} }),
		m.startAnimating(),
	)
}

func (m *Model) diffPending() bool {
	if m.diffWanted == "" {
		return false
	}
	_, ok := m.diffCache[m.diffWanted]
	_, failed := m.diffErrs[m.diffWanted]
	return !ok && !failed
}

func (m *Model) loadDiff(k string) tea.Cmd {
	st, f, ok := m.selectedFile()
	if !ok || diffKey(st, f) != k {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
		defer cancel()
		raw, err := git.Diff(ctx, st, f)
		return diffMsg{k, raw, err}
	}
}

func (m *Model) move(delta int) tea.Cmd {
	if len(m.rows) == 0 {
		return nil
	}
	next := max(0, min(len(m.rows)-1, m.cursor+delta))
	if next == m.cursor {
		return nil
	}
	m.cursor = next
	return m.selectionChanged(false)
}

func (m *Model) moveTo(i int) tea.Cmd {
	return m.move(i - m.cursor)
}

// jumpTree moves to the next (dir > 0) or previous worktree header.
func (m *Model) jumpTree(dir int) tea.Cmd {
	for i := m.cursor + dir; i >= 0 && i < len(m.rows); i += dir {
		if m.rows[i].kind == rowTree {
			return m.moveTo(i)
		}
	}
	return nil
}

func (m *Model) toggleCollapse(wt int) tea.Cmd {
	path := m.snap.States[wt].Path
	m.collapsed[path] = !m.collapsed[path]
	// Keep the selection on the worktree header being folded.
	m.lastSel = path
	m.rebuildRows()
	return m.selectionChanged(false)
}

func (m *Model) onKey(msg tea.KeyPressMsg) tea.Cmd {
	k := m.keys

	switch {
	case m.quitAsk:
		return m.onQuitKey(msg)
	case m.conflicts != nil:
		return m.conflictKey(msg)
	case m.confirm != nil:
		return m.onConfirmKey(msg)
	case m.creating != nil:
		return m.onCreateKey(msg)
	case m.picker != nil:
		return m.onPickerKey(msg)
	case m.undo != nil:
		return m.onUndoKey(msg)
	case m.review != nil:
		return m.onReviewKey(msg)
	case m.gitCmd.active:
		return m.onGitPromptKey(msg)
	case m.focus == focusChat:
		return m.onChatKey(msg)
	case m.focus == focusGroup:
		return m.onGroupKey(msg)
	}
	if m.showHelp {
		if key.Matches(msg, k.Help, k.Back, k.Quit) || msg.String() == "enter" {
			m.showHelp = false
		}
		if msg.String() == "ctrl+c" {
			return m.quitNow()
		}
		return nil
	}

	if m.overview {
		return m.onOverviewKey(msg)
	}

	if m.filterActive {
		switch msg.String() {
		case "esc":
			m.filterActive = false
			m.filter.Blur()
			m.filter.SetValue("")
			m.setFilter("")
			return m.selectionChanged(false)
		case "enter", "tab":
			m.filterActive = false
			m.filter.Blur()
			return nil
		case "up", "ctrl+p":
			return m.move(-1)
		case "down", "ctrl+n":
			return m.move(1)
		case "ctrl+c":
			return m.quitNow()
		}
		var cmd tea.Cmd
		m.filter, cmd = m.filter.Update(msg)
		if m.filter.Value() != m.filterQuery {
			m.setFilter(m.filter.Value())
			return tea.Batch(cmd, m.selectionChanged(false))
		}
		return cmd
	}

	switch {
	case msg.String() == "ctrl+c":
		return m.quitNow()
	case key.Matches(msg, k.Merge):
		return m.openConflictReview()
	case key.Matches(msg, k.Quit):
		return m.quit()
	case key.Matches(msg, k.Help):
		m.showHelp = true
		return nil
	case key.Matches(msg, k.Refresh):
		m.toastID++
		return tea.Batch(m.startScan(), m.notify("Refreshing…", toastInfo))
	case key.Matches(msg, k.Mode):
		return m.toggleMode()
	case key.Matches(msg, k.Overlaps):
		return m.toggleOverlaps()
	case key.Matches(msg, k.Filter):
		if m.snap == nil {
			return nil
		}
		m.filterActive = true
		m.focus = focusList
		m.filter.SetValue(m.filterQuery)
		m.filter.CursorEnd()
		return m.filter.Focus()
	case key.Matches(msg, k.SwitchPane):
		if m.focus != focusList {
			m.focus = focusList
			return nil
		}
		// Back to the agent if its worktree is the one selected.
		if r, ok := m.selected(); ok && !m.chatPlanner && m.chatPath != "" && git.SamePath(m.snap.States[r.wt].Path, m.chatPath) && m.chatSession() != nil {
			return m.showChat(m.chatSession())
		}
		m.focus = focusDiff
		return nil
	case key.Matches(msg, k.Back):
		switch {
		case m.focus == focusDiff:
			m.focus = focusList
		case m.filtering():
			m.filter.SetValue("")
			m.overlapView = overlapsOff
			m.setFilter("")
			return m.selectionChanged(false)
		}
		return nil
	case key.Matches(msg, k.Reveal):
		return m.reveal()
	case key.Matches(msg, k.Editor):
		return m.openEditor()
	case key.Matches(msg, k.Copy):
		return m.copyPath()
	case key.Matches(msg, k.Remove):
		return m.askRemove()
	case key.Matches(msg, k.Git):
		return m.openGitPrompt()
	case key.Matches(msg, k.Agent):
		return m.openChat()
	case key.Matches(msg, k.Overview):
		return m.openOverview()
	case key.Matches(msg, k.Planner):
		return m.openPlanner()
	case key.Matches(msg, k.Group):
		if r, ok := m.selected(); ok {
			return m.openGroupChat(m.snap.States[r.wt].Path)
		}
		return nil
	}

	if m.focus == focusDiff {
		return m.onDiffKey(msg)
	}
	return m.onListKey(msg)
}

func (m *Model) onListKey(msg tea.KeyPressMsg) tea.Cmd {
	k := m.keys
	r, ok := m.selected()
	switch {
	case key.Matches(msg, k.Up):
		return m.move(-1)
	case key.Matches(msg, k.Down):
		return m.move(1)
	case key.Matches(msg, k.PageUp):
		return m.move(-max(m.listHeight()/2, 1))
	case key.Matches(msg, k.PageDown):
		return m.move(max(m.listHeight()/2, 1))
	case key.Matches(msg, k.Top):
		return m.moveTo(0)
	case key.Matches(msg, k.Bottom):
		return m.moveTo(len(m.rows) - 1)
	case key.Matches(msg, k.NextTree):
		return m.jumpTree(1)
	case key.Matches(msg, k.PrevTree):
		return m.jumpTree(-1)
	case key.Matches(msg, k.New):
		// List only: in the diff pane n jumps to the next hunk.
		return m.askCreate()
	case !ok:
		return nil
	case key.Matches(msg, k.Undo):
		return m.askUndo(m.sessionFor(m.snap.States[r.wt].Path))
	case key.Matches(msg, k.Collapse):
		if r.kind == rowFile {
			for i := m.cursor; i >= 0; i-- {
				if m.rows[i].kind == rowTree {
					return m.moveTo(i)
				}
			}
		}
		if !m.collapsed[m.snap.States[r.wt].Path] {
			return m.toggleCollapse(r.wt)
		}
	case key.Matches(msg, k.Expand):
		if r.kind == rowTree {
			if m.collapsed[m.snap.States[r.wt].Path] {
				return m.toggleCollapse(r.wt)
			}
			return m.move(1)
		}
		m.focus = focusDiff
	case key.Matches(msg, k.Open), msg.String() == "space":
		if r.kind == rowTree {
			return m.toggleCollapse(r.wt)
		}
		return m.openFile()
	}
	return nil
}

func (m *Model) onDiffKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		if _, _, ok := m.selectedFile(); ok {
			return m.openFile()
		}
		return nil
	case "n":
		m.jumpHunk(1)
		return nil
	case "N", "p":
		m.jumpHunk(-1)
		return nil
	case "g", "home":
		m.diff.GotoTop()
		return nil
	case "G", "end":
		m.diff.GotoBottom()
		return nil
	case "]":
		return m.move(1)
	case "[":
		return m.move(-1)
	case "left", "h":
		// Already at the left edge: carry on into the file list.
		if m.diff.XOffset() == 0 {
			m.focus = focusList
			return nil
		}
	}
	var cmd tea.Cmd
	m.diff, cmd = m.diff.Update(msg)
	return cmd
}

// jumpHunk scrolls the diff to the next or previous hunk header.
func (m *Model) jumpHunk(dir int) {
	if m.shownKey == "" {
		return
	}
	y := m.diff.YOffset()
	if dir > 0 {
		for i, l := range m.parsed.lines {
			if l.kind == lineHunk && i > y {
				m.diff.SetYOffset(i)
				return
			}
		}
		return
	}
	for i := len(m.parsed.lines) - 1; i >= 0; i-- {
		if m.parsed.lines[i].kind == lineHunk && i < y {
			m.diff.SetYOffset(i)
			return
		}
	}
	m.diff.GotoTop()
}

func (m *Model) setFilter(q string) {
	m.filterQuery = strings.TrimSpace(q)
	if m.snap != nil {
		m.rebuildRows()
	}
}

func (m *Model) toggleMode() tea.Cmd {
	if m.mode == git.ModeBranch {
		m.mode = git.ModeUncommitted
	} else {
		m.mode = git.ModeBranch
	}
	text := "Showing uncommitted changes only"
	if m.mode == git.ModeBranch {
		text = "Showing everything changed since " + m.baseLabel()
	}
	m.rescan = m.scanning
	return tea.Batch(m.notify(text, toastInfo), m.startScan())
}

func (m *Model) toggleOverlaps() tea.Cmd {
	if m.snap == nil {
		return nil
	}
	// off → overlaps → conflicts only → off, skipping conflicts when there
	// are none.
	switch m.overlapView {
	case overlapsOff:
		m.overlapView = overlapsShown
	case overlapsShown:
		m.overlapView = overlapsOff
		if m.conflictCount() > 0 {
			m.overlapView = conflictsShown
		}
	default:
		m.overlapView = overlapsOff
	}
	m.rebuildRows()
	if m.overlapView == overlapsShown && m.snap.OverlapCount() == 0 {
		return tea.Batch(m.notify("No files are changed in more than one worktree", toastInfo), m.selectionChanged(false))
	}
	return m.selectionChanged(false)
}

func (m *Model) baseLabel() string {
	if m.base == "" {
		return "HEAD"
	}
	return m.base
}

// action runs fn off the UI goroutine and reports the result as a toast.
func action(fn func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		text, err := fn()
		return actionMsg{text, err}
	}
}

func (m *Model) openFile() tea.Cmd {
	st, f, ok := m.selectedFile()
	if !ok {
		return nil
	}
	if f.Status == git.Deleted {
		return m.notify("That file was deleted in this worktree; press f to open its folder", toastInfo)
	}
	path := git.AbsPath(st, f)
	name := baseName(f.Path)
	opener := m.opener
	return action(func() (string, error) {
		via, err := opener.File(path)
		if err != nil {
			return "", fmt.Errorf("couldn't open %s: %w", name, err)
		}
		return fmt.Sprintf("Opened %s in %s", name, via), nil
	})
}

func (m *Model) reveal() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	st := m.snap.States[r.wt]
	if r.kind == rowTree {
		return action(func() (string, error) {
			return "Opened folder", open.Default(st.Path)
		})
	}
	f := st.Files[r.file]
	path := git.AbsPath(st, f)
	if f.Status == git.Deleted {
		path = git.DirOf(st, f)
	}
	return action(func() (string, error) {
		return "Showing " + baseName(f.Path) + " in its folder", open.Reveal(path)
	})
}

func (m *Model) openEditor() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	st := m.snap.States[r.wt]
	opener := m.opener
	return action(func() (string, error) {
		via, err := opener.Folder(st.Path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Opened %s in %s", st.Name(), via), nil
	})
}

func (m *Model) copyPath() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	st := m.snap.States[r.wt]
	path := st.Path
	if r.kind == rowFile {
		path = git.AbsPath(st, st.Files[r.file])
	}
	return tea.Batch(
		tea.SetClipboard(path),
		action(func() (string, error) {
			_ = open.Copy(path) // OSC 52 above covers terminals where this fails
			return "Copied " + path, nil
		}),
	)
}

func (m *Model) onClick(msg tea.MouseClickMsg) tea.Cmd {
	if m.confirm != nil || m.creating != nil || m.picker != nil || m.undo != nil || m.quitAsk || m.conflicts != nil || m.review != nil {
		return nil
	}
	if m.showHelp {
		m.showHelp = false
		return nil
	}
	if m.overview {
		return m.onOverviewClick(msg)
	}
	m.chatSel = chatSelection{}
	listW, _, paneH := m.layout()
	top := 1 // header line
	if msg.Y < top || msg.Y >= top+paneH {
		return nil
	}
	if msg.Button == tea.MouseLeft && m.inChat(msg.X, msg.Y) {
		m.startChatSelection(msg.Y)
		return nil
	}
	if m.narrow() && m.focus != focusList || !m.narrow() && msg.X >= listW {
		if m.focus != focusChat && m.focus != focusGroup {
			m.focus = focusDiff
		}
		return nil
	}
	m.focus = focusList
	if msg.Button != tea.MouseLeft || m.snap == nil {
		return nil
	}
	dh := m.dashHeight()
	if msg.Y < top+dh {
		return m.openOverview()
	}
	line := msg.Y - top - dh - 1 + m.listOffset
	lines := m.listLines(listW - 2)
	if line < 0 || line >= len(lines) || lines[line].row < 0 {
		return nil
	}
	ri := lines[line].row
	double := ri == m.lastClickRow && time.Since(m.lastClickAt) < 400*time.Millisecond
	m.lastClickRow, m.lastClickAt = ri, time.Now()
	cmd := m.moveTo(ri)
	if double {
		m.lastClickRow = -1
		if m.rows[ri].kind == rowTree {
			return tea.Batch(cmd, m.toggleCollapse(m.rows[ri].wt))
		}
		return tea.Batch(cmd, m.openFile())
	}
	return cmd
}

func (m *Model) onWheel(msg tea.MouseWheelMsg) tea.Cmd {
	if m.overview {
		switch msg.Button {
		case tea.MouseWheelUp:
			m.ovMove(-1)
		case tea.MouseWheelDown:
			m.ovMove(1)
		}
		return nil
	}
	listW, _, _ := m.layout()
	overDiff := m.narrow() && m.focus != focusList || !m.narrow() && msg.X >= listW
	if overDiff && m.focus == focusGroup && m.groupChat != nil {
		switch msg.Button {
		case tea.MouseWheelUp:
			m.groupChat.view.ScrollUp(3)
			m.groupChat.follow = false
		case tea.MouseWheelDown:
			m.groupChat.view.ScrollDown(3)
			m.groupChat.follow = m.groupChat.view.AtBottom()
		}
		return nil
	}
	if overDiff && m.focus == focusChat {
		switch msg.Button {
		case tea.MouseWheelUp:
			m.chatView.ScrollUp(3)
			m.chatFollow = false
		case tea.MouseWheelDown:
			m.chatView.ScrollDown(3)
			m.chatFollow = m.chatView.AtBottom()
		}
		return nil
	}
	switch msg.Button {
	case tea.MouseWheelUp:
		if overDiff {
			m.diff.ScrollUp(3)
			return nil
		}
		return m.move(-1)
	case tea.MouseWheelDown:
		if overDiff {
			m.diff.ScrollDown(3)
			return nil
		}
		return m.move(1)
	case tea.MouseWheelLeft:
		if overDiff {
			m.diff.ScrollLeft(6)
		}
	case tea.MouseWheelRight:
		if overDiff {
			m.diff.ScrollRight(6)
		}
	}
	return nil
}

// ensureCursorVisible scrolls the list so the selected row is on screen,
// including the spacer above a worktree header.
func (m *Model) ensureCursorVisible() {
	if m.snap == nil || m.width == 0 {
		return
	}
	listW, _, _ := m.layout()
	h := m.listHeight()
	lines := m.listLines(listW - 2)
	first, last := -1, -1
	for i, l := range lines {
		if l.row == m.cursor {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	// Keep the "nothing changed" note under a header in view too.
	if last >= 0 && last+1 < len(lines) && lines[last+1].row < 0 && lines[last+1].text != "" {
		last++
	}
	if first >= 0 {
		if first > 0 && m.rows[m.cursor].kind == rowTree {
			first--
		}
		if first < m.listOffset {
			m.listOffset = first
		}
		if last >= m.listOffset+h {
			m.listOffset = last - h + 1
		}
	}
	m.listOffset = max(0, min(m.listOffset, len(lines)-h))
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
