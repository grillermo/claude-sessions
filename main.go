// claude-sessions lists the latest Claude Code sessions and resumes one.
//
// Each row is one session: its first message on the left, its last message on
// the right. Arrow keys move, Tab switches between the list and a full-screen
// preview of the selection, `/` filters as you type, Esc leaves the filter,
// Enter replaces this process with `claude --resume` so the session takes over
// the terminal the TUI was started in, and `q` quits.
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	sessionLimit = 40
	// Roughly 80 pixels of text per row on a normal terminal font, so a
	// screenful holds many sessions at once.
	messageLines = 3
	rowHeight    = messageLines + 1 // message lines plus the header line
	columnGap    = 3
)

// selectionBg is saturated rather than a dim grey so the selected row stands
// out on both light and dark terminal themes.
var selectionBg = lipgloss.Color("62")

var (
	plainStyle   = lipgloss.NewStyle()
	headerStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	pathStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	ageStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	firstStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	lastStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	accountStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))

	// Every selected style shares the background: segments are rendered one
	// after another, so each has to paint its own stretch of the row.
	selectedRow     = lipgloss.NewStyle().Background(selectionBg)
	selectedBar     = selectedRow.Foreground(lipgloss.Color("213")).Bold(true)
	selectedPath    = selectedRow.Foreground(lipgloss.Color("231")).Bold(true)
	selectedAge     = selectedRow.Foreground(lipgloss.Color("189"))
	selectedFirst   = selectedRow.Foreground(lipgloss.Color("231"))
	selectedLast    = selectedRow.Foreground(lipgloss.Color("252"))
	selectedAccount = selectedRow.Foreground(lipgloss.Color("219"))

	labelStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	footerStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	searchStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	emptyStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	placeholderTxt = "(no message)"
)

type model struct {
	sessions []Session
	filtered []Session
	cursor   int
	offset   int
	query    string
	viewport int // rows of screen available for the session list
	width    int
	height   int
	now      time.Time
	// searching is filter-input mode: every rune typed goes to the query.
	searching bool
	// preview is the full-screen view of the selected session.
	preview bool
	// scope is the directory the list was limited to, shown in the title so it
	// is clear the list is not everything.
	scope scope
	// chosen is the session Enter picked, resumed after the TUI shuts down.
	chosen *Session
}

func newModel(sessions []Session, within scope) model {
	m := model{sessions: sessions, scope: within, now: time.Now(), width: 80, height: 24}
	m.applyFilter()
	return m
}

// applyFilter narrows the list to sessions matching the query and keeps the
// cursor inside the result.
func (m *model) applyFilter() {
	query := strings.ToLower(strings.TrimSpace(m.query))
	if query == "" {
		m.filtered = m.sessions
	} else {
		matched := make([]Session, 0, len(m.sessions))
		for _, s := range m.sessions {
			haystack := strings.ToLower(s.First + "\n" + s.Last + "\n" + s.Cwd + "\n" + s.Account.label())
			if strings.Contains(haystack, query) {
				matched = append(matched, s)
			}
		}
		m.filtered = matched
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.scrollIntoView()
}

// scrollIntoView keeps the selected row on screen.
func (m *model) scrollIntoView() {
	visible := m.visibleRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+visible {
		m.offset = m.cursor - visible + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// visibleRows is how many session rows fit in the list area.
func (m model) visibleRows() int {
	rows := m.viewport / rowHeight
	if rows < 1 {
		return 1
	}
	return rows
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Two lines of chrome: the title and the footer.
		m.viewport = msg.Height - 2
		if m.viewport < rowHeight {
			m.viewport = rowHeight
		}
		m.scrollIntoView()
		return m, nil

	case tea.KeyMsg:
		if m.searching {
			return m.updateSearch(msg)
		}
		return m.updateList(msg)
	}
	return m, nil
}

// updateSearch handles keys while the filter is being typed.
func (m model) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.searching = false
		return m, nil
	case tea.KeyEnter:
		if session := m.selected(); session != nil {
			m.chosen = session
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyBackspace:
		if runes := []rune(m.query); len(runes) > 0 {
			m.query = string(runes[:len(runes)-1])
			m.applyFilter()
		}
		return m, nil
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyUp:
		m.moveCursor(-1)
		return m, nil
	case tea.KeyDown:
		m.moveCursor(1)
		return m, nil
	case tea.KeyTab:
		// Tab switches views while filtering too; nothing here completes
		// anything, so it is free.
		m.togglePreview()
		return m, nil
	case tea.KeyRunes, tea.KeySpace:
		// A space arrives as its own key type, but still carries its rune.
		m.query += string(msg.Runes)
		m.applyFilter()
		return m, nil
	}
	return m, nil
}

// updateList handles keys while navigating the sessions, in the list and in
// the full-screen preview alike: the preview is the same selection seen larger,
// so every key keeps doing what it did.
func (m model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.togglePreview()
		return m, nil
	case "/":
		m.searching = true
		return m, nil
	case "esc":
		if m.preview {
			m.preview = false
			return m, nil
		}
		if m.query != "" {
			m.query = ""
			m.applyFilter()
		}
		return m, nil
	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "pgup":
		m.moveCursor(-m.visibleRows())
	case "pgdown":
		m.moveCursor(m.visibleRows())
	case "home", "g":
		m.moveCursor(-len(m.filtered))
	case "end", "G":
		m.moveCursor(len(m.filtered))
	case "enter":
		if session := m.selected(); session != nil {
			m.chosen = session
			return m, tea.Quit
		}
	}
	return m, nil
}

