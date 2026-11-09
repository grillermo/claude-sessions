// Headless search: the same sessions the TUI lists, every account on this
// machine and every other machine from the shared index, searched and printed
// as plain text or JSON lines, for scripts and agents that cannot drive a TUI.
//
//	claude-sessions --search [--in DIR] [--host NAME] [--limit N] [--json] [--no-sync] WORD...
//	claude-sessions --show [--host NAME] [--json] ID
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// searchSyncTimeout bounds the mirror refresh a search starts with; past it
// the search runs on the mirror as it is.
var searchSyncTimeout = 20 * time.Second

// showSSHTimeout bounds fetching a transcript from the machine it lives on.
var showSSHTimeout = 30 * time.Second

// parseInterspersed parses flags wherever they sit among the arguments, so
// `--search foo --limit 5 bar` works, and returns the rest in order.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return rest, nil
		}
		if args[0] == "--" {
			return append(rest, args[1:]...), nil
		}
		rest = append(rest, args[0])
		args = args[1:]
	}
}

// hit is one session that matched a search.
type hit struct {
	Record
	Matches  int      `json:"matches"`
	Snippets []string `json:"snippets"`
	Resume   string   `json:"resume"` // run on Host
}

// searchOptions narrow a search.
type searchOptions struct {
	terms   []string
	within  scope
	host    string // "" for every machine
	limit   int
	exclude string // a session ID to leave out: the one running the search
}

// searchRecords returns the records holding every term, case-insensitively,
// newest first. A term matches the first or last message, the full prose, the
// directory, the account, or the machine, as the TUI's filter does.
func searchRecords(records []Record, self string, opt searchOptions) []hit {
	patterns := make([]*regexp.Regexp, len(opt.terms))
	lowered := make([]string, len(opt.terms))
	for i, t := range opt.terms {
		patterns[i] = regexp.MustCompile("(?i)" + regexp.QuoteMeta(t))
		lowered[i] = strings.ToLower(t)
	}
	var hits []hit
	for _, r := range records {
		if r.ID == opt.exclude {
			continue
		}
		if opt.host != "" && !strings.EqualFold(r.Host, opt.host) {
			continue
		}
		if r.Host == self {
			if !opt.within.coversPath(r.Cwd) {
				continue
			}
		} else if !opt.within.coversRemote(r.Cwd) {
			continue
		}
		haystack := strings.ToLower(strings.Join(
			[]string{r.First, r.Last, r.Cwd, r.Account.Email, r.Host, r.Text}, "\n"))
		matched := true
		for _, t := range lowered {
			if !strings.Contains(haystack, t) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		h := hit{Record: r}
		var shown []int // where each snippet's match starts
		for _, p := range patterns {
			locs := p.FindAllStringIndex(r.Text, -1)
			h.Matches += len(locs)
			if len(locs) == 0 || len(h.Snippets) == 3 || nearAny(locs[0][0], shown, snippetRadius) {
				continue
			}
			shown = append(shown, locs[0][0])
			h.Snippets = append(h.Snippets, snippet(r.Text, locs[0][0], locs[0][1], snippetRadius))
		}
		hits = append(hits, h)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].MTime.After(hits[j].MTime) })
	if opt.limit > 0 && len(hits) > opt.limit {
		hits = hits[:opt.limit]
	}
	return hits
}

// snippetRadius is about how much text a snippet shows either side of a match.
const snippetRadius = 120

// nearAny reports whether offset falls within radius of any of the others, so
// two words matched side by side share one snippet.
func nearAny(offset int, others []int, radius int) bool {
	for _, o := range others {
		if offset > o-radius && offset < o+radius {
			return true
		}
	}
	return false
}

var whitespace = regexp.MustCompile(`\s+`)

// snippet is the text around a match, on one line, with about `radius` runes
// either side and ellipses where it was cut.
func snippet(text string, start, end, radius int) string {
	before := []rune(text[:start])
	after := []rune(text[end:])
	lead, trail := "", ""
	if len(before) > radius {
		before, lead = before[len(before)-radius:], "…"
	}
	if len(after) > radius {
		after, trail = after[:radius], "…"
	}
	s := lead + string(before) + text[start:end] + string(after) + trail
	return strings.TrimSpace(whitespace.ReplaceAllString(s, " "))
}

