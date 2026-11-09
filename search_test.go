package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// searchHome sets up a machine named immunemac with one local session and an
// index mirror holding a session from the pc, and returns the mirror.
func searchHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_PROJECTS_DIR", "")
	t.Setenv("CLAUDE_SESSIONS_HOST", "immunemac")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	// The mirror is read as it is: these tests never sync it.
	t.Setenv("CLAUDE_SESSIONS_REMOTE", filepath.Join(home, "index"))
	index := filepath.Join(home, ".cache", "claude-sessions", "remote")
	os.MkdirAll(index, 0o755)

	project := filepath.Join(home, ".claude", "projects", "-Users-me-alpha")
	os.MkdirAll(project, 0o755)
	os.WriteFile(filepath.Join(project, "local-1.jsonl"), []byte(strings.Join([]string{
		`{"type":"user","cwd":"/Users/me/alpha","message":{"content":"how do I rotate the Rails master key?"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"},{"type":"text","text":"Copy it from the main worktree."}]}}`,
	}, "\n")+"\n"), 0o644)

	writeRecordFile(filepath.Join(index, "pc.jsonl"), []Record{{
		Host: "pc", ID: "remote-1", Cwd: "/home/me/beta", MTime: time.Now().Add(-time.Hour),
		First: "set up the GPU gate", Last: "done", Text: "set up the GPU gate\n\nthe master key lives in .env\n\ndone",
	}})
	return index
}

func search(t *testing.T, args ...string) (string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := runSearch(append([]string{"--no-sync"}, args...), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	return out.String(), errOut.String()
}

func TestSearchFindsEveryMachinesSessionsNewestFirst(t *testing.T) {
	searchHome(t)
	out, _ := search(t, "master", "KEY")
	local := strings.Index(out, "local-1  immunemac")
	remote := strings.Index(out, "remote-1  pc")
	if local < 0 || remote < 0 || local > remote {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(out, "match: how do I rotate the Rails master key?") {
		t.Errorf("no snippet:\n%s", out)
	}
	if !strings.Contains(out, "resume on pc: cd '/home/me/beta' && claude --resume 'remote-1'") {
		t.Errorf("no resume line:\n%s", out)
	}
}

func TestSearchNeedsEveryWord(t *testing.T) {
	searchHome(t)
	out, _ := search(t, "master", "gpu")
	if strings.Contains(out, "local-1") || !strings.Contains(out, "remote-1") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSearchNarrowsByHostAndDirectory(t *testing.T) {
	searchHome(t)
	if out, _ := search(t, "--host", "PC", "master"); strings.Contains(out, "local-1") || !strings.Contains(out, "remote-1") {
		t.Errorf("--host:\n%s", out)
	}
	// /Users/me/beta here is /home/me/beta on the pc: directories compare home-relative.
	if out, _ := search(t, "master", "--in", "/Users/me/beta"); strings.Contains(out, "local-1") || !strings.Contains(out, "remote-1") {
		t.Errorf("--in:\n%s", out)
	}
}

func TestSearchLeavesOutTheSessionRunningIt(t *testing.T) {
	searchHome(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "local-1")
	if out, _ := search(t, "master"); strings.Contains(out, "local-1") {
		t.Errorf("output:\n%s", out)
	}
	if out, _ := search(t, "--include-current", "master"); !strings.Contains(out, "local-1") {
		t.Errorf("--include-current:\n%s", out)
	}
}

func TestSearchJSONLeavesTheFullTextOut(t *testing.T) {
	searchHome(t)
	out, _ := search(t, "--json", "--limit", "1", "master")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	var h hit
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		t.Fatal(err)
	}
	if h.ID != "local-1" || h.Text != "" || h.Matches != 1 || h.Resume == "" {
		t.Errorf("hit = %+v", h)
	}
}

func TestShowPrintsALocalSessionWithRoles(t *testing.T) {
	searchHome(t)
	var out, errOut bytes.Buffer
	if code := runShow([]string{"local"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	want := "## user\n\nhow do I rotate the Rails master key?\n\n## assistant\n\nCopy it from the main worktree.\n"
	if out.String() != want {
		t.Errorf("got:\n%s", out.String())
	}
}

func TestShowFallsBackToTheIndexForAnUnreachableMachine(t *testing.T) {
	searchHome(t)
	showSSHTimeout = 5 * time.Second
	var out, errOut bytes.Buffer
	if code := runShow([]string{"--host", "nosuchhost.invalid", "remote-1"}, &out, &errOut); code != 1 {
		t.Errorf("a host that holds no such session: exit %d", code)
	}
	out.Reset()
	errOut.Reset()
	// The record says the pc; with no pc to reach, the index's copy is shown.
	t.Setenv("PATH", t.TempDir())
	if code := runShow([]string{"remote-1"}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "the master key lives in .env") || !strings.Contains(errOut.String(), "could not read it on pc") {
		t.Errorf("out:\n%s\nerr:\n%s", out.String(), errOut.String())
	}
}

func TestSnippetCutsAroundTheMatch(t *testing.T) {
	text := strings.Repeat("a ", 100) + "NEEDLE" + strings.Repeat(" b", 100)
	got := snippet(text, 200, 206, 10)
	if got != "… a a a a aNEEDLE b b b b b…" && got != "…a a a a a NEEDLE b b b b b…" {
		t.Errorf("snippet = %q", got)
	}
}
