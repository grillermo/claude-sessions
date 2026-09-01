// Reading Claude Code transcripts, the same way the Python backend does.
//
// Transcripts live in ~/.claude/projects/<encoded-cwd>/<id>.jsonl, one JSON
// record per line. A session's first message is the first thing the user
// actually typed that is not an argument-less slash command; its last message
// is the last prose from either side. Tool calls, tool results, thinking
// blocks, and system reminders are never messages.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	systemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	commandName    = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgs    = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	bareCommand    = regexp.MustCompile(`^/[A-Za-z][A-Za-z0-9_:-]*$`)
)

// Session is one transcript, reduced to what the list needs to show.
type Session struct {
	ConvID string
	Cwd    string
	MTime  time.Time
	First  string
	Last   string
}

// record is the slice of a transcript line we care about.
type record struct {
	Type    string `json:"type"`
	IsMeta  bool   `json:"isMeta"`
	Cwd     string `json:"cwd"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// contentBlock is one entry of a structured message content array.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// defaultRoot returns where Claude Code keeps its per-project transcripts.
func defaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// dirToPath converts a Claude project directory name back to a filesystem path.
func dirToPath(dirname string) string {
	s := "-" + strings.TrimLeft(dirname, "-")
	s = strings.ReplaceAll(s, "--", "/.")
	return strings.ReplaceAll(s, "-", "/")
}

// commandText renders a slash-command envelope as the command line the user
// typed, and leaves anything else unchanged.
func commandText(text string) string {
	name := commandName.FindStringSubmatch(text)
	if name == nil {
		return text
	}
	parts := []string{strings.TrimSpace(name[1])}
	if args := commandArgs.FindStringSubmatch(text); args != nil {
		parts = append(parts, strings.TrimSpace(args[1]))
	}
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " ")
}

// isBareCommand reports whether a message is only a slash command with no
// arguments, such as `/clear`, which says nothing about what a session is about.
func isBareCommand(text string) bool {
	return bareCommand.MatchString(strings.TrimSpace(text))
}

// messageTexts returns the prose blocks of one transcript record, in order.
func messageTexts(rec record) []string {
	if rec.Message == nil || len(rec.Message.Content) == 0 {
		return nil
	}

	var blocks []string
	var text string
	if err := json.Unmarshal(rec.Message.Content, &text); err == nil {
		blocks = []string{text}
	} else {
		var structured []contentBlock
		if err := json.Unmarshal(rec.Message.Content, &structured); err != nil {
			return nil
		}
		for _, block := range structured {
			if block.Type == "text" {
				blocks = append(blocks, block.Text)
			}
		}
	}

	var texts []string
	for _, block := range blocks {
		cleaned := commandText(strings.TrimSpace(systemReminder.ReplaceAllString(block, "")))
		if cleaned != "" && !strings.HasPrefix(cleaned, "<local-command") {
			texts = append(texts, cleaned)
		}
	}
	return texts
}

// readSession reads one transcript and returns its first and last messages
// along with the cwd it recorded, if any.
func readSession(transcript string) (first, last, cwd string, err error) {
	file, err := os.Open(transcript)
	if err != nil {
		return "", "", "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if cwd == "" && rec.Cwd != "" {
			cwd = rec.Cwd
		}
		if rec.IsMeta || (rec.Type != "user" && rec.Type != "assistant") {
			continue
		}
		for _, text := range messageTexts(rec) {
			last = text
			if first == "" && rec.Type == "user" && !isBareCommand(text) {
				first = text
			}
		}
	}
	// A truncated or oversized line ends the scan; whatever we read is still
	// worth showing.
	return first, last, cwd, nil
}

// resumeCommand returns the shell command that resumes a session.
func resumeCommand(s Session) string {
	return fmt.Sprintf("cd %s && claude --resume %s", shellQuote(s.Cwd), shellQuote(s.ConvID))
}

// cwdFileEnv names the file a wrapping shell function asks us to write the
// chosen session's directory into. A child process cannot change its parent
// shell's directory, so the function reads the file back and cds there itself.
const cwdFileEnv = "CLAUDE_SESSIONS_CWD_FILE"

// reportCwd tells the wrapping shell function where the session lives. It is
// best-effort: without the wrapper there is no file to write, and a failed
// write must not stop the session from resuming.
func reportCwd(cwd string) {
	path := os.Getenv(cwdFileEnv)
	if path == "" {
		return
	}
	os.WriteFile(path, []byte(cwd+"\n"), 0o600)
}

// shellQuote wraps a value so a POSIX shell reads it as one literal word.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// latestSessions returns the most recently touched sessions, newest first.
func latestSessions(root string, limit int) ([]Session, error) {
	transcripts, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}

	type entry struct {
		path  string
		id    string
		dir   string
		mtime time.Time
	}
	var entries []entry
	for _, path := range transcripts {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		name := filepath.Base(path)
		entries = append(entries, entry{
			path:  path,
			id:    strings.TrimSuffix(name, ".jsonl"),
			dir:   filepath.Base(filepath.Dir(path)),
			mtime: info.ModTime(),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].mtime.Equal(entries[j].mtime) {
			return entries[i].id > entries[j].id
		}
		return entries[i].mtime.After(entries[j].mtime)
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}

	// Only the newest transcripts are parsed: reading every session on disk
	// would cost seconds for a list that shows a few dozen rows.
	sessions := make([]Session, 0, len(entries))
	for _, e := range entries {
		first, last, cwd, err := readSession(e.path)
		if err != nil {
			continue
		}
		if cwd == "" {
			cwd = dirToPath(e.dir)
		}
		sessions = append(sessions, Session{
			ConvID: e.id,
			Cwd:    cwd,
			MTime:  e.mtime,
			First:  first,
			Last:   last,
		})
	}
	return sessions, nil
}

// timeAgo renders an age the way the web UI does: coarse and short.
func timeAgo(d time.Duration) string {
	seconds := int(d.Seconds())
	switch {
	case seconds < 60:
		return "just now"
	case seconds < 3600:
		return fmt.Sprintf("%dm ago", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh ago", seconds/3600)
	case seconds < 30*86400:
		return fmt.Sprintf("%dd ago", seconds/86400)
	default:
		return fmt.Sprintf("%dmo ago", seconds/(30*86400))
	}
}
