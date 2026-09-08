package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// local is one unmanaged projects directory, which is what a machine without
// claude-swap has and what most of these tests care about.
func local(root string) []source {
	return []source{{root: root}}
}

// writeTranscript puts a transcript on disk and returns the projects root.
func writeTranscript(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, dir)
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(project, name+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFirstMessageSkipsBareCommandsAndReminders(t *testing.T) {
	root := writeTranscript(t, "-Users-me-proj", "abc",
		`{"type":"user","cwd":"/Users/me/proj","message":{"content":"/clear"}}`,
		`{"type":"user","message":{"content":"<system-reminder>noise</system-reminder>real question"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"an answer"}]}}`,
	)

	sessions, err := latestSessions(local(root), 20, scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].First != "real question" {
		t.Errorf("first message = %q", sessions[0].First)
	}
	if sessions[0].Last != "an answer" {
		t.Errorf("last message = %q", sessions[0].Last)
	}
	if sessions[0].Cwd != "/Users/me/proj" {
		t.Errorf("cwd = %q", sessions[0].Cwd)
	}
}

func TestSlashCommandWithArgumentsIsAFirstMessage(t *testing.T) {
	root := writeTranscript(t, "-tmp-x", "id1",
		`{"type":"user","message":{"content":"<command-name>/loop</command-name><command-args>5m ship it</command-args>"}}`,
	)

	sessions, err := latestSessions(local(root), 20, scope{})
	if err != nil {
		t.Fatal(err)
	}
	if sessions[0].First != "/loop 5m ship it" {
		t.Errorf("first message = %q", sessions[0].First)
	}
}

func TestMetaAndToolRecordsAreNotMessages(t *testing.T) {
	root := writeTranscript(t, "-tmp-x", "id1",
		`{"type":"user","isMeta":true,"message":{"content":"meta noise"}}`,
		`{"type":"summary","summary":"a summary"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"output"}]}}`,
		`{"type":"user","message":{"content":"the only message"}}`,
	)

	sessions, err := latestSessions(local(root), 20, scope{})
	if err != nil {
		t.Fatal(err)
	}
	if sessions[0].First != "the only message" || sessions[0].Last != "the only message" {
		t.Errorf("first=%q last=%q", sessions[0].First, sessions[0].Last)
	}
}

func TestCwdFallsBackToTheDirectoryName(t *testing.T) {
	root := writeTranscript(t, "-Users-me--config-app", "id1",
		`{"type":"user","message":{"content":"hi"}}`,
	)

	sessions, err := latestSessions(local(root), 20, scope{})
	if err != nil {
		t.Fatal(err)
	}
	if sessions[0].Cwd != "/Users/me/.config/app" {
		t.Errorf("cwd = %q", sessions[0].Cwd)
	}
}

func TestLatestSessionsAreNewestFirstAndLimited(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "-tmp-x")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"old", "mid", "new"} {
		path := filepath.Join(project, name+".jsonl")
		line := `{"type":"user","message":{"content":"` + name + `"}}` + "\n"
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	sessions, err := latestSessions(local(root), 2, scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	if sessions[0].ConvID != "new" || sessions[1].ConvID != "mid" {
		t.Errorf("order = %q, %q", sessions[0].ConvID, sessions[1].ConvID)
	}
}

// projects puts several transcripts on disk, one per project directory, and
// returns the root they share.
func projects(t *testing.T, dirs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for dir, cwd := range dirs {
		project := filepath.Join(root, dir)
		if err := os.MkdirAll(project, 0o755); err != nil {
			t.Fatal(err)
		}
		line := `{"type":"user","cwd":"` + cwd + `","message":{"content":"hi"}}` + "\n"
		if err := os.WriteFile(filepath.Join(project, dir+".jsonl"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestADirectoryLimitsTheListToItselfAndItsSubpaths(t *testing.T) {
	root := projects(t, map[string]string{
		"-Users-me-c-datacenters":        "/Users/me/c/datacenters",
		"-Users-me-c-datacenters-ui-kit": "/Users/me/c/datacenters/ui-kit",
		"-Users-me-c-datacenters2":       "/Users/me/c/datacenters2",
		"-Users-me-c-other":              "/Users/me/c/other",
	})

	within, err := newScope("/Users/me/c/datacenters/")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := latestSessions(local(root), 20, within)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(sessions))
	for _, s := range sessions {
		got = append(got, s.Cwd)
	}
	sort.Strings(got)
	want := []string{"/Users/me/c/datacenters", "/Users/me/c/datacenters/ui-kit"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("scoped sessions = %v, want %v", got, want)
	}
}

// A dash in a directory name encodes the same way a separator does, so the
// cheap directory match lets neighbours through and the recorded cwd has to
// throw them out.
func TestScopingRejectsPathsThatOnlyEncodeAlike(t *testing.T) {
	root := projects(t, map[string]string{
		"-Users-me-c-claude-sessions": "/Users/me/c/claude-sessions",
	})

	within, err := newScope("/Users/me/c/claude")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := latestSessions(local(root), 20, within)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Errorf("got %d sessions, want none: %v", len(sessions), sessions)
	}
}

func TestScopeExpandsHomeAndRelativeDirectories(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	within, err := newScope("~/c/datacenters")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "c/datacenters"); within.path != want {
		t.Errorf("scope path = %q, want %q", within.path, want)
	}

	relative, err := newScope(".")
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if relative.path != cwd {
		t.Errorf("scope path = %q, want %q", relative.path, cwd)
	}
}

// The limit is meant to cap what is shown, not to hide a project's older
// sessions behind newer ones from elsewhere.
func TestScopingHappensBeforeTheLimit(t *testing.T) {
	root := t.TempDir()
	newer := filepath.Join(root, "-Users-me-c-other")
	wanted := filepath.Join(root, "-Users-me-c-mine")
	for _, dir := range []string{newer, wanted} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, cwd string, age time.Duration) {
		path := filepath.Join(dir, name+".jsonl")
		line := `{"type":"user","cwd":"` + cwd + `","message":{"content":"hi"}}` + "\n"
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write(newer, "fresh", "/Users/me/c/other", time.Minute)
	write(wanted, "stale", "/Users/me/c/mine", time.Hour)

	within, err := newScope("/Users/me/c/mine")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := latestSessions(local(root), 1, within)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ConvID != "stale" {
		t.Errorf("scoped sessions = %v, want the one in scope", sessions)
	}
}

func TestResumeCommandQuotesPaths(t *testing.T) {
	got := resumeCommand(Session{ConvID: "abc", Cwd: "/tmp/it's here"})
	want := `cd '/tmp/it'\''s here' && claude --resume 'abc'`
	if got != want {
		t.Errorf("resume command = %q, want %q", got, want)
	}
}

func TestReportCwdWritesTheDirectoryForTheShellFunction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cwd")
	t.Setenv(cwdFileEnv, path)

	reportCwd("/tmp/it's here")

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading reported cwd: %v", err)
	}
	if string(got) != "/tmp/it's here\n" {
		t.Errorf("reported cwd = %q", got)
	}
}

