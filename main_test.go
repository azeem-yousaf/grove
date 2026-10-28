package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionFrom(t *testing.T) {
	settings := func(rev, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: modified}}
	}
	for _, c := range []struct {
		ldflag string
		info   *debug.BuildInfo
		want   string
	}{
		{"0.3.0-abc1234", &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}, "0.3.0-abc1234"},
		{"dev", nil, "dev"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, "1.2.3"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261028215517-88b5d3f669ab+dirty"}}, "dev-88b5d3f-dirty"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261028215517-88b5d3f669ab"}}, "dev-88b5d3f"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings("88b5d3f669ab0000", "true")}, "dev-88b5d3f-dirty"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings("88b5d3f669ab0000", "false")}, "dev-88b5d3f"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev"},
	} {
		if got := versionFrom(c.ldflag, c.info); got != c.want {
			t.Errorf("versionFrom(%q, %+v) = %q, want %q", c.ldflag, c.info, got, c.want)
		}
	}
}
