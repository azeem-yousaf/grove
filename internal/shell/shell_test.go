package shell

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	dir := t.TempDir()
	ok := Run(context.Background(), dir, "echo hello && echo CI=$CI", 1000)
	if runtime.GOOS == "windows" {
		ok = Run(context.Background(), dir, "echo hello && echo CI=%CI%", 1000)
	}
	if !ok.OK() || !strings.Contains(string(ok.Output), "hello") || !strings.Contains(string(ok.Output), "CI=true") {
		t.Fatalf("ok = %+v, %q", ok, ok.Output)
	}
	fail := Run(context.Background(), dir, "echo broken && exit 3", 1000)
	if fail.OK() || fail.ExitCode != 3 || fail.Err != nil || !strings.Contains(string(fail.Output), "broken") {
		t.Fatalf("fail = %+v, %q", fail, fail.Output)
	}
	long := Run(context.Background(), dir, "echo 0123456789 && echo abcdefghij", 8)
	if got := strings.TrimSpace(string(long.Output)); len(long.Output) > 8 || !strings.HasSuffix(got, "hij") {
		t.Errorf("tail = %q", long.Output)
	}
}

func TestRunTimesOut(t *testing.T) {
	sleep := "sleep 30"
	if runtime.GOOS == "windows" {
		sleep = "ping -n 30 127.0.0.1 >NUL"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := Run(ctx, t.TempDir(), sleep, 100)
	if !r.TimedOut || r.OK() || time.Since(start) > 10*time.Second {
		t.Fatalf("r = %+v after %v", r, time.Since(start))
	}
}
