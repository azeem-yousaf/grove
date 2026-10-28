// Command grove shows every git worktree in a repository, what each one
// changed, and where they overlap.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/ui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		lipgloss.Fprintln(os.Stderr, lipgloss.NewStyle().Foreground(lipgloss.Color("#F87171")).Render("grove: ")+err.Error())
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "mcp-group" {
		return mcpGroup(os.Args[2:])
	}
	var (
		base       = flag.String("base", "", "branch to compare worktrees against (default: auto-detect)")
		mode       = flag.String("mode", "", `"branch" (everything since the base) or "uncommitted"`)
		plain      = flag.Bool("plain", false, "print a summary instead of opening the interface")
		initConfig = flag.Bool("init-config", false, "write a starter config file and print its path")
		showVer    = flag.Bool("version", false, "print the version")
	)
	flag.Usage = usage
	paths, err := parseArgs(os.Args[1:])
	if err != nil {
		return err
	}

	if *showVer {
		fmt.Println("grove", version)
		return nil
	}
	if *initConfig {
		path, err := config.WriteDefault()
		if errors.Is(err, fs.ErrExist) {
			fmt.Println("Config already exists:", path)
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Println("Wrote", path)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if *base != "" {
		cfg.Base = *base
	}
	if *mode != "" {
		cfg.Mode = *mode
	}
	m, err := git.ParseMode(cfg.Mode)
	if err != nil {
		return err
	}

	if err := git.Available(); err != nil {
		return err
	}
	dir := "."
	if len(paths) > 0 {
		dir = paths[0]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	repo, err := git.Open(ctx, dir)
	if errors.Is(err, git.ErrNotRepo) {
		return fmt.Errorf("%s is not inside a git repository\n\n  Run grove from a repository folder, or pass one:  grove C:\\path\\to\\repo", dir)
	}
	if err != nil {
		return err
	}

	if *plain {
		return printPlain(ctx, repo, cfg.Base, m)
	}

	p := tea.NewProgram(ui.New(ctx, ui.Options{
		Repo:    repo,
		Config:  cfg,
		Mode:    m,
		Base:    cfg.Base,
		Version: version,
	}), tea.WithContext(ctx))
	_, err = p.Run()
	if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// parseArgs parses flags wherever they appear, so `grove C:\repo --plain`
// works as well as `grove --plain C:\repo`. Go's flag package alone stops at
// the first non-flag argument.
func parseArgs(args []string) ([]string, error) {
	var paths []string
	for {
		if err := flag.CommandLine.Parse(args); err != nil {
			return nil, err
		}
		args = flag.Args()
		if len(args) == 0 {
			break
		}
		paths = append(paths, args[0])
		args = args[1:]
	}
	if len(paths) > 1 {
		return nil, fmt.Errorf("expected one path, got %d: %s", len(paths), strings.Join(paths, ", "))
	}
	return paths, nil
}

// printPlain writes a plain-text summary, handy for scripts and CI logs.
func printPlain(ctx context.Context, repo *git.Repo, baseOverride string, mode git.Mode) error {
	base, err := git.DetectBase(ctx, repo, baseOverride)
	if err != nil {
		return err
	}
	snap, err := git.Scan(ctx, repo, base, mode)
	if err != nil {
		return err
	}
	for _, st := range snap.States {
		head := fmt.Sprintf("%s  %s", st.Name(), st.Path)
		if st.Base != "" && st.Branch != st.Base {
			head += fmt.Sprintf("  ↑%d ↓%d", st.Ahead, st.Behind)
		}
		fmt.Println(head)
		if st.Err != nil {
			fmt.Println("    error:", st.Err)
			continue
		}
		for _, f := range st.Files {
			mark := ""
			if len(snap.Overlaps[f.Path]) > 1 {
				mark = "  ⚠ overlap"
			}
			counts := fmt.Sprintf("+%d -%d", f.Added, f.Deleted)
			if f.Binary {
				counts = "binary"
			}
			fmt.Printf("    %c %-50s %s%s\n", f.Status, f.Path, counts, mark)
		}
		fmt.Printf("    %d files  +%d -%d\n\n", len(st.Files), st.Added, st.Deleted)
	}
	if n := snap.OverlapCount(); n > 0 {
		var paths []string
		for p := range snap.Overlaps {
			paths = append(paths, p)
		}
		fmt.Printf("⚠ %d file(s) changed in more than one worktree: %s\n", n, strings.Join(paths, ", "))
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `grove: see every git worktree, what changed, and where they overlap

Usage:
  grove [flags] [path]

  path defaults to the current folder. It can be a worktree, any folder or
  file inside one, a .git folder, or a bare repository.

Flags:
`)
	flag.PrintDefaults()
	path, _ := config.Path()
	fmt.Fprintf(os.Stderr, "\nConfig file: %s  (create one with --init-config)\n", path)
}

// mcpGroup is the tool server grove gives a grouped agent: `grove
// mcp-group --dir <group folder> --as <member>`, speaking MCP on stdin and
// stdout. The agent's CLI starts it; people never run it.
func mcpGroup(args []string) error {
	fs := flag.NewFlagSet("mcp-group", flag.ContinueOnError)
	dir := fs.String("dir", "", "the group's folder")
	as := fs.String("as", "", "the member this agent is")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *as == "" {
		return errors.New("mcp-group needs --dir and --as")
	}
	return group.Serve(os.Stdin, os.Stdout, group.Log{Dir: *dir}, *as, version)
}