// allRecords is every session this search can see: this machine's, read
// through the export cache so unchanged transcripts are not parsed again, and
// the other machines' from the mirror, refreshed first unless noSync.
func allRecords(noSync bool, warn io.Writer) ([]Record, string, error) {
	cache, err := cacheDir()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, "", err
	}
	sources, err := projectSources()
	if err != nil {
		return nil, "", err
	}
	self := localHost()
	records, err := exportRecords(sources, self, filepath.Join(cache, "local.json"))
	if err != nil {
		return nil, "", err
	}
	remote, err := newRemoteSource()
	if err != nil {
		return records, self, nil
	}
	if remote.rsyncSrc != "" && !noSync {
		if err := remote.syncWithin(searchSyncTimeout); err != nil {
			age := "never synced"
			if t := remote.syncedAt(); !t.IsZero() {
				age = "synced " + timeAgo(time.Since(t))
			}
			fmt.Fprintf(warn, "claude-sessions: could not refresh the other machines' sessions (%v); using the mirror, %s\n", err, age)
		}
	}
	paths, _ := filepath.Glob(filepath.Join(remote.dir, "*.jsonl"))
	for _, path := range paths {
		hostRecords, err := readRecordFile(path)
		if err != nil {
			continue
		}
		for _, r := range hostRecords {
			if r.Host != "" && r.Host != self {
				records = append(records, r)
			}
		}
	}
	return records, self, nil
}

// resumeSession is the Session a record describes, for resumeCommand.
func (r Record) resumeSession(self string) Session {
	s := Session{ConvID: r.ID, Cwd: r.Cwd, Account: account{number: r.Account.Number, email: r.Account.Email}}
	if r.Host != self {
		s.Host = r.Host
	}
	return s
}

// resumeLine is how a person resumes the session from a terminal on this
// machine.
func resumeLine(s Session) string {
	if s.Host == "" {
		return resumeCommand(s)
	}
	return "ssh -t " + shellQuote(s.Host) + " " + shellQuote(`exec "$SHELL" -lic `+shellQuote(resumeCommand(s)))
}

func runSearch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("--search", flag.ContinueOnError)
	fs.SetOutput(stderr)
	in := fs.String("in", "", "only sessions run in this directory or below it")
	host := fs.String("host", "", "only this machine's sessions")
	limit := fs.Int("limit", 20, "most sessions to print; 0 for all")
	asJSON := fs.Bool("json", false, "print JSON lines")
	noSync := fs.Bool("no-sync", false, "do not refresh the other machines' sessions first")
	withSelf := fs.Bool("include-current", false, "keep the session running this search")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: claude-sessions --search [--in DIR] [--host NAME] [--limit N] [--json] [--no-sync] WORD...")
		fs.PrintDefaults()
	}
	terms, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	within, err := newScope(*in)
	if err != nil {
		fmt.Fprintln(stderr, "claude-sessions:", err)
		return 1
	}
	records, self, err := allRecords(*noSync, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "claude-sessions:", err)
		return 1
	}
	opt := searchOptions{terms: terms, within: within, host: strings.ToLower(*host), limit: *limit}
	if !*withSelf {
		opt.exclude = os.Getenv("CLAUDE_CODE_SESSION_ID")
	}
	hits := searchRecords(records, self, opt)
	out := bufio.NewWriter(stdout)
	defer out.Flush()
	now := time.Now()
	for i, h := range hits {
		h.Resume = resumeCommand(h.resumeSession(self))
		if *asJSON {
			h.Text = ""
			enc := json.NewEncoder(out)
			enc.SetEscapeHTML(false)
			enc.Encode(h)
			continue
		}
		if i > 0 {
			fmt.Fprintln(out)
		}
		writeHit(out, h, now)
	}
	if !*asJSON && len(hits) == 0 {
		fmt.Fprintln(stderr, "claude-sessions: no sessions match")
	}
	return 0
}

// writeHit prints one search result as a few readable lines.
func writeHit(w io.Writer, h hit, now time.Time) {
	head := []string{h.ID, h.Host}
	if h.Account.Email != "" {
		head = append(head, h.Account.Email)
	}
	head = append(head, h.MTime.Local().Format("2006-01-02 15:04"), "("+timeAgo(now.Sub(h.MTime))+")")
	if h.Matches > 0 {
		head = append(head, fmt.Sprintf("%d matches", h.Matches))
	}
	fmt.Fprintln(w, strings.Join(head, "  "))
	fmt.Fprintln(w, "  dir:   "+h.Cwd)
	fmt.Fprintln(w, "  first: "+oneLine(h.First, 200))
	fmt.Fprintln(w, "  last:  "+oneLine(h.Last, 200))
	for _, s := range h.Snippets {
		fmt.Fprintln(w, "  match: "+s)
	}
	fmt.Fprintf(w, "  resume on %s: %s\n", h.Host, h.Resume)
}

// oneLine flattens text to one line of at most max runes.
func oneLine(text string, max int) string {
	return truncate(strings.TrimSpace(whitespace.ReplaceAllString(text, " ")), max)
}

// turn is one prose block of a transcript and who wrote it.
type turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// readTurns reads a transcript's prose with who said each part, the same
// blocks readSession joins into a session's text.
func readTurns(path string) ([]turn, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var turns []turn
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var rec record
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		if rec.IsMeta || (rec.Type != "user" && rec.Type != "assistant") {
			continue
		}
		for _, block := range messageTexts(rec) {
			turns = append(turns, turn{Role: rec.Type, Text: block})
		}
	}
	return turns, nil
}

