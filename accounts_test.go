package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// swapHome builds a home directory holding an unmanaged ~/.claude plus the
// claude-swap accounts named, each with one transcript in the same project.
func swapHome(t *testing.T, registry string, accounts map[string]string) string {
	t.Helper()
	home := t.TempDir()

	write := func(root, id, cwd string, age time.Duration) {
		project := filepath.Join(root, "projects", "-Users-me-proj")
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(project, id+".jsonl")
		line := `{"type":"user","cwd":"` + cwd + `","message":{"content":"` + id + `"}}` + "\n"
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(home, ".claude"), "unmanaged", "/Users/me/proj", 4*time.Hour)
	age := 3 * time.Hour
	for dir, id := range accounts {
		write(filepath.Join(home, swapDir, "sessions", dir), id, "/Users/me/proj", age)
		age -= time.Hour
	}
	if registry != "" {
		path := filepath.Join(home, swapDir, "sequence.json")
		if err := os.WriteFile(path, []byte(registry), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

const twoAccounts = `{"activeAccountNumber":1,"accounts":{
	"1":{"email":"me@work.com"},
	"2":{"email":"me@home.com"}
}}`

func TestSessionsComeFromEveryAccount(t *testing.T) {
	home := swapHome(t, twoAccounts, map[string]string{
		"1-me_work.com": "work",
		"2-me_home.com": "home",
	})

	sessions, err := latestSessions(discoverSources(home), 20, scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Fatalf("got %d sessions, want one per account plus the unmanaged one: %v", len(sessions), sessions)
	}

	// Newest first, across the accounts rather than a run of each.
	want := []struct {
		id      string
		account account
	}{
		{"home", account{number: 2, email: "me@home.com"}},
		{"work", account{number: 1, email: "me@work.com"}},
		{"unmanaged", account{}},
	}
	for i, w := range want {
		if sessions[i].ConvID != w.id || sessions[i].Account != w.account {
			t.Errorf("session %d = %q from %v, want %q from %v",
				i, sessions[i].ConvID, sessions[i].Account, w.id, w.account)
		}
	}
}

// The slot registry is the exact source of an address; a directory name only
// approximates one, since it flattens the `@` into an underscore.
func TestAccountEmailFallsBackToTheDirectoryName(t *testing.T) {
	home := swapHome(t, "", map[string]string{"3-first_last_example.com": "solo"})

	sessions, err := latestSessions(discoverSources(home), 20, scope{})
	if err != nil {
		t.Fatal(err)
	}
	var found *Session
	for i := range sessions {
		if sessions[i].ConvID == "solo" {
			found = &sessions[i]
		}
	}
	if found == nil {
		t.Fatal("the account's session was not listed")
	}
	if want := (account{number: 3, email: "first@last_example.com"}); found.Account != want {
		t.Errorf("account = %v, want %v", found.Account, want)
	}
}

func TestHomeWithoutClaudeSwapHasOnlyTheUnmanagedSource(t *testing.T) {
	home := t.TempDir()

	sources := discoverSources(home)
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1: %v", len(sources), sources)
	}
	if want := filepath.Join(home, ".claude", "projects"); sources[0].root != want {
		t.Errorf("source root = %q, want %q", sources[0].root, want)
	}
	if sources[0].account != (account{}) {
		t.Errorf("unmanaged source claims account %v", sources[0].account)
	}
}

func TestResumeRunsAnAccountSessionThroughCswap(t *testing.T) {
	got := resumeCommand(Session{
		ConvID:  "abc",
		Cwd:     "/tmp/proj",
		Account: account{number: 2, email: "me@home.com"},
	})
	want := `cd '/tmp/proj' && cswap run 2 -- --resume 'abc'`
	if got != want {
		t.Errorf("resume command = %q, want %q", got, want)
	}
}

func TestRowAndFilterShowTheAccount(t *testing.T) {
	m := newModel([]Session{
		{ConvID: "1", Cwd: "/a", First: "one", Last: "done", Account: account{number: 1, email: "me@work.com"}},
		{ConvID: "2", Cwd: "/b", First: "two", Last: "done"},
	}, scope{})
	m.width = 100

	row := ansiCodes.ReplaceAllString(m.renderRow(m.sessions[0], false), "")
	if !strings.Contains(row, "me@work.com") {
		t.Errorf("row does not name the account: %q", row)
	}
	if plain := ansiCodes.ReplaceAllString(m.renderRow(m.sessions[1], false), ""); strings.Contains(plain, "@") {
		t.Errorf("unmanaged row invented an account: %q", plain)
	}

	m.query = "work.com"
	m.applyFilter()
	if len(m.filtered) != 1 || m.filtered[0].ConvID != "1" {
		t.Errorf("account filter kept %d sessions", len(m.filtered))
	}
}

// The account and the age are short and unreadable when cut, so the path is
// what gives way — and the line still has to fit the screen.
func TestTheAccountDoesNotPushTheHeaderPastTheScreen(t *testing.T) {
	m := newModel([]Session{{
		ConvID:  "1",
		Cwd:     "/Users/me/" + strings.Repeat("deep/", 30) + "project",
		First:   "one",
		Last:    "done",
		Account: account{number: 1, email: "me@work.com"},
	}}, scope{})
	m.width = 80

	for _, line := range strings.Split(strings.TrimRight(m.renderRow(m.sessions[0], true), "\n"), "\n") {
		plain := ansiCodes.ReplaceAllString(line, "")
		if width := len([]rune(plain)); width != m.width {
			t.Errorf("line is %d wide, want %d: %q", width, m.width, plain)
		}
	}
}
