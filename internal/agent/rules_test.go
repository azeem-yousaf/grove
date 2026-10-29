package agent

import (
	"slices"
	"testing"
)

func TestClaudePrefixRule(t *testing.T) {
	var got []string
	for _, c := range CommandParts("go vet ./... && go test ./... -count=1") {
		r, ok := ClaudePrefixRule("Bash", c)
		if !ok {
			t.Fatalf("no prefix rule for %q", c)
		}
		got = append(got, r)
	}
	if want := []string{"Bash(go vet:*)", "Bash(go test:*)"}; !slices.Equal(got, want) {
		t.Errorf("rules = %q, want %q", got, want)
	}
	for c, want := range map[string]string{
		"npm test":                  "Bash(npm test:*)",
		"gofmt -l .":                "",
		"go.exe test -run X ./...":  "Bash(go.exe test:*)",
		"cargo test --all":          "Bash(cargo test:*)",
		"make":                      "",
		"sh -c 'go test'":           "",
		"sh run.sh":                 "",
		"bash test.sh":              "",
		"python -m pytest":          "",
		"python3 manage.py test":    "",
		"node test.js":              "",
		"npx jest":                  "",
		"env FOO=1 go test":         "",
		"git status":                "",
		"git commit -m x":           "",
		"./run tests":               "",
		"go $(rm -rf x)":            "",
		"go `rm`":                   "",
		"PowerShell.exe -Command x": "",
		"go run ./cmd/x":            "",
		"go generate ./...":         "",
		"npm exec jest":             "",
		"cargo run --release":       "",
		"uv run pytest":             "",
		"Go.exe Run main.go":        "",
	} {
		r, _ := ClaudePrefixRule("Bash", c)
		if r != want {
			t.Errorf("ClaudePrefixRule(%q) = %q, want %q", c, r, want)
		}
	}
}

func TestCommandParts(t *testing.T) {
	got := CommandParts("go vet ./... && go test ./... ; gofmt -l . || true\nnpm test")
	want := []string{"go vet ./...", "go test ./...", "gofmt -l .", "true", "npm test"}
	if !slices.Equal(got, want) {
		t.Errorf("parts = %q, want %q", got, want)
	}
}