// togglePreview switches between the list and the full-screen preview. With
// nothing selected there is nothing to preview, so the list stays.
func (m *model) togglePreview() {
	m.preview = !m.preview && m.selected() != nil
}

func (m *model) moveCursor(delta int) {
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.scrollIntoView()
}

func (m model) selected() *Session {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return nil
	}
	session := m.filtered[m.cursor]
	return &session
}

func (m model) View() string {
	if session := m.selected(); m.preview && session != nil {
		return m.previewView(*session)
	}

	var b strings.Builder
	b.WriteString(m.titleLine() + "\n")

	if len(m.filtered) == 0 {
		b.WriteString(emptyStyle.Render("No sessions match.") + "\n")
	} else {
		visible := m.visibleRows()
		for i := m.offset; i < len(m.filtered) && i < m.offset+visible; i++ {
			b.WriteString(m.renderRow(m.filtered[i], i == m.cursor))
		}
	}

	b.WriteString(m.footerLine())
	return b.String()
}

// previewFooter reminds the reader that the filter is still live when the
// preview was opened from search mode.
func (m model) previewFooter() string {
	if m.searching {
		return footerStyle.Render("type to filter · tab list · ↑↓ session · enter resume · esc leave search")
	}
	return footerStyle.Render("tab list · ↑↓ session · enter resume · q quit")
}

// previewView shows one session on the whole screen: the same two columns as a
// list row, given every line the terminal has.
func (m model) previewView(s Session) string {
	columnWidth := (m.width - columnGap) / 2
	if columnWidth < 8 {
		columnWidth = 8
	}
	// The title, the column labels, and the footer.
	bodyHeight := m.height - 3
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	left := wrapLines(orPlaceholder(s.First), columnWidth, bodyHeight)
	right := wrapLines(orPlaceholder(s.Last), columnWidth, bodyHeight)
	gap := strings.Repeat(" ", columnGap)

	var b strings.Builder
	head := pathStyle.Render(truncate(s.Cwd, m.width-24))
	if account := s.Account.label(); account != "" {
		head += "  " + accountStyle.Render(account)
	}
	head += "  " + ageStyle.Render(timeAgo(m.now.Sub(s.MTime)))
	// The filter stays live behind the preview, so it stays on screen too.
	if m.query != "" || m.searching {
		head += "  " + searchStyle.Render("/"+m.query+cursorMark(m.searching))
	}
	b.WriteString(head + "\n")
	b.WriteString(labelStyle.Render(pad("first message", columnWidth)) + gap +
		labelStyle.Render("last message") + "\n")
	for i := 0; i < bodyHeight; i++ {
		b.WriteString(firstStyle.Render(pad(left[i], columnWidth)) + gap +
			lastStyle.Render(right[i]) + "\n")
	}
	b.WriteString(m.previewFooter())
	return b.String()
}

