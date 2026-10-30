package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestSetKey(t *testing.T) {
	cases := map[string]string{
		// Replace an existing value, keeping comments around it.
		"theme = \"dark\"\n\n[ai]\n# which one\nagent = \"\"\nmodel = \"\"\n": "theme = \"dark\"\n\n[ai]\n# which one\nagent = \"codex\"\nmodel = \"\"\n",
		// Add the key to an existing section that lacks it.
		"[ai]\nmodel = \"x\"\n\n[open]\n\".ts\" = \"code\"\n": "[ai]\nmodel = \"x\"\n\nagent = \"codex\"\n\n[open]\n\".ts\" = \"code\"\n",
		// Add the section.
		"theme = \"dark\"\n": "theme = \"dark\"\n\n[ai]\nagent = \"codex\"\n",
	}
	for in, want := range cases {
		if got := setKey(in, "ai", "agent", `"codex"`); got != want {
			t.Errorf("setKey(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	// A key with the same name in another section is left alone.
	got := setKey("[open]\nagent = \"x\"\n", "ai", "agent", `"claude"`)
	var c struct {
		Open map[string]string `toml:"open"`
		AI   AI                `toml:"ai"`
	}
	if _, err := toml.Decode(got, &c); err != nil || c.AI.Agent != "claude" || c.Open["agent"] != "x" {
		t.Errorf("sections mixed up: %q -> %+v, %v", got, c, err)
	}
	// The starter template stays valid TOML after setting the agent.
	var cfg Config
	if _, err := toml.Decode(setKey(Template, "ai", "agent", `"claude"`), &cfg); err != nil || cfg.AI.Agent != "claude" {
		t.Errorf("template: %+v, %v", cfg.AI, err)
	}
	if strings.Count(setKey(Template, "ai", "agent", `"claude"`), "[ai]") != 1 {
		t.Error("template gained a second [ai] section")
	}
}

func TestConflictsEnabled(t *testing.T) {
	if !(Conflicts{}).Enabled() {
		t.Error("prediction should be on when unset")
	}
	var cfg Config
	if _, err := toml.Decode(Template, &cfg); err != nil || !cfg.Conflicts.Enabled() {
		t.Errorf("template: %+v, %v", cfg.Conflicts, err)
	}
	cfg = Config{}
	if _, err := toml.Decode(setKey(Template, "conflicts", "predict", "false"), &cfg); err != nil || cfg.Conflicts.Enabled() {
		t.Errorf("predict = false: %+v, %v", cfg.Conflicts, err)
	}
}

func TestModelFor(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(setKey(Template, "ai", "claude_model", `"sonnet"`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.AI.ModelFor("claude") != "sonnet" || cfg.AI.ModelFor("codex") != "" {
		t.Errorf("models = %+v", cfg.AI)
	}
	// The older shared key still applies where an agent has none.
	ai := AI{Model: "old", CodexModel: "gpt"}
	if ai.ModelFor("claude") != "old" || ai.ModelFor("codex") != "gpt" {
		t.Errorf("fallback = %q, %q", ai.ModelFor("claude"), ai.ModelFor("codex"))
	}
}

func TestRoleModels(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(setKey(Template, "planner", "crew_model", `"sonnet"`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Planner.CrewModel != "sonnet" || cfg.Planner.Model != "" || cfg.Planner.TaskModel != "" {
		t.Fatalf("planner = %+v", cfg.Planner)
	}
	ai := AI{ClaudeModel: "opus", CodexModel: "gpt-5"}
	p := Planner{Model: "claude-opus-4-1", CrewModel: "sonnet", TaskModel: "o3"}
	cases := []struct{ role, agent, want string }{
		{RoleLead, "claude", "claude-opus-4-1"},
		{RoleLead, "codex", "gpt-5"}, // a Claude name isn't given to Codex
		{RoleCrew, "claude", "sonnet"},
		{RoleCrew, "codex", "gpt-5"},
		{RoleTask, "codex", "o3"},
		{RoleTask, "claude", "opus"}, // nor a Codex one to Claude
	}
	for _, c := range cases {
		if got := p.ModelFor(c.role, c.agent, ai); got != c.want {
			t.Errorf("%s/%s = %q, want %q", c.role, c.agent, got, c.want)
		}
	}
	// Empty falls back to [ai]; a name that doesn't tell applies to both;
	// a prefix says which agent explicitly.
	if got := (Planner{}).ModelFor(RoleCrew, "claude", ai); got != "opus" {
		t.Errorf("empty = %q", got)
	}
	p = Planner{CrewModel: "my-model", TaskModel: "codex:gpt-5-mini"}
	if p.ModelFor(RoleCrew, "claude", ai) != "my-model" || p.ModelFor(RoleCrew, "codex", ai) != "my-model" {
		t.Errorf("unknown name = %q, %q", p.ModelFor(RoleCrew, "claude", ai), p.ModelFor(RoleCrew, "codex", ai))
	}
	if p.ModelFor(RoleTask, "codex", ai) != "gpt-5-mini" || p.ModelFor(RoleTask, "claude", ai) != "opus" {
		t.Errorf("prefixed = %q, %q", p.ModelFor(RoleTask, "codex", ai), p.ModelFor(RoleTask, "claude", ai))
	}
	for name, want := range map[string]string{"Sonnet": "claude", "haiku": "claude", "opus[1m]": "claude", "gpt-5-codex": "codex", "o4-mini": "codex", "llama": ""} {
		if got := ModelAgent(name); got != want {
			t.Errorf("ModelAgent(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestGitProtected(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(Template, &cfg); err != nil {
		t.Fatal(err)
	}
	got := cfg.Git.ProtectedWith("develop")
	if want := []string{"main", "master", "develop", "release/*"}; !slices.Equal(got, want) {
		t.Errorf("protected = %q, want %q", got, want)
	}
	if got := (Git{Protected: []string{" master ", ""}}).ProtectedWith("refs/heads/main"); !slices.Equal(got, []string{"main", "master"}) {
		t.Errorf("duplicates kept: %q", got)
	}
	if !cfg.Planner.OpenPRs() || !(Planner{}).OpenPRs() {
		t.Error("pull requests should be on by default")
	}
	off := false
	if (Planner{PullRequests: &off}).OpenPRs() {
		t.Error("pull_requests = false ignored")
	}
}

func TestSettle(t *testing.T) {
	var cfg Config
	if _, err := toml.Decode(setKey(Template, "planner", "settle", `"0s"`), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Planner.SettleDelay(); got != 0 || cfg.Planner.PlannerSettle() != 0 {
		t.Errorf("settle 0 = %v", got)
	}
	if got := (Planner{}).SettleDelay(); got != 15*time.Second || (Planner{}).PlannerSettle() != 5*time.Second {
		t.Errorf("default = %v, planner %v", got, (Planner{}).PlannerSettle())
	}
	short := Planner{Settle: &Duration{2 * time.Second}}
	if short.SettleDelay() != 2*time.Second || short.PlannerSettle() != 2*time.Second {
		t.Errorf("2s = %v, planner %v", short.SettleDelay(), short.PlannerSettle())
	}
}

func TestTestTimeout(t *testing.T) {
	if got := (Planner{}).TestTimeout(); got != 20*time.Minute {
		t.Errorf("default = %v", got)
	}
	if got := (Planner{TestMinutes: 5}).TestTimeout(); got != 5*time.Minute {
		t.Errorf("5 = %v", got)
	}
}