// findTranscript finds this machine's transcript for a session ID, or for the
// one session whose ID starts with the given prefix.
func findTranscript(id string) (transcript, bool, error) {
	sources, err := projectSources()
	if err != nil {
		return transcript{}, false, err
	}
	entries, err := listTranscripts(sources, 0, scope{})
	if err != nil {
		return transcript{}, false, err
	}
	var found []transcript
	for _, e := range entries {
		if e.id == id {
			return e, true, nil
		}
		if strings.HasPrefix(e.id, id) {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		return transcript{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return transcript{}, false, fmt.Errorf("%d sessions start with %s; give more of the ID", len(found), id)
}

var errNotFound = errors.New("no session with that ID")

func runShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("--show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	host := fs.String("host", "", "the machine the session ran on, when it is not this one")
	asJSON := fs.Bool("json", false, "print one JSON line per turn")
	localOnly := fs.Bool("local", false, "look only at this machine's transcripts")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: claude-sessions --show [--host NAME] [--json] ID")
		fs.PrintDefaults()
	}
	ids, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(ids) != 1 || ids[0] == "" {
		fs.Usage()
		return 2
	}
	id := ids[0]
	self := localHost()
	want := strings.ToLower(*host)

	if want == "" || want == self {
		e, ok, err := findTranscript(id)
		if err != nil {
			fmt.Fprintln(stderr, "claude-sessions:", err)
			return 1
		}
		if ok {
			turns, err := readTurns(e.path)
			if err != nil {
				fmt.Fprintln(stderr, "claude-sessions:", err)
				return 1
			}
			writeTurns(stdout, turns, *asJSON)
			return 0
		}
		if *localOnly || want == self {
			fmt.Fprintln(stderr, "claude-sessions:", errNotFound)
			return 1
		}
	}

	// Another machine's session: ask that machine for its transcript, and fall
	// back to the prose the index holds when it cannot be reached.
	rec, ok := findRecord(id, want, self)
	if !ok {
		fmt.Fprintln(stderr, "claude-sessions:", errNotFound)
		return 1
	}
	if err := showOver(rec.Host, rec.ID, *asJSON, stdout); err == nil {
		return 0
	} else {
		fmt.Fprintf(stderr, "claude-sessions: could not read it on %s (%v); showing the index's copy, without who said what\n", rec.Host, err)
	}
	if *asJSON {
		writeTurns(stdout, []turn{{Role: "unknown", Text: rec.Text}}, true)
	} else {
		fmt.Fprintln(stdout, rec.Text)
	}
	return 0
}

// findRecord looks a session up in the mirror of the other machines' index.
func findRecord(id, host, self string) (Record, bool) {
	remote, err := newRemoteSource()
	if err != nil {
		return Record{}, false
	}
	paths, _ := filepath.Glob(filepath.Join(remote.dir, "*.jsonl"))
	var found []Record
	for _, path := range paths {
		records, _ := readRecordFile(path)
		for _, r := range records {
			if r.Host == self || (host != "" && !strings.EqualFold(r.Host, host)) {
				continue
			}
			if r.ID == id {
				return r, true
			}
			if strings.HasPrefix(r.ID, id) {
				found = append(found, r)
			}
		}
	}
	if len(found) == 1 {
		return found[0], true
	}
	return Record{}, false
}

// showOver runs --show on the machine a session lives on.
func showOver(host, id string, asJSON bool, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), showSSHTimeout)
	defer cancel()
	remote := remotePath + " ~/c/claude-sessions/claude-sessions --show --local "
	if asJSON {
		remote += "--json "
	}
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "--", host, remote+shellQuote(id))
	var buf strings.Builder
	var errBuf strings.Builder
	cmd.Stdout, cmd.Stderr = &buf, &errBuf
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errBuf.String()); msg != "" {
			return fmt.Errorf("%v: %s", err, msg)
		}
		return err
	}
	// A machine still running a build from before --show prints the TUI's
	// usage line instead, and exits 0.
	if strings.HasPrefix(buf.String(), "usage: claude-sessions [directory]") {
		return errors.New("its claude-sessions has no --show yet")
	}
	_, err := io.WriteString(stdout, buf.String())
	return err
}

// writeTurns prints a transcript's prose, each block under who wrote it.
func writeTurns(w io.Writer, turns []turn, asJSON bool) {
	out := bufio.NewWriter(w)
	defer out.Flush()
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	for i, t := range turns {
		if asJSON {
			enc.Encode(t)
			continue
		}
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "## %s\n\n%s\n", t.Role, t.Text)
	}
}