func (m model) titleLine() string {
	count := fmt.Sprintf("%d/%d sessions", len(m.filtered), len(m.sessions))
	title := headerStyle.Render("Claude sessions") + "  " + ageStyle.Render(count)
	if m.scope.path != "" {
		title += "  " + pathStyle.Render("in "+m.scope.path)
	}
	if m.query != "" || m.searching {
		title += "  " + searchStyle.Render("/"+m.query+cursorMark(m.searching))
	}
	return title
}

func cursorMark(active bool) string {
	if active {
		return "▏"
	}
	return ""
}

func (m model) footerLine() string {
	if m.searching {
		return footerStyle.Render("type to filter · ↑↓ move · tab preview · enter resume · esc leave search")
	}
	return footerStyle.Render("↑↓ move · tab preview · enter resume · / search · q quit")
}

// segment is one run of text on a row line, styled one way when the row is
// selected and another when it is not.
type segment struct {
	text     string
	normal   lipgloss.Style
	selected lipgloss.Style
}

// renderRow draws one session: a header line, then the first and last messages
// side by side. A selected row is filled to the edge of the screen with the
// selection background, so the whole block reads as one highlighted entry.
func (m model) renderRow(s Session, selected bool) string {
	// The selection bar and the gap between columns eat into the text width.
	bodyWidth := m.width - 2
	if bodyWidth < 20 {
		bodyWidth = 20
	}
	columnWidth := (bodyWidth - columnGap) / 2
	if columnWidth < 8 {
		columnWidth = 8
	}

	left := wrapLines(orPlaceholder(s.First), columnWidth, messageLines)
	right := wrapLines(orPlaceholder(s.Last), columnWidth, messageLines)
	gap := strings.Repeat(" ", columnGap)

	var b strings.Builder
	b.WriteString(m.renderLine(selected, m.headerSegments(s, bodyWidth, selected)...))
	for i := 0; i < messageLines; i++ {
		b.WriteString(m.renderLine(selected,
			m.gutter(selected),
			segment{pad(left[i], columnWidth), firstStyle, selectedFirst},
			segment{gap, plainStyle, selectedRow},
			segment{right[i], lastStyle, selectedLast},
		))
	}
	return b.String()
}

// headerSegments is a row's first line: where the session ran, which account it
// belongs to, and how long ago it was touched. The path gives way to the other
// two, since they are short and neither survives being cut.
func (m model) headerSegments(s Session, width int, selected bool) []segment {
	account := s.Account.label()
	room := width - 14
	if account != "" {
		room -= len([]rune(account)) + 2
	}

	segments := []segment{
		m.gutter(selected),
		{truncate(s.Cwd, room), pathStyle, selectedPath},
	}
	if account != "" {
		segments = append(segments,
			segment{"  ", plainStyle, selectedRow},
			segment{account, accountStyle, selectedAccount})
	}
	return append(segments,
		segment{"  ", plainStyle, selectedRow},
		segment{timeAgo(m.now.Sub(s.MTime)), ageStyle, selectedAge})
}

// renderLine styles one line's segments and pads it to the full screen width,
// which is what carries the selection background across the row.
func (m model) renderLine(selected bool, segments ...segment) string {
	var b strings.Builder
	width := 0
	for _, s := range segments {
		style := s.normal
		if selected {
			style = s.selected
		}
		b.WriteString(style.Render(s.text))
		width += len([]rune(s.text))
	}
	if filler := m.width - width; filler > 0 {
		spaces := strings.Repeat(" ", filler)
		if selected {
			spaces = selectedRow.Render(spaces)
		}
		b.WriteString(spaces)
	}
	return b.String() + "\n"
}

// gutter is the selection bar and its space, drawn on every line of a row.
func (m model) gutter(selected bool) segment {
	if selected {
		return segment{"▌ ", plainStyle, selectedBar}
	}
	return segment{"  ", plainStyle, plainStyle}
}

