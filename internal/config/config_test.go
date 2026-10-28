package config

import (
	"strings"
	"testing"

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
