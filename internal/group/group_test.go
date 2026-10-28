package group

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLog(t *testing.T) {
	l := Log{Dir: Dir(t.TempDir(), "plan1", "checkout")}
	if msgs, err := l.All(); err != nil || len(msgs) != 0 {
		t.Fatalf("empty log: %v, %v", msgs, err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(l.Post(Message{From: "feature/api", Text: "POST /guest takes {email}"}))
	must(l.Post(Message{From: "feature/ui", To: "feature/api", Text: "Does it return the order id?"}))
	must(l.Post(Message{From: User, To: "feature/docs", Text: "docs only"}))
	if err := l.Post(Message{From: "x", Text: "  "}); err == nil {
		t.Error("an empty message should be refused")
	}

	// api sees ui's question but not its own post or one for someone else.
	unread, _ := l.Unread("feature/api")
	if len(unread) != 1 || unread[0].From != "feature/ui" {
		t.Fatalf("api unread = %+v", unread)
	}
	got, err := l.Read("feature/api", 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("Read = %+v, %v", got, err)
	}
	if again, _ := l.Read("feature/api", 0); len(again) != 0 {
		t.Errorf("read twice: %+v", again)
	}
	if l.Cursor("feature/api") != 3 || l.Cursor("feature/ui") != 0 {
		t.Errorf("cursors = %d, %d", l.Cursor("feature/api"), l.Cursor("feature/ui"))
	}

	// A read with a wait returns as soon as something arrives.
	go func() {
		time.Sleep(200 * time.Millisecond)
		l.Post(Message{From: "feature/ui", Text: "thanks"})
	}()
	start := time.Now()
	got, _ = l.Read("feature/api", 10*time.Second)
	if len(got) != 1 || got[0].Text != "thanks" || time.Since(start) > 5*time.Second {
		t.Errorf("waited read = %+v after %s", got, time.Since(start))
	}

	// A line cut short by a crash doesn't hide the others.
	f, _ := os.OpenFile(filepath.Join(l.Dir, "messages.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"from":"x","te`)
	f.Close()
	if all, err := l.All(); err != nil || len(all) != 4 {
		t.Errorf("All after a torn line = %d, %v", len(all), err)
	}
}

func TestConcurrentPosts(t *testing.T) {
	l := Log{Dir: t.TempDir()}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.Post(Message{From: fmt.Sprintf("m%d", i), Text: strings.Repeat("x", 5000)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if all, _ := l.All(); len(all) != 20 {
		t.Errorf("got %d messages, want 20", len(all))
	}
	if _, err := os.Stat(filepath.Join(l.Dir, "lock")); !os.IsNotExist(err) {
		t.Error("lock left behind")
	}
}

// session runs Serve on pipes and returns a function that sends one request
// and returns its response.
func session(t *testing.T, l Log, me string) func(method string, params any) map[string]any {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		Serve(inR, outW, l, me, "test")
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	rd := bufio.NewReader(outR)
	id := 0
	return func(method string, params any) map[string]any {
		t.Helper()
		id++
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		inW.Write(append(b, '\n'))
		line, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var resp map[string]any
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatal(err)
		}
		return resp
	}
}

func text(resp map[string]any) (string, bool) {
	r := resp["result"].(map[string]any)
	return r["content"].([]any)[0].(map[string]any)["text"].(string), r["isError"].(bool)
}

func TestServe(t *testing.T) {
	l := Log{Dir: t.TempDir()}
	if err := l.WriteInfo(Info{Plan: "Guest checkout", Name: "Checkout", Members: []Member{
		{Name: "feature/api", Task: "Guest API", Path: "/src/shop-api"},
		{Name: "feature/ui", Task: "Guest page"},
	}}); err != nil {
		t.Fatal(err)
	}
	ui := session(t, l, "feature/ui")

	init := ui("initialize", map[string]any{"protocolVersion": "2025-03-26"})
	if r := init["result"].(map[string]any); r["protocolVersion"] != "2025-03-26" || r["serverInfo"].(map[string]any)["name"] != "grove" {
		t.Errorf("initialize = %v", init)
	}
	list := ui("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	if len(list) != len(Tools) {
		t.Errorf("tools/list = %v", list)
	}

	got, failed := text(ui("tools/call", map[string]any{"name": "members", "arguments": map[string]any{}}))
	if failed || !strings.Contains(got, "feature/ui (you): Guest page · not started yet") || !strings.Contains(got, "worktree /src/shop-api") {
		t.Errorf("members = %q", got)
	}
	if _, failed := text(ui("tools/call", map[string]any{"name": "post", "arguments": map[string]any{"text": "hi", "to": "feature/nobody"}})); !failed {
		t.Error("posting to a stranger should fail")
	}
	if got, failed := text(ui("tools/call", map[string]any{"name": "post", "arguments": map[string]any{"text": "What does POST /guest return?", "to": "feature/api"}})); failed {
		t.Fatalf("post: %s", got)
	}

	api := session(t, l, "feature/api")
	got, _ = text(api("tools/call", map[string]any{"name": "read", "arguments": map[string]any{}}))
	if !strings.Contains(got, "feature/ui (to feature/api): What does POST /guest return?") {
		t.Errorf("api read = %q", got)
	}
	if got, _ := text(api("tools/call", map[string]any{"name": "read", "arguments": map[string]any{}})); got != "No new messages." {
		t.Errorf("second read = %q", got)
	}
	if resp := api("nope", nil); resp["error"] == nil {
		t.Errorf("unknown method = %v", resp)
	}
}