func orPlaceholder(text string) string {
	if strings.TrimSpace(text) == "" {
		return placeholderTxt
	}
	return text
}

// wrapLines breaks text into exactly `lines` display rows of at most `width`
// runes, marking truncation with an ellipsis.
func wrapLines(text string, width, lines int) []string {
	out := make([]string, 0, lines)
	source := strings.Split(strings.ReplaceAll(text, "\t", "    "), "\n")
	truncated := false

	for _, paragraph := range source {
		paragraph = strings.TrimRight(paragraph, " \r")
		if paragraph == "" {
			continue
		}
		for _, chunk := range wrapWords(paragraph, width) {
			if len(out) == lines {
				truncated = true
				break
			}
			out = append(out, chunk)
		}
		if len(out) == lines {
			truncated = true
			break
		}
	}

	if truncated && len(out) > 0 {
		out[len(out)-1] = withEllipsis(out[len(out)-1], width)
	}
	for len(out) < lines {
		out = append(out, "")
	}
	return out
}

// wrapWords greedily wraps one paragraph at word boundaries, splitting words
// that are wider than the column.
func wrapWords(paragraph string, width int) []string {
	var lines []string
	var line []rune
	for _, word := range strings.Fields(paragraph) {
		runes := []rune(word)
		if len(line) > 0 && len(line)+1+len(runes) > width {
			lines = append(lines, string(line))
			line = nil
		}
		for len(runes) > width {
			if len(line) > 0 {
				lines = append(lines, string(line))
				line = nil
			}
			lines = append(lines, string(runes[:width]))
			runes = runes[width:]
		}
		if len(line) > 0 {
			line = append(line, ' ')
		}
		line = append(line, runes...)
	}
	if len(line) > 0 {
		lines = append(lines, string(line))
	}
	return lines
}

// withEllipsis marks a line as continuing past what is shown.
func withEllipsis(line string, width int) string {
	runes := []rune(line)
	if len(runes) >= width && width > 0 {
		runes = runes[:width-1]
	}
	return string(runes) + "…"
}

func truncate(text string, width int) string {
	runes := []rune(text)
	if width <= 0 || len(runes) <= width {
		return text
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func pad(text string, width int) string {
	if gap := width - len([]rune(text)); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}

// projectSources is where transcripts are read from: the one directory named
// by CLAUDE_PROJECTS_DIR when it is set, and otherwise every account's.
func projectSources() ([]source, error) {
	if root := os.Getenv("CLAUDE_PROJECTS_DIR"); root != "" {
		return []source{{root: root}}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return discoverSources(home), nil
}

func main() {
	sources, err := projectSources()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		os.Exit(1)
	}

	// The one optional argument is a directory: only sessions run there or
	// below it are listed.
	var arg string
	if args := os.Args[1:]; len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions [directory]")
		os.Exit(1)
	} else if len(args) == 1 {
		if arg = args[0]; strings.HasPrefix(arg, "-") {
			fmt.Println("usage: claude-sessions [directory]")
			os.Exit(0)
		}
	}
	within, err := newScope(arg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		os.Exit(1)
	}

	sessions, err := latestSessions(sources, sessionLimit, within)
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		os.Exit(1)
	}
	if len(sessions) == 0 {
		missing := "claude-sessions: no Claude sessions found"
		if within.path != "" {
			missing += " in " + within.path
		}
		fmt.Fprintln(os.Stderr, missing)
		os.Exit(1)
	}

	program := tea.NewProgram(newModel(sessions, within), tea.WithAltScreen())
	finished, err := program.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		os.Exit(1)
	}

	final, ok := finished.(model)
	if !ok || final.chosen == nil {
		return
	}

	// Report the directory before exec'ing: after exec there is no more of this
	// process left to run, and the shell function needs the answer regardless of
	// how the session ends.
	reportCwd(final.chosen.Cwd)

	// Hand the terminal over to Claude: exec replaces this process, so the
	// session runs where the TUI was, with no shell wrapper left behind.
	command := resumeCommand(*final.chosen)
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	if err := syscall.Exec(shell, []string{shell, "-c", command}, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions: could not resume:", err)
		os.Exit(1)
	}
}
