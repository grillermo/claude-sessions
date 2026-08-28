package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

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

	sessions, err := latestSessions(root, 20)
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

	sessions, err := latestSessions(root, 20)
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

	sessions, err := latestSessions(root, 20)
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

	sessions, err := latestSessions(root, 20)
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

	sessions, err := latestSessions(root, 2)
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

func TestResumeCommandQuotesPaths(t *testing.T) {
	got := resumeCommand(Session{ConvID: "abc", Cwd: "/tmp/it's here"})
	want := `cd '/tmp/it'\''s here' && claude --resume 'abc'`
	if got != want {
		t.Errorf("resume command = %q, want %q", got, want)
	}
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
	m := newModel([]Session{{ConvID: "1", Cwd: "/a", First: "short", Last: "also short"}})
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
		"row":   selectedRow,
		"bar":   selectedBar,
		"path":  selectedPath,
		"age":   selectedAge,
		"first": selectedFirst,
		"last":  selectedLast,
	}
	for name, style := range styles {
		if style.GetBackground() != selectionBg {
			t.Errorf("selected %s style background = %v, want %v", name, style.GetBackground(), selectionBg)
		}
	}
}

func TestUnselectedRowHasNoSelectionBar(t *testing.T) {
	m := newModel([]Session{{ConvID: "1", Cwd: "/a", First: "short", Last: "also short"}})
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
	})
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
	})

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