func TestReportCwdIsANoOpWithoutTheShellFunction(t *testing.T) {
	t.Setenv(cwdFileEnv, "")
	reportCwd("/tmp/anywhere") // Must not panic or write anything.
}

func TestWrapLinesFillsExactlyTheRequestedHeight(t *testing.T) {
	lines := wrapLines("a short one", 20, 3)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if lines[0] != "a short one" || lines[1] != "" || lines[2] != "" {
		t.Errorf("lines = %q", lines)
	}
}

func TestWrapLinesMarksTruncation(t *testing.T) {
	lines := wrapLines(strings.Repeat("word ", 40), 10, 2)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if !strings.HasSuffix(lines[1], "…") {
		t.Errorf("last line %q is not marked as truncated", lines[1])
	}
	for _, line := range lines {
		if len([]rune(line)) > 10 {
			t.Errorf("line %q is wider than the column", line)
		}
	}
}

// ansiCodes matches the colour escapes lipgloss writes, so tests can measure
// what a line actually occupies on screen.
var ansiCodes = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func TestSelectedRowIsFilledToTheScreenWidth(t *testing.T) {
	m := newModel([]Session{{ConvID: "1", Cwd: "/a", First: "short", Last: "also short"}}, scope{})
	m.width = 100

	for _, line := range strings.Split(strings.TrimRight(m.renderRow(m.sessions[0], true), "\n"), "\n") {
		plain := ansiCodes.ReplaceAllString(line, "")
		if width := len([]rune(plain)); width != m.width {
			t.Errorf("selected line is %d wide, want %d: %q", width, m.width, plain)
		}
	}
}

// Segments are rendered one after another, so a selected style that forgets
// the background would punch a hole in the highlighted row.
func TestEverySelectedStyleCarriesTheSelectionBackground(t *testing.T) {
	styles := map[string]lipgloss.Style{
		"row":     selectedRow,
		"bar":     selectedBar,
		"path":    selectedPath,
		"age":     selectedAge,
		"first":   selectedFirst,
		"last":    selectedLast,
		"account": selectedAccount,
	}
	for name, style := range styles {
		if style.GetBackground() != selectionBg {
			t.Errorf("selected %s style background = %v, want %v", name, style.GetBackground(), selectionBg)
		}
	}
}

func TestUnselectedRowHasNoSelectionBar(t *testing.T) {
	m := newModel([]Session{{ConvID: "1", Cwd: "/a", First: "short", Last: "also short"}}, scope{})
	m.width = 100

	if row := m.renderRow(m.sessions[0], false); strings.Contains(row, "▌") {
		t.Errorf("unselected row drew the selection bar: %q", row)
	}
}

// press sends one key to the model and returns the model that came back.
func press(t *testing.T, m model, key tea.KeyMsg) model {
	t.Helper()
	next, _ := m.Update(key)
	updated, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", next)
	}
	return updated
}

func key(s string) tea.KeyMsg {
	switch s {
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func previewModel() model {
	m := newModel([]Session{
		{ConvID: "1", Cwd: "/a", First: "the question", Last: "the answer"},
		{ConvID: "2", Cwd: "/b", First: "another", Last: "reply"},
	}, scope{})
	m.width, m.height, m.viewport = 100, 30, 28
	return m
}

func TestTabSwitchesBetweenListAndPreview(t *testing.T) {
	m := press(t, previewModel(), key("tab"))
	if !m.preview {
		t.Fatal("tab did not open the preview")
	}
	if view := m.View(); !strings.Contains(view, "first message") {
		t.Errorf("preview is missing its column labels: %q", view)
	}

	if m = press(t, m, key("tab")); m.preview {
		t.Error("tab did not go back to the list")
	}
	if m = press(t, press(t, m, key("tab")), key("esc")); m.preview {
		t.Error("esc did not close the preview")
	}
}

func TestTabSwitchesViewsWhileFiltering(t *testing.T) {
	m := press(t, press(t, previewModel(), key("/")), key("tab"))
	if !m.preview || !m.searching {
		t.Fatalf("preview = %v, searching = %v; want both open", m.preview, m.searching)
	}

	// Typing keeps filtering, and the preview follows the selection.
	m = press(t, m, key("another"))
	if m.query != "another" {
		t.Errorf("query = %q, want %q", m.query, "another")
	}
	if view := m.View(); !strings.Contains(view, "reply") {
		t.Errorf("preview does not show the only match: %q", view)
	}

	if m = press(t, m, key("tab")); m.preview {
		t.Error("tab did not go back to the list while filtering")
	}
	if !m.searching {
		t.Error("going back to the list dropped out of search mode")
	}
}

func TestPreviewFillsTheScreenAndKeepsEnter(t *testing.T) {
	m := press(t, previewModel(), key("tab"))

	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Errorf("preview drew %d lines, want %d", len(lines), m.height)
	}
	if !strings.Contains(m.View(), "the question") || !strings.Contains(m.View(), "the answer") {
		t.Error("preview does not show both messages")
	}

	resumed := press(t, m, key("enter"))
	if resumed.chosen == nil || resumed.chosen.ConvID != "1" {
		t.Errorf("enter in the preview chose %v", resumed.chosen)
	}
}

func TestSearchModeStillTypesSpaces(t *testing.T) {
	m := press(t, press(t, previewModel(), key("/")), key("a"))
	m = press(t, m, key(" "))
	m = press(t, m, key("b"))

	if m.preview {
		t.Error("space opened the preview while searching")
	}
	if m.query != "a b" {
		t.Errorf("query = %q, want %q", m.query, "a b")
	}
}

func TestFilterMatchesEitherMessageOrPath(t *testing.T) {
	m := newModel([]Session{
		{ConvID: "1", Cwd: "/a", First: "fix the parser", Last: "done"},
		{ConvID: "2", Cwd: "/b/webapp", First: "hello", Last: "bye"},
	}, scope{})

	m.query = "PARSER"
	m.applyFilter()
	if len(m.filtered) != 1 || m.filtered[0].ConvID != "1" {
		t.Errorf("message filter kept %d sessions", len(m.filtered))
	}

	m.query = "webapp"
	m.applyFilter()
	if len(m.filtered) != 1 || m.filtered[0].ConvID != "2" {
		t.Errorf("path filter kept %d sessions", len(m.filtered))
	}

	m.query = "nothing here"
	m.applyFilter()
	if len(m.filtered) != 0 || m.cursor != 0 {
		t.Errorf("empty filter left %d sessions, cursor %d", len(m.filtered), m.cursor)
	}
}
