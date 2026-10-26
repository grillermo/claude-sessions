# Cross-machine search Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development (recommended) or executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** claude-sessions lists, searches (full prose) and resumes sessions from immunemac, macmini
and PC-linux in one timeline, without slowing startup.

**Architecture:** Each host exports a per-session index (full prose) and pushes only what changed to
the macmini, which holds one JSON-lines file and one manifest per host and enforces a 5 GB rolling
cap. The TUI reads a local mirror of those files at launch and refreshes it with `rsync -az` in the
background. A row from another machine resumes over `ssh -t`. A Claude Code hook and a 10-minute
timer, shipped through `~/c/agents-configs`, trigger the pushes.

**Tech Stack:** Go 1.24, Bubble Tea v1.3.10, `syscall.Flock`, `ssh`, `rsync` (openrsync on macOS),
POSIX sh, bash, jq, launchd, systemd user units.

**Spec:** `docs/superpowers/specs/2026-10-26-cross-machine-search-design.md` (approved).

---

## Facts verified while planning (2026-10-26)

- On immunemac and the macmini, the cswap account `settings.json` files are symlinks to
  `~/.claude/settings.json`. The PC has no cswap accounts. One hook entry per machine covers
  everything.
- The short hostnames are `immunemac`, `macmini` and `PC` (lowercased to `pc`), which match the SSH
  aliases.
- immunemac and the macmini run openrsync (protocol 29), and the PC runs rsync 3.5.0. This command
  worked from immunemac against the macmini:
  `rsync -az --delete --include='*.jsonl' --exclude='*' -e 'ssh -o BatchMode=yes -o ConnectTimeout=5' macmini:<dir>/ <local>/`.
- `tea.Batch` drops nil commands and returns the only command when one is left, so the existing
  `m.Init()()` test still gets a `batchMsg` while there's no remote source.
- The macmini and PC have `/usr/bin/rsync`. The macmini has `/usr/bin/jq` and `/bin/launchctl`; the
  PC has `/usr/bin/jq` and `/usr/bin/systemctl`.
- Hostname, cswap and rsync checks were done for immunemac, the macmini and the PC. macaliada,
  mactastic and PC-windows are out of scope.

## File structure

**claude-sessions (`~/c/claude-sessions`)**

| File | Responsibility |
| --- | --- |
| `sessions.go` (modify) | Full-text extraction, `Session.Text/Host/haystack`, transcript size, `resumeArgv` |
| `index.go` (create) | `Record` type and its JSON-lines file I/O, host names, cache dir, atomic writes |
| `export.go` (create) | Parse cache, `manifest` and its hash, the delta format |
| `lock.go` (create) | `lockFile`, a non-blocking or blocking `flock` |
| `ingest.go` (create) | The index host's `store`: applying deltas, the 5 GB cap |
| `push.go` (create) | Debounced push loop, the transport (in-process or ssh), the push log |
| `remote.go` (create) | The remote source (mirror or the index dir), rsync fetch, `remoteMsg`, remote scope |
| `cli.go` (create) | `--export`, `--ingest`, `--push` entry points |
| `main.go` (modify) | Local/remote merge, remote status in the title, host label, CLI dispatch, exec of the resume |
| `README.md` (modify) | "Other machines" section |
| Tests | `sessions_test.go`, `index_test.go`, `export_test.go`, `ingest_test.go`, `push_test.go`, `remote_test.go` |

**agents-configs (`~/c/agents-configs`)**

| File | Responsibility |
| --- | --- |
| `hooks/claude-sessions-push.sh` (create) | The detaching wrapper; `--wait` runs in the foreground for timers |
| `install.sh` (modify) | `register_session_push`, `install_session_push_timer` |
| `tests/claude-sessions-push.test.sh` (create) | Wrapper behavior |
| `tests/install.test.sh` (modify) | Stubs for launchctl and systemctl, registration and timer cases |

Commit trailer for every commit in this plan:

```
Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
```

---

### Task 1: Full prose in every session, searched by the filter

**Files:**
- Modify: `sessions.go` (Session struct, `readSession`, `readTranscript`)
- Modify: `main.go` (`newModel`, `applyFilter`)
- Test: `sessions_test.go`

- [ ] **Step 1: Write the failing test** (append to `sessions_test.go`)

```go
func TestFilterMatchesTheMiddleOfASession(t *testing.T) {
	root := writeTranscript(t, "-tmp-x", "id1",
		`{"type":"user","message":{"content":"start here"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"the flux capacitor is fine"}]}}`,
		`{"type":"user","message":{"content":"thanks, bye"}}`,
	)

	sessions, err := latestSessions(local(root), 20, scope{})
	if err != nil {
		t.Fatal(err)
	}
	want := "start here\n\nthe flux capacitor is fine\n\nthanks, bye"
	if sessions[0].Text != want {
		t.Errorf("text = %q, want %q", sessions[0].Text, want)
	}

	m := newModel(sessions, scope{})
	m.query = "FLUX CAPACITOR"
	m.applyFilter()
	if len(m.filtered) != 1 {
		t.Errorf("a phrase from the middle matched %d sessions, want 1", len(m.filtered))
	}
}
```

- [ ] **Step 2: Run it and check that it fails**

Run: `go test -run TestFilterMatchesTheMiddleOfASession ./...`
Expected: it fails to compile with `sessions[0].Text undefined`.

- [ ] **Step 3: Implement**

In `sessions.go`, replace the `Session` struct with:

```go
// Session is one transcript, reduced to what the list needs to show.
type Session struct {
	ConvID  string
	Cwd     string
	MTime   time.Time
	First   string
	Last    string
	Account account
	// Text is every prose block of the session, yours and Claude's, in order.
	Text string
	// Host is the machine a session from the shared index ran on; empty for
	// this machine's own sessions.
	Host string
	// haystack is what the filter searches, lowercased once by index.
	haystack string
}

// index builds the lowercased search text, so filtering does not lowercase
// every session's full text on every keystroke.
func (s *Session) index() {
	s.haystack = strings.ToLower(strings.Join(
		[]string{s.First, s.Last, s.Cwd, s.Account.label(), s.Host, s.Text}, "\n"))
}
```

Replace `readSession` with this version. The changes: it also returns `text`, and the loop
variable is renamed so it doesn't shadow `text`:

```go
// readSession reads one transcript and returns its first and last messages,
// all of its prose joined by blank lines, and the cwd it recorded, if any.
func readSession(transcript string) (first, last, cwd, text string, err error) {
	file, err := os.Open(transcript)
	if err != nil {
		return "", "", "", "", err
	}
	defer file.Close()

	var texts []string
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
		for _, block := range messageTexts(rec) {
			texts = append(texts, block)
			last = block
			if first == "" && rec.Type == "user" && !isBareCommand(block) {
				first = block
			}
		}
	}
	// A truncated or oversized line ends the scan; whatever we read is still
	// worth showing.
	return first, last, cwd, strings.Join(texts, "\n\n"), nil
}
```

In `readTranscript`, change the first line to
`first, last, cwd, text, err := readSession(e.path)`, and replace its `return Session{...}, true`
with:

```go
	s := Session{
		ConvID:  e.id,
		Cwd:     cwd,
		MTime:   e.mtime,
		First:   first,
		Last:    last,
		Account: e.account,
		Text:    text,
	}
	s.index()
	return s, true
```

In `main.go`, replace `newModel` with:

```go
func newModel(sessions []Session, within scope) model {
	for i := range sessions {
		sessions[i].index()
	}
	m := model{sessions: sessions, scope: within, now: time.Now(), width: 80, height: 24}
	m.applyFilter()
	return m
}
```

In `applyFilter`, replace these two lines:

```go
			haystack := strings.ToLower(s.First + "\n" + s.Last + "\n" + s.Cwd + "\n" + s.Account.label())
			if strings.Contains(haystack, query) {
```

with:

```go
			if strings.Contains(s.haystack, query) {
```

In the `batchMsg` case of `Update`, nothing changes in this task; `readTranscript` already indexes
the sessions.

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS. That includes `TestFilterMatchesEitherMessageOrPath` and
`TestRowAndFilterShowTheAccount`, whose sessions get indexed by `newModel`.

- [ ] **Step 5: Commit**

```bash
git add sessions.go main.go sessions_test.go
git commit -m "feat: search the full prose of every session

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Index records and host names

**Files:**
- Create: `index.go`
- Modify: `sessions.go` (`transcript.size`)
- Test: `index_test.go`

- [ ] **Step 1: Write the failing tests** (`index_test.go`)

```go
package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecordsRoundTripAndSkipBadLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pc.jsonl")
	want := []Record{
		{Host: "pc", ID: "a", Cwd: "/home/me/c/x", MTime: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
			Bytes: 10, First: "hi <b>", Last: "bye", Text: "hi <b>\n\nbye"},
		{Host: "pc", ID: "b", Account: recordAccount{Number: 3, Email: "me@x.com"}},
	}
	if err := writeRecordFile(path, want); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not json\n{\"host\":\"pc\"}\n")
	f.Close()

	got, err := readRecordFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %+v\nwant      %+v", got, want)
	}
}

func TestAMissingRecordFileIsEmpty(t *testing.T) {
	got, err := readRecordFile(filepath.Join(t.TempDir(), "nobody.jsonl"))
	if err != nil || got != nil {
		t.Errorf("missing file = %v, %v", got, err)
	}
}

func TestARecordBecomesASearchableSession(t *testing.T) {
	r := Record{Host: "pc", ID: "a", Cwd: "/home/me/c/x", Text: "Deep In The Middle",
		Account: recordAccount{Number: 2, Email: "me@x.com"}}
	s := r.session()
	if s.Host != "pc" || s.ConvID != "a" || s.Account.number != 2 || s.Account.email != "me@x.com" {
		t.Errorf("session = %+v", s)
	}
	if !strings.Contains(s.haystack, "deep in the middle") || !strings.Contains(s.haystack, "pc") {
		t.Errorf("haystack = %q", s.haystack)
	}
}

func TestLocalHostIsTheShortLowercaseName(t *testing.T) {
	t.Setenv("CLAUDE_SESSIONS_HOST", "PC")
	if got := localHost(); got != "pc" {
		t.Errorf("override = %q", got)
	}
	t.Setenv("CLAUDE_SESSIONS_HOST", "")
	if got := localHost(); got == "" || strings.Contains(got, ".") || got != strings.ToLower(got) {
		t.Errorf("hostname = %q", got)
	}
}

func TestIndexHostDefaultsToTheMacmini(t *testing.T) {
	t.Setenv("CLAUDE_SESSIONS_INDEX_HOST", "")
	if got := indexHost(); got != "macmini" {
		t.Errorf("index host = %q", got)
	}
}
```

- [ ] **Step 2: Run them and check that they fail**

Run: `go test -run 'Record|LocalHost|IndexHost' ./...`
Expected: they fail to compile with `undefined: Record`.

- [ ] **Step 3: Implement** (`index.go`)

```go
// The shared index: each machine's sessions in a form the other machines can
// list and search without the transcripts.
//
// A host's index is JSON lines, one Record per session, kept on the index host
// (the macmini) in ~/.cache/claude-sessions/hosts/<host>.jsonl and mirrored to
// every other machine.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is one session in a host's index file.
type Record struct {
	Host    string        `json:"host"`
	ID      string        `json:"id"`
	Cwd     string        `json:"cwd"`
	MTime   time.Time     `json:"mtime"`
	Bytes   int64         `json:"bytes"`
	Account recordAccount `json:"account"`
	First   string        `json:"first"`
	Last    string        `json:"last"`
	Text    string        `json:"text"`
}

// recordAccount is the cswap account a session belongs to on its host.
type recordAccount struct {
	Number int    `json:"number"`
	Email  string `json:"email"`
}

// newRecord describes a session read on this machine. Times are kept in UTC
// so the same session always encodes the same way.
func newRecord(host string, s Session, bytes int64) Record {
	return Record{
		Host:    host,
		ID:      s.ConvID,
		Cwd:     s.Cwd,
		MTime:   s.MTime.UTC(),
		Bytes:   bytes,
		Account: recordAccount{Number: s.Account.number, Email: s.Account.email},
		First:   s.First,
		Last:    s.Last,
		Text:    s.Text,
	}
}

// session turns a record from another machine into a row of the list.
func (r Record) session() Session {
	s := Session{
		ConvID:  r.ID,
		Cwd:     r.Cwd,
		MTime:   r.MTime,
		First:   r.First,
		Last:    r.Last,
		Account: account{number: r.Account.Number, email: r.Account.Email},
		Text:    r.Text,
		Host:    r.Host,
	}
	s.index()
	return s
}

// localHost names this machine the way the others reach it over ssh: the
// short hostname, lowercased (immunemac, macmini, pc).
func localHost() string {
	if host := os.Getenv("CLAUDE_SESSIONS_HOST"); host != "" {
		return strings.ToLower(host)
	}
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.SplitN(name, ".", 2)[0])
}

// indexHost is the machine that keeps every host's index: the always-on
// macmini, the one machine reachable from away over WARP.
func indexHost() string {
	if host := os.Getenv("CLAUDE_SESSIONS_INDEX_HOST"); host != "" {
		return host
	}
	return "macmini"
}

// cacheDir is where this machine keeps the index, its mirror and push state.
func cacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "claude-sessions"), nil
}

// readLine returns the next line without its newline, however long it is,
// and an error only once there is nothing left.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if len(line) > 0 {
		return bytes.TrimRight(line, "\r\n"), nil
	}
	return nil, err
}

// readRecords parses index lines, skipping any that are not a record.
func readRecords(r io.Reader) []Record {
	br := bufio.NewReader(r)
	var records []Record
	for {
		line, err := readLine(br)
		if err != nil {
			return records
		}
		var rec Record
		if json.Unmarshal(line, &rec) != nil || rec.ID == "" {
			continue
		}
		records = append(records, rec)
	}
}

// readRecordFile reads one host's index file. A host with no file has no
// sessions, which is not an error.
func readRecordFile(path string) ([]Record, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readRecords(file), nil
}

// writeRecordFile replaces a host's index file in one step.
func writeRecordFile(path string, records []Record) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return writeAtomic(path, buf.Bytes())
}

// writeAtomic writes a file under a temporary name and renames it into place,
// so a reader, rsync included, never sees half of it. The temporary name does
// not end in .jsonl, so rsync's *.jsonl filter skips it.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
```

In `sessions.go`, add `size int64` to `transcript` (below `mtime time.Time`) and set
`size: info.Size(),` in the literal inside `listTranscripts`.

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add index.go index_test.go sessions.go
git commit -m "feat: index records for sharing sessions between machines

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Exporter, manifest and delta

**Files:**
- Create: `export.go`
- Test: `export_test.go`

- [ ] **Step 1: Write the failing tests** (`export_test.go`)

```go
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExportReparsesOnlyChangedTranscripts(t *testing.T) {
	root := writeTranscript(t, "-Users-me-proj", "a",
		`{"type":"user","cwd":"/Users/me/proj","message":{"content":"first version"}}`)
	cache := filepath.Join(t.TempDir(), "local.json")

	records, err := exportRecords(local(root), "immunemac", cache)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].First != "first version" || records[0].Host != "immunemac" || records[0].Bytes == 0 {
		t.Fatalf("records = %+v", records)
	}

	// Doctor the cache: an unchanged transcript must come from it, not the file.
	entries := map[string]cacheEntry{}
	data, _ := os.ReadFile(cache)
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatal(err)
	}
	for path, e := range entries {
		e.Record.First = "from cache"
		entries[path] = e
	}
	data, _ = json.Marshal(entries)
	os.WriteFile(cache, data, 0o644)
	if records, _ = exportRecords(local(root), "immunemac", cache); records[0].First != "from cache" {
		t.Errorf("unchanged transcript was parsed again: %q", records[0].First)
	}

	// A new mtime makes it read the file again.
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(root, "-Users-me-proj", "a.jsonl"), later, later)
	if records, _ = exportRecords(local(root), "immunemac", cache); records[0].First != "first version" {
		t.Errorf("changed transcript came from the cache: %q", records[0].First)
	}
}

// decodeDelta splits a delta into its header and the IDs it sends and deletes.
func decodeDelta(t *testing.T, data []byte) (deltaHeader, []string, []string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var header deltaHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	var sent, deleted []string
	for _, line := range lines[1:] {
		var d deltaLine
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatal(err)
		}
		if d.Delete != "" {
			deleted = append(deleted, d.Delete)
		} else {
			sent = append(sent, d.ID)
		}
	}
	return header, sent, deleted
}

func TestDeltaSendsOnlyNewChangedAndDeleted(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	records := []Record{
		{ID: "same", MTime: t0, Bytes: 1},
		{ID: "changed", MTime: t0.Add(time.Hour), Bytes: 2},
		{ID: "new", MTime: t0, Bytes: 3},
		{ID: "was-dropped", MTime: t0, Bytes: 4},
	}
	pushed := manifest{
		"same":        {MTime: t0, Bytes: 1},
		"changed":     {MTime: t0, Bytes: 2},
		"gone":        {MTime: t0, Bytes: 5},
		"was-dropped": {MTime: t0, Bytes: 4, Dropped: true},
	}

	var buf bytes.Buffer
	if err := writeDelta(&buf, records, pushed, false); err != nil {
		t.Fatal(err)
	}
	header, sent, deleted := decodeDelta(t, buf.Bytes())
	if header.Base != pushed.hash() || header.Full {
		t.Errorf("header = %+v", header)
	}
	if !reflect.DeepEqual(sent, []string{"changed", "new"}) {
		t.Errorf("sent = %v", sent)
	}
	if !reflect.DeepEqual(deleted, []string{"gone"}) {
		t.Errorf("deleted = %v", deleted)
	}
}

func TestAFullDeltaSendsEverythingAndDeletesNothing(t *testing.T) {
	records := []Record{{ID: "a"}, {ID: "b"}}
	var buf bytes.Buffer
	if err := writeDelta(&buf, records, manifest{"gone": {}}, true); err != nil {
		t.Fatal(err)
	}
	header, sent, deleted := decodeDelta(t, buf.Bytes())
	if !header.Full || !reflect.DeepEqual(sent, []string{"a", "b"}) || deleted != nil {
		t.Errorf("header %+v, sent %v, deleted %v", header, sent, deleted)
	}
}

func TestManifestsRoundTripWithTheSameHash(t *testing.T) {
	m := manifest{"a": {MTime: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Bytes: 7, Dropped: true}}
	path := filepath.Join(t.TempDir(), "pc.manifest.json")
	if err := writeManifest(path, m); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readManifest(path)
	if err != nil || !ok || got.hash() != m.hash() {
		t.Errorf("read back %v, %v, %v", got, ok, err)
	}
	if manifest(nil).hash() != (manifest{}).hash() {
		t.Error("a nil manifest must hash like an empty one")
	}
	if _, ok, err := readManifest(filepath.Join(t.TempDir(), "none.json")); ok || err != nil {
		t.Errorf("missing manifest = %v, %v", ok, err)
	}
}
```

- [ ] **Step 2: Run them and check that they fail**

Run: `go test -run 'Export|Delta|Manifests' ./...`
Expected: they fail to compile with `undefined: exportRecords`.

- [ ] **Step 3: Implement** (`export.go`)

```go
// Exporting this machine's sessions, and the delta format that carries only
// what changed to the index host.
//
// A delta is JSON lines: a header with the hash of the manifest it was built
// against, then a Record for every new or changed session, then a
// {"delete":"<id>"} line for every session whose transcript is gone.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
	"time"
)

// cacheEntry is a parsed transcript, kept until the file changes.
type cacheEntry struct {
	MTime  time.Time `json:"mtime"`
	Size   int64     `json:"size"`
	Record Record    `json:"record"`
}

// exportRecords returns a record for every session on this machine. Only
// transcripts whose mtime or size changed since the cache at cachePath was
// written are parsed again, which keeps a push about as cheap as a listing.
func exportRecords(sources []source, host, cachePath string) ([]Record, error) {
	entries, err := listTranscripts(sources, 0, scope{})
	if err != nil {
		return nil, err
	}
	old := map[string]cacheEntry{}
	if data, err := os.ReadFile(cachePath); err == nil {
		json.Unmarshal(data, &old)
	}

	fresh := make(map[string]cacheEntry, len(entries))
	records := make([]Record, 0, len(entries))
	changed := len(old) != len(entries)
	for _, e := range entries {
		if c, ok := old[e.path]; ok && c.MTime.Equal(e.mtime) && c.Size == e.size {
			c.Record.Host = host
			fresh[e.path] = c
			records = append(records, c.Record)
			continue
		}
		changed = true
		s, ok := readTranscript(e, scope{})
		if !ok {
			continue
		}
		r := newRecord(host, s, e.size)
		fresh[e.path] = cacheEntry{MTime: e.mtime, Size: e.size, Record: r}
		records = append(records, r)
	}
	if changed {
		data, err := json.Marshal(fresh)
		if err != nil {
			return nil, err
		}
		if err := writeAtomic(cachePath, data); err != nil {
			return nil, err
		}
	}
	return records, nil
}

// manifestEntry is what the index host knows of one session from a host.
type manifestEntry struct {
	MTime   time.Time `json:"mtime"`
	Bytes   int64     `json:"bytes"`
	Dropped bool      `json:"dropped,omitempty"`
}

// manifest is every session the index host has seen from one host, by ID,
// including the ones the cap dropped, so they are not sent again.
type manifest map[string]manifestEntry

// hash identifies a manifest's contents. json.Marshal sorts map keys, so the
// same entries always hash the same, on either machine.
func (m manifest) hash() string {
	if m == nil {
		m = manifest{}
	}
	data, _ := json.Marshal(m)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readManifest reads a manifest, reporting false when there is none.
func readManifest(path string) (manifest, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	m := manifest{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false, err
	}
	if m == nil {
		m = manifest{}
	}
	return m, true, nil
}

func writeManifest(path string, m manifest) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}

// deltaHeader is a delta's first line.
type deltaHeader struct {
	Base string `json:"base"`
	Full bool   `json:"full,omitempty"`
}

// deltaLine is any line after the header: a record, or a deletion.
type deltaLine struct {
	Record
	Delete string `json:"delete,omitempty"`
}

// writeDelta writes what the index host needs to bring its copy of this host's
// sessions up to date with records: everything when full, otherwise the
// sessions that are new or changed since the pushed manifest and the ones that
// disappeared. A session the cap dropped is only sent again once it changes.
func writeDelta(w io.Writer, records []Record, pushed manifest, full bool) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(deltaHeader{Base: pushed.hash(), Full: full}); err != nil {
		return err
	}
	seen := make(map[string]bool, len(records))
	for _, r := range records {
		seen[r.ID] = true
		if !full {
			if e, ok := pushed[r.ID]; ok && e.MTime.Equal(r.MTime) && e.Bytes == r.Bytes {
				continue
			}
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	if full {
		return nil
	}
	var gone []string
	for id := range pushed {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		if err := enc.Encode(map[string]string{"delete": id}); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add export.go export_test.go
git commit -m "feat: export sessions incrementally as deltas against a manifest

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Ingest and the 5 GB cap

**Files:**
- Create: `lock.go`, `ingest.go`
- Test: `ingest_test.go`

- [ ] **Step 1: Write the failing tests** (`ingest_test.go`)

```go
package main

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"
)

var day0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func rec(id string, hour int, size int64) Record {
	return Record{ID: id, MTime: day0.Add(time.Duration(hour) * time.Hour), Bytes: size, Text: "text of " + id}
}

func delta(t *testing.T, records []Record, pushed manifest, full bool) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	if err := writeDelta(&b, records, pushed, full); err != nil {
		t.Fatal(err)
	}
	return &b
}

func storedIDs(t *testing.T, st store, host string) []string {
	t.Helper()
	records, err := readRecordFile(st.recordsPath(host))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range records {
		if r.Host != host {
			t.Errorf("record %s stored with host %q", r.ID, r.Host)
		}
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestAFullPushStoresTheHostAndReturnsItsManifest(t *testing.T) {
	st := store{t.TempDir()}
	got, err := st.ingest("pc", delta(t, []Record{rec("a", 1, 10), rec("b", 2, 10)}, nil, true), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if ids := storedIDs(t, st, "pc"); !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Errorf("stored %v", ids)
	}
	stored, ok, err := readManifest(st.manifestPath("pc"))
	if err != nil || !ok || stored.hash() != got.hash() || len(got) != 2 {
		t.Errorf("stored manifest %v, returned %v", stored, got)
	}
}

func TestADeltaAgainstAnUnknownManifestAsksForAFullPush(t *testing.T) {
	st := store{t.TempDir()}
	_, err := st.ingest("pc", delta(t, []Record{rec("a", 1, 10)}, manifest{}, false), 1000)
	if !errors.Is(err, errResync) {
		t.Fatalf("no manifest: err = %v, want errResync", err)
	}
	if _, err := os.Stat(st.recordsPath("pc")); !os.IsNotExist(err) {
		t.Error("a refused delta wrote records")
	}

	if _, err := st.ingest("pc", delta(t, []Record{rec("a", 1, 10)}, nil, true), 1000); err != nil {
		t.Fatal(err)
	}
	_, err = st.ingest("pc", delta(t, []Record{rec("z", 1, 10)}, manifest{"x": {}}, false), 1000)
	if !errors.Is(err, errResync) {
		t.Fatalf("wrong base: err = %v, want errResync", err)
	}
	if ids := storedIDs(t, st, "pc"); !reflect.DeepEqual(ids, []string{"a"}) {
		t.Errorf("a refused delta changed the records: %v", ids)
	}
}

func TestADeltaMergesNewChangedAndDeleted(t *testing.T) {
	st := store{t.TempDir()}
	pushed, err := st.ingest("pc", delta(t, []Record{rec("a", 1, 10), rec("b", 2, 10)}, nil, true), 1000)
	if err != nil {
		t.Fatal(err)
	}
	now := []Record{rec("a", 5, 12), rec("c", 6, 10)} // a changed, c new, b gone
	if _, err := st.ingest("pc", delta(t, now, pushed, false), 1000); err != nil {
		t.Fatal(err)
	}
	if ids := storedIDs(t, st, "pc"); !reflect.DeepEqual(ids, []string{"a", "c"}) {
		t.Errorf("stored %v", ids)
	}
	records, _ := readRecordFile(st.recordsPath("pc"))
	for _, r := range records {
		if r.ID == "a" && !r.MTime.Equal(day0.Add(5*time.Hour)) {
			t.Errorf("a was not updated: %v", r.MTime)
		}
	}
}

func TestTheCapDropsJustEnoughOfTheOldest(t *testing.T) {
	st := store{t.TempDir()}
	if _, err := st.ingest("pc", delta(t, []Record{rec("a", 1, 40), rec("b", 2, 40)}, nil, true), 100); err != nil {
		t.Fatal(err)
	}
	mine, err := st.ingest("immunemac", delta(t, []Record{rec("c", 3, 40)}, nil, true), 100)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: c 40, b 80, then a would make 120.
	if ids := storedIDs(t, st, "pc"); !reflect.DeepEqual(ids, []string{"b"}) {
		t.Errorf("pc kept %v", ids)
	}
	pcManifest, _, _ := readManifest(st.manifestPath("pc"))
	if !pcManifest["a"].Dropped || pcManifest["b"].Dropped {
		t.Errorf("pc manifest = %v", pcManifest)
	}
	if mine["c"].Dropped {
		t.Error("the newest session was dropped")
	}
}

func TestTiesBreakByHostThenID(t *testing.T) {
	st := store{t.TempDir()}
	if _, err := st.ingest("pc", delta(t, []Record{rec("b", 1, 60), rec("a", 1, 60)}, nil, true), 100); err != nil {
		t.Fatal(err)
	}
	if ids := storedIDs(t, st, "pc"); !reflect.DeepEqual(ids, []string{"a"}) {
		t.Errorf("kept %v, want a", ids)
	}
}

func TestADroppedSessionStaysDroppedUntilItChanges(t *testing.T) {
	st := store{t.TempDir()}
	pushed, err := st.ingest("pc", delta(t, []Record{rec("a", 1, 40), rec("b", 2, 40)}, nil, true), 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ingest("immunemac", delta(t, []Record{rec("c", 3, 40)}, nil, true), 100); err != nil {
		t.Fatal(err)
	}
	pushed, _, _ = readManifest(st.manifestPath("pc")) // now marks a as dropped

	// Unchanged, the dropped session is not sent again.
	var buf bytes.Buffer
	writeDelta(&buf, []Record{rec("a", 1, 40), rec("b", 2, 40)}, pushed, false)
	_, sent, deleted := decodeDelta(t, buf.Bytes())
	if sent != nil || deleted != nil {
		t.Errorf("unchanged push sent %v, deleted %v", sent, deleted)
	}

	// New activity makes it the newest session, so it fits and b goes instead.
	if _, err := st.ingest("pc", delta(t, []Record{rec("a", 10, 40), rec("b", 2, 40)}, pushed, false), 100); err != nil {
		t.Fatal(err)
	}
	if ids := storedIDs(t, st, "pc"); !reflect.DeepEqual(ids, []string{"a"}) {
		t.Errorf("pc kept %v, want a", ids)
	}
}

func TestIngestRejectsAHostNameThatIsNotAFileName(t *testing.T) {
	st := store{t.TempDir()}
	if _, err := st.ingest("../etc", delta(t, nil, nil, true), 100); err == nil {
		t.Error("a path in the host name was accepted")
	}
}
```

- [ ] **Step 2: Run them and check that they fail**

Run: `go test -run 'Push|Delta|Cap|Ties|Dropped|Ingest' ./...`
Expected: they fail to compile with `undefined: store`.

- [ ] **Step 3: Implement**

`lock.go`:

```go
package main

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on path, creating it if needed. With wait
// false it reports ok=false instead of blocking when another process holds it.
func lockFile(path string, wait bool) (unlock func(), ok bool, err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, true, nil
}
```

`ingest.go`:

```go
// The index host's side: applying a host's delta and keeping the whole index
// inside the cap.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// resyncExit is the exit status of --ingest when a delta was built against a
// manifest the index host does not have; the pusher answers with a full push.
const resyncExit = 3

var errResync = errors.New("the index needs a full push from this host")

// defaultCapBytes is how much history the shared index covers: the newest
// 5 GB, counted in raw transcript bytes across every host.
const defaultCapBytes int64 = 5 << 30

// hostName is what a host may be called; it becomes a file name.
var hostName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// capBytes is the cap, which CLAUDE_SESSIONS_CAP_MB overrides for tests.
func capBytes() int64 {
	if mb, err := strconv.ParseInt(os.Getenv("CLAUDE_SESSIONS_CAP_MB"), 10, 64); err == nil && mb > 0 {
		return mb << 20
	}
	return defaultCapBytes
}

// store is the index host's directory: <host>.jsonl and <host>.manifest.json
// for every host, and the lock that serializes ingests.
type store struct{ dir string }

func (st store) recordsPath(host string) string {
	return filepath.Join(st.dir, host+".jsonl")
}

func (st store) manifestPath(host string) string {
	return filepath.Join(st.dir, host+".manifest.json")
}

// manifests loads every host's manifest in the store.
func (st store) manifests() (map[string]manifest, error) {
	paths, err := filepath.Glob(filepath.Join(st.dir, "*.manifest.json"))
	if err != nil {
		return nil, err
	}
	all := map[string]manifest{}
	for _, path := range paths {
		m, ok, err := readManifest(path)
		if err != nil {
			return nil, err
		}
		if ok {
			all[strings.TrimSuffix(filepath.Base(path), ".manifest.json")] = m
		}
	}
	return all, nil
}

// ingest applies one host's delta, enforces the cap across every host, and
// returns the host's new manifest. A delta built against a manifest other than
// the stored one changes nothing and returns errResync.
func (st store) ingest(host string, in io.Reader, limit int64) (manifest, error) {
	if !hostName.MatchString(host) {
		return nil, fmt.Errorf("bad host name %q", host)
	}
	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		return nil, err
	}
	unlock, _, err := lockFile(filepath.Join(st.dir, ".lock"), true)
	if err != nil {
		return nil, err
	}
	defer unlock()

	br := bufio.NewReader(in)
	first, err := readLine(br)
	if err != nil {
		return nil, errors.New("empty delta")
	}
	var header deltaHeader
	if err := json.Unmarshal(first, &header); err != nil {
		return nil, fmt.Errorf("bad delta header: %w", err)
	}

	all, err := st.manifests()
	if err != nil {
		return nil, err
	}
	have, known := all[host]
	if !header.Full && (!known || have.hash() != header.Base) {
		return nil, errResync
	}

	records := map[string]Record{}
	if header.Full {
		have = manifest{}
	} else {
		existing, err := readRecordFile(st.recordsPath(host))
		if err != nil {
			return nil, err
		}
		for _, r := range existing {
			records[r.ID] = r
		}
	}
	for {
		line, err := readLine(br)
		if err != nil {
			break
		}
		var d deltaLine
		if json.Unmarshal(line, &d) != nil {
			continue
		}
		switch {
		case d.Delete != "":
			delete(records, d.Delete)
			delete(have, d.Delete)
		case d.ID != "":
			d.Record.Host = host
			records[d.ID] = d.Record
			have[d.ID] = manifestEntry{MTime: d.MTime, Bytes: d.Bytes}
		}
	}
	all[host] = have

	changed := map[string]map[string]Record{host: records}
	for h, ids := range overCap(all, limit) {
		recs, ok := changed[h]
		if !ok {
			existing, err := readRecordFile(st.recordsPath(h))
			if err != nil {
				return nil, err
			}
			recs = make(map[string]Record, len(existing))
			for _, r := range existing {
				recs[r.ID] = r
			}
			changed[h] = recs
		}
		for _, id := range ids {
			delete(recs, id)
			e := all[h][id]
			e.Dropped = true
			all[h][id] = e
		}
	}

	// Records first, manifests last. If this stops in between, the host never
	// got its new manifest, so it sends the same changes again, and applying a
	// record twice is harmless.
	for h, recs := range changed {
		if err := writeRecordFile(st.recordsPath(h), sortedRecords(recs)); err != nil {
			return nil, err
		}
	}
	for h := range changed {
		if err := writeManifest(st.manifestPath(h), all[h]); err != nil {
			return nil, err
		}
	}
	return have, nil
}

// overCap returns, by host, the sessions that fall outside the cap: walking
// every live session newest first (ties by host, then ID), the first one that
// would take the total over limit and everything older than it.
func overCap(all map[string]manifest, limit int64) map[string][]string {
	type keyed struct {
		host, id string
		entry    manifestEntry
	}
	var live []keyed
	for host, m := range all {
		for id, e := range m {
			if !e.Dropped {
				live = append(live, keyed{host, id, e})
			}
		}
	}
	sort.Slice(live, func(i, j int) bool {
		a, b := live[i], live[j]
		if !a.entry.MTime.Equal(b.entry.MTime) {
			return a.entry.MTime.After(b.entry.MTime)
		}
		if a.host != b.host {
			return a.host < b.host
		}
		return a.id < b.id
	})
	drops := map[string][]string{}
	var total int64
	for i, k := range live {
		if total+k.entry.Bytes > limit {
			for _, d := range live[i:] {
				drops[d.host] = append(drops[d.host], d.id)
			}
			break
		}
		total += k.entry.Bytes
	}
	return drops
}

// sortedRecords orders a host's records newest first, so its file is stable.
func sortedRecords(recs map[string]Record) []Record {
	out := make([]Record, 0, len(recs))
	for _, r := range recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].MTime.Equal(out[j].MTime) {
			return out[i].MTime.After(out[j].MTime)
		}
		return out[i].ID < out[j].ID
	})
	return out
}
```

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add lock.go ingest.go ingest_test.go
git commit -m "feat: ingest deltas on the index host under a 5 GB rolling cap

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Debounced push and its transport

**Files:**
- Create: `push.go`
- Test: `push_test.go`

- [ ] **Step 1: Write the failing tests** (`push_test.go`)

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := pushInterval
	pushInterval = d
	t.Cleanup(func() { pushInterval = old })
}

func TestAPendingMarkDuringAPushCausesOneMoreRound(t *testing.T) {
	withInterval(t, 0)
	dir := t.TempDir()
	calls := 0
	p := pusher{dir: dir, send: func() error {
		calls++
		if calls == 1 {
			touch(filepath.Join(dir, "push.pending"))
		}
		return nil
	}}
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("sent %d times, want 2", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "push.stamp")); err != nil {
		t.Error("no stamp after a successful push")
	}
}

func TestASecondPushLeavesTheWorkToTheOneHoldingTheLock(t *testing.T) {
	dir := t.TempDir()
	unlock, ok, err := lockFile(filepath.Join(dir, "push.lock"), false)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer unlock()

	calls := 0
	p := pusher{dir: dir, send: func() error { calls++; return nil }}
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("sent %d times while another push held the lock", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "push.pending")); err != nil {
		t.Error("the pending mark was not left for the holder")
	}
}

func TestAPushWaitsOutTheInterval(t *testing.T) {
	withInterval(t, 300*time.Millisecond)
	dir := t.TempDir()
	touch(filepath.Join(dir, "push.stamp"))
	start := time.Now()
	p := pusher{dir: dir, send: func() error { return nil }}
	if err := p.run(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("pushed after %v, before the interval was up", elapsed)
	}
}

func TestPushOnceFallsBackToAFullPushWhenTheIndexForgot(t *testing.T) {
	root := writeTranscript(t, "-home-me-c-x", "s1",
		`{"type":"user","cwd":"/home/me/c/x","message":{"content":"hello from the pc"}}`)
	cache, index := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", root)
	t.Setenv("CLAUDE_SESSIONS_REMOTE", index)
	t.Setenv("CLAUDE_SESSIONS_HOST", "pc")

	if err := pushOnce(cache); err != nil {
		t.Fatal(err)
	}
	records, _ := readRecordFile(filepath.Join(index, "pc.jsonl"))
	if len(records) != 1 || records[0].First != "hello from the pc" {
		t.Fatalf("first push stored %+v", records)
	}

	// The index loses this host: the next delta no longer matches anything.
	os.Remove(filepath.Join(index, "pc.manifest.json"))
	os.Remove(filepath.Join(index, "pc.jsonl"))
	if err := pushOnce(cache); err != nil {
		t.Fatal(err)
	}
	if records, _ = readRecordFile(filepath.Join(index, "pc.jsonl")); len(records) != 1 {
		t.Errorf("after a resync the index holds %d records", len(records))
	}
}

func TestThePushLogKeepsTheLastHundredLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "push.log")
	for i := 0; i < 150; i++ {
		appendLog(path, "line")
	}
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "\n"); n != 100 {
		t.Errorf("log has %d lines", n)
	}
}
```

- [ ] **Step 2: Run them and check that they fail**

Run: `go test -run 'Pending|SecondPush|Interval|PushOnce|PushLog' ./...`
Expected: they fail to compile with `undefined: pusher`.

- [ ] **Step 3: Implement** (`push.go`)

```go
// Pushing this machine's index to the index host: debounced, so a burst of
// turns costs one push, and incremental, so a push carries only what changed.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// pushInterval is the least time between two pushes from one machine.
var pushInterval = time.Minute

// pusher runs send under push.lock in dir, at most once per pushInterval,
// again for as long as new pushes are asked for while one is running.
type pusher struct {
	dir  string
	send func() error
}

func (p pusher) run() error {
	pending := filepath.Join(p.dir, "push.pending")
	if err := touch(pending); err != nil {
		return err
	}
	unlock, ok, err := lockFile(filepath.Join(p.dir, "push.lock"), false)
	if err != nil {
		return err
	}
	if !ok {
		// Another push is running; it sees the pending mark and goes again.
		return nil
	}
	defer unlock()

	stamp := filepath.Join(p.dir, "push.stamp")
	for {
		if info, err := os.Stat(stamp); err == nil {
			if wait := pushInterval - time.Since(info.ModTime()); wait > 0 {
				time.Sleep(wait)
			}
		}
		os.Remove(pending)
		if err := p.send(); err != nil {
			return err
		}
		if err := touch(stamp); err != nil {
			return err
		}
		if _, err := os.Stat(pending); err != nil {
			return nil
		}
	}
}

// touch creates path if needed and sets its mtime to now.
func touch(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	file.Close()
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// pushOnce exports this machine's sessions and sends the index host what
// changed since the last push. When the index host does not recognise the
// manifest the delta was built against, it sends everything instead.
func pushOnce(cache string) error {
	sources, err := projectSources()
	if err != nil {
		return err
	}
	host := localHost()
	records, err := exportRecords(sources, host, filepath.Join(cache, "local.json"))
	if err != nil {
		return err
	}
	pushedPath := filepath.Join(cache, "pushed.json")
	pushed, havePushed, err := readManifest(pushedPath)
	if err != nil {
		havePushed = false
	}
	full := !havePushed
	for {
		var delta bytes.Buffer
		if err := writeDelta(&delta, records, pushed, full); err != nil {
			return err
		}
		got, err := ingestAt(host, &delta)
		if errors.Is(err, errResync) && !full {
			full = true
			continue
		}
		if err != nil {
			return err
		}
		return writeManifest(pushedPath, got)
	}
}

// ingestAt hands a delta to the index: in this process when this machine is
// the index host or CLAUDE_SESSIONS_REMOTE names a local index directory,
// otherwise over ssh to the index host's own claude-sessions.
func ingestAt(host string, delta io.Reader) (manifest, error) {
	if dir := os.Getenv("CLAUDE_SESSIONS_REMOTE"); dir != "" {
		return store{dir}.ingest(host, delta, capBytes())
	}
	if host == indexHost() {
		cache, err := cacheDir()
		if err != nil {
			return nil, err
		}
		return store{filepath.Join(cache, "hosts")}.ingest(host, delta, capBytes())
	}

	cmd := exec.Command("ssh", "-C", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", indexHost(),
		"~/c/claude-sessions/claude-sessions --ingest "+shellQuote(host))
	cmd.Stdin = delta
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == resyncExit {
		return nil, errResync
	}
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %v: %s", indexHost(), err, strings.TrimSpace(stderr.String()))
	}
	got := manifest{}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		return nil, fmt.Errorf("ssh %s: unreadable manifest: %w", indexHost(), err)
	}
	return got, nil
}

// appendLog adds a line to the push log and keeps only its last 100 lines.
func appendLog(path, line string) {
	data, _ := os.ReadFile(path)
	var lines []string
	if text := strings.TrimRight(string(data), "\n"); text != "" {
		lines = strings.Split(text, "\n")
	}
	lines = append(lines, line)
	if len(lines) > 100 {
		lines = lines[len(lines)-100:]
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}
```

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add push.go push_test.go
git commit -m "feat: debounced, incremental push to the index host

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `--export`, `--ingest`, `--push`

**Files:**
- Create: `cli.go`
- Modify: `main.go` (top of `main`)

- [ ] **Step 1: Implement** (`cli.go`)

```go
// The subcommands that keep the shared index: --push on every machine (run by
// the hook and the timer), --ingest on the index host, --export for a look at
// what a push would send.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// runCommand runs a subcommand and reports false when args name none.
func runCommand(args []string) (exit int, handled bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "--export":
		return runExport(args[1:]), true
	case "--ingest":
		return runIngest(args[1:]), true
	case "--push":
		runPush()
		return 0, true
	}
	return 0, false
}

// runExport writes the delta a push would send now; --full writes everything.
func runExport(args []string) int {
	full := len(args) == 1 && args[0] == "--full"
	cache, err := cacheDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	sources, err := projectSources()
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	records, err := exportRecords(sources, localHost(), filepath.Join(cache, "local.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	pushed, _, _ := readManifest(filepath.Join(cache, "pushed.json"))
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	if err := writeDelta(out, records, pushed, full); err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	return 0
}

// runIngest applies a delta from stdin for the named host and prints that
// host's new manifest. It exits resyncExit when the host must push in full.
func runIngest(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: claude-sessions --ingest <host>")
		return 1
	}
	dir := os.Getenv("CLAUDE_SESSIONS_REMOTE")
	if dir == "" {
		cache, err := cacheDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "claude-sessions:", err)
			return 1
		}
		dir = filepath.Join(cache, "hosts")
	}
	got, err := store{dir}.ingest(args[0], os.Stdin, capBytes())
	if errors.Is(err, errResync) {
		return resyncExit
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-sessions:", err)
		return 1
	}
	if err := json.NewEncoder(os.Stdout).Encode(got); err != nil {
		return 1
	}
	return 0
}

// runPush never fails loudly: it runs behind Claude Code's hooks and a timer,
// so a failure is logged and the next run tries again.
func runPush() {
	cache, err := cacheDir()
	if err != nil {
		return
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return
	}
	p := pusher{dir: cache, send: func() error { return pushOnce(cache) }}
	if err := p.run(); err != nil {
		appendLog(filepath.Join(cache, "push.log"), time.Now().Format(time.RFC3339)+" "+err.Error())
	}
}
```

In `main.go`, insert at the very top of `func main()`, before `sources, err := projectSources()`:

```go
	if exit, handled := runCommand(os.Args[1:]); handled {
		os.Exit(exit)
	}
```

- [ ] **Step 2: Build and check it by hand against a local index**

```bash
go build -o /tmp/cs-dev . && idx=$(mktemp -d) && CLAUDE_SESSIONS_REMOTE="$idx" /tmp/cs-dev --push; ls "$idx"; head -c 300 "$idx"/*.jsonl; echo; CLAUDE_SESSIONS_REMOTE="$idx" /tmp/cs-dev --export | wc -l
```

Expected: `immunemac.jsonl` and `immunemac.manifest.json` show up, and the head shows one record
with `"host":"immunemac"`. The `--export` count is about 1: only the header, since nothing changed.
(`--push` writes `~/.cache/claude-sessions/pushed.json` for that throwaway index. The first real push
in Task 13 doesn't recognize that manifest, gets a resync, and sends everything, which is the
designed behavior.)

```bash
printf '{"base":"nope"}\n' | CLAUDE_SESSIONS_REMOTE="$idx" /tmp/cs-dev --ingest immunemac; echo "exit $?"
```

Expected: `exit 3`

- [ ] **Step 3: Run all tests**

Run: `go test ./...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add cli.go main.go
git commit -m "feat: --export, --ingest and --push subcommands

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Rows from the other machines in the TUI

**Files:**
- Create: `remote.go`
- Modify: `main.go` (model fields, `newModel`, `Init`, `Update`, `titleLine`, `headerSegments`, styles, `main`)
- Test: `remote_test.go`, `sessions_test.go` (styles test)

- [ ] **Step 1: Write the failing tests** (`remote_test.go`)

```go
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteRowsMergeByAgeAndKeepTheCursor(t *testing.T) {
	now := time.Now()
	m := newModel([]Session{
		{ConvID: "l1", Cwd: "/a", MTime: now.Add(-1 * time.Hour)},
		{ConvID: "l2", Cwd: "/b", MTime: now.Add(-3 * time.Hour)},
	}, scope{})
	m.cursor = 1 // on l2

	next, _ := m.Update(remoteMsg{live: true, sessions: []Session{
		{ConvID: "r1", Host: "pc", Cwd: "/home/me/c", MTime: now.Add(-2 * time.Hour)},
		{ConvID: "r0", Host: "pc", Cwd: "/home/me/c", MTime: now},
	}})
	got := next.(model)

	var order []string
	for _, s := range got.filtered {
		order = append(order, s.ConvID)
	}
	if strings.Join(order, " ") != "r0 l1 r1 l2" {
		t.Errorf("order = %v", order)
	}
	if sel := got.selected(); sel == nil || sel.ConvID != "l2" {
		t.Errorf("cursor moved to %+v", sel)
	}
}

func TestALateCacheReadDoesNotReplaceLiveRows(t *testing.T) {
	m := newModel(nil, scope{})
	next, _ := m.Update(remoteMsg{live: true, sessions: []Session{{ConvID: "fresh", Host: "pc"}}})
	next, _ = next.(model).Update(remoteMsg{synced: time.Now().Add(-time.Hour), sessions: []Session{{ConvID: "stale", Host: "pc"}}})
	got := next.(model)
	if len(got.sessions) != 1 || got.sessions[0].ConvID != "fresh" {
		t.Errorf("sessions = %+v", got.sessions)
	}
	if !strings.Contains(got.titleLine(), "remote: live") {
		t.Errorf("title = %q", got.titleLine())
	}
}

func TestTheTitleSaysHowOldTheRemoteRowsAre(t *testing.T) {
	m := newModel(nil, scope{})
	if strings.Contains(m.titleLine(), "remote:") {
		t.Errorf("no remote data, yet the title says %q", m.titleLine())
	}
	next, _ := m.Update(remoteMsg{synced: m.now.Add(-3 * time.Hour), sessions: []Session{{ConvID: "r", Host: "pc"}}})
	if title := next.(model).titleLine(); !strings.Contains(title, "remote: cached 3h ago") {
		t.Errorf("title = %q", title)
	}
}

func TestRemoteRowsShowTheirHostAndMatchIt(t *testing.T) {
	m := newModel([]Session{{ConvID: "l", Cwd: "/a", First: "x", Last: "y"}}, scope{})
	m.width = 120
	next, _ := m.Update(remoteMsg{live: true, sessions: []Session{{ConvID: "r", Host: "pc", Cwd: "/home/me/c", First: "x", Last: "y"}}})
	got := next.(model)

	var row string
	for _, s := range got.filtered {
		if s.ConvID == "r" {
			row = got.renderRow(s, false)
		}
	}
	if !strings.Contains(row, "pc") {
		t.Errorf("remote row has no host: %q", row)
	}
	got.query = "pc"
	got.applyFilter()
	if len(got.filtered) != 1 || got.filtered[0].ConvID != "r" {
		t.Errorf("filtering by host kept %d rows", len(got.filtered))
	}
}

func TestRemoteRowsRespectTheDirectoryScope(t *testing.T) {
	m := newModel(nil, scope{path: "/Users/me/c/proj"})
	next, _ := m.Update(remoteMsg{live: true, sessions: []Session{
		{ConvID: "in", Host: "pc", Cwd: "/home/me/c/proj/sub"},
		{ConvID: "out", Host: "pc", Cwd: "/home/me/c/other"},
	}})
	got := next.(model)
	if len(got.sessions) != 1 || got.sessions[0].ConvID != "in" {
		t.Errorf("sessions = %+v", got.sessions)
	}
}

func TestTheRemoteSourceDropsThisHostAndBadLines(t *testing.T) {
	dir := t.TempDir()
	writeRecordFile(filepath.Join(dir, "pc.jsonl"), []Record{{Host: "pc", ID: "a"}})
	writeRecordFile(filepath.Join(dir, "immunemac.jsonl"), []Record{{Host: "immunemac", ID: "b"}})
	f, _ := os.OpenFile(filepath.Join(dir, "pc.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("garbage\n")
	f.Close()

	sessions := remoteSource{dir: dir, self: "immunemac"}.read()
	if len(sessions) != 1 || sessions[0].ConvID != "a" || sessions[0].Host != "pc" {
		t.Errorf("sessions = %+v", sessions)
	}
}

func TestFetchSyncsTheMirrorWithRsync(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	t.Setenv("HOME", t.TempDir())
	index := t.TempDir()
	t.Setenv("CLAUDE_SESSIONS_REMOTE", index)
	t.Setenv("CLAUDE_SESSIONS_HOST", "immunemac")
	writeRecordFile(filepath.Join(index, "pc.jsonl"), []Record{{Host: "pc", ID: "a", MTime: time.Now()}})
	writeManifest(filepath.Join(index, "pc.manifest.json"), manifest{})

	src, err := newRemoteSource()
	if err != nil {
		t.Fatal(err)
	}
	live, ok := fetchRemote(src)().(remoteMsg)
	if !ok || !live.live || len(live.sessions) != 1 {
		t.Fatalf("fetch = %+v", live)
	}
	if _, err := os.Stat(filepath.Join(src.dir, "pc.manifest.json")); !os.IsNotExist(err) {
		t.Error("the manifest was mirrored")
	}
	cached := loadCachedRemote(src)().(remoteMsg)
	if cached.live || len(cached.sessions) != 1 || time.Since(cached.synced) > time.Minute {
		t.Errorf("cached = %+v", cached)
	}
}

func TestHomeRelativeMatchesMacAndLinuxHomes(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/me/c/x": "~/c/x",
		"/home/me/c/x":  "~/c/x",
		"/home/me":      "~",
		"/opt/x":        "/opt/x",
	} {
		if got := homeRelative(in); got != want {
			t.Errorf("homeRelative(%q) = %q, want %q", in, got, want)
		}
	}
}
```

In `sessions_test.go`, add `"host": selectedHost,` to the map in
`TestEverySelectedStyleCarriesTheSelectionBackground`.

- [ ] **Step 2: Run them and check that they fail**

Run: `go test ./...`
Expected: it fails to compile with `undefined: remoteMsg`.

- [ ] **Step 3: Implement**

`remote.go`:

```go
// The other machines' sessions, read from a mirror of the index host's files.
//
// Startup reads the mirror as it is, so those rows show up at once, and an
// rsync in the background brings it up to date: only the changed parts of each
// host's file cross the network, compressed. On the index host itself there is
// no mirror; it reads the index directly.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// remoteMsg carries the other machines' sessions.
type remoteMsg struct {
	sessions []Session
	synced   time.Time // when the mirror last matched the index host
	live     bool      // read right after a successful sync
}

// remoteSource is where the other machines' host files are read from.
type remoteSource struct {
	dir      string // directory of <host>.jsonl files
	rsyncSrc string // what to rsync dir from; "" when dir is the index itself
	self     string // this machine, whose rows come from its own transcripts
}

// fetchTimeout bounds the background rsync.
var fetchTimeout = 30 * time.Second

func newRemoteSource() (remoteSource, error) {
	cache, err := cacheDir()
	if err != nil {
		return remoteSource{}, err
	}
	self := localHost()
	if dir := os.Getenv("CLAUDE_SESSIONS_REMOTE"); dir != "" {
		return remoteSource{dir: filepath.Join(cache, "remote"), rsyncSrc: strings.TrimRight(dir, "/") + "/", self: self}, nil
	}
	if self == indexHost() {
		return remoteSource{dir: filepath.Join(cache, "hosts"), self: self}, nil
	}
	return remoteSource{
		dir:      filepath.Join(cache, "remote"),
		rsyncSrc: indexHost() + ":.cache/claude-sessions/hosts/",
		self:     self,
	}, nil
}

// read loads every host file in the source, leaving out this machine's rows:
// its own transcripts are newer than anything it pushed.
func (r remoteSource) read() []Session {
	paths, _ := filepath.Glob(filepath.Join(r.dir, "*.jsonl"))
	var sessions []Session
	for _, path := range paths {
		records, err := readRecordFile(path)
		if err != nil {
			continue
		}
		for _, rec := range records {
			if rec.Host == "" || rec.Host == r.self {
				continue
			}
			sessions = append(sessions, rec.session())
		}
	}
	return sessions
}

// syncedAt is when the mirror last matched the index host.
func (r remoteSource) syncedAt() time.Time {
	if r.rsyncSrc == "" {
		return time.Now()
	}
	info, err := os.Stat(filepath.Join(r.dir, ".synced"))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// sync brings the mirror up to date. rsync writes each file under a temporary
// name and renames it, so a sync that dies halfway leaves whole files behind.
func (r remoteSource) sync() error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rsync", "-az", "--delete",
		"--include=*.jsonl", "--exclude=*",
		"-e", "ssh -o BatchMode=yes -o ConnectTimeout=5",
		r.rsyncSrc, r.dir+"/")
	if err := cmd.Run(); err != nil {
		return err
	}
	return touch(filepath.Join(r.dir, ".synced"))
}

// loadCachedRemote reads the mirror as it is.
func loadCachedRemote(r remoteSource) tea.Cmd {
	return func() tea.Msg {
		return remoteMsg{sessions: r.read(), synced: r.syncedAt(), live: r.rsyncSrc == ""}
	}
}

// fetchRemote syncs the mirror and reads it. A failed sync sends nothing, so
// the cached rows stay and the title keeps saying how old they are.
func fetchRemote(r remoteSource) tea.Cmd {
	if r.rsyncSrc == "" {
		return nil
	}
	return func() tea.Msg {
		if err := r.sync(); err != nil {
			return nil
		}
		return remoteMsg{sessions: r.read(), synced: r.syncedAt(), live: true}
	}
}

var homePrefix = regexp.MustCompile(`^/(Users|home)/[^/]+`)

// homeRelative writes a path under a home directory as ~/…, so a directory on
// the Mac and the same directory on the PC compare equal: the repos under ~/c
// mirror each other, but the homes are /Users/… and /home/….
func homeRelative(path string) string {
	return homePrefix.ReplaceAllString(path, "~")
}

// coversRemote is coversPath for a session from another machine.
func (s scope) coversRemote(cwd string) bool {
	if s.path == "" {
		return true
	}
	return scope{path: homeRelative(s.path)}.coversPath(homeRelative(cwd))
}
```

In `main.go`:

1. Add `"sort"` to the imports.
2. In the style block, add these after `accountStyle`:
   `hostStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))`.
   Add these after `selectedAccount`:
   `selectedHost    = selectedRow.Foreground(lipgloss.Color("157"))`.
3. In `model`, add these after `pending []transcript`:

```go
	// local are this machine's sessions, remote the other machines' from the
	// shared index; sessions is both merged into one timeline.
	local  []Session
	remote []Session
	// remoteSrc is where the other machines' rows come from; nil for none.
	remoteSrc *remoteSource
	// remoteSeen, remoteLive and remoteSynced describe the remote rows for the
	// title: whether there are any, whether they were just synced, and when
	// they were last synced.
	remoteSeen   bool
	remoteLive   bool
	remoteSynced time.Time
```

4. Replace `newModel` with:

```go
func newModel(sessions []Session, within scope) model {
	for i := range sessions {
		sessions[i].index()
	}
	m := model{local: sessions, scope: within, now: time.Now(), width: 80, height: 24}
	m.rebuild()
	return m
}

// rebuild merges this machine's sessions with the other machines' into one
// timeline, newest first, and keeps the cursor on the session it was on.
func (m *model) rebuild() {
	keep := m.selected()
	merged := make([]Session, 0, len(m.local)+len(m.remote))
	merged = append(merged, m.local...)
	merged = append(merged, m.remote...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].MTime.After(merged[j].MTime) })
	m.sessions = merged
	m.applyFilter()
	if keep == nil {
		return
	}
	for i, s := range m.filtered {
		if s.ConvID == keep.ConvID && s.Host == keep.Host {
			m.cursor = i
			break
		}
	}
	m.scrollIntoView()
}
```

5. Replace `Init` with:

```go
func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{loadNext(m.pending, m.scope)}
	if m.remoteSrc != nil {
		cmds = append(cmds, loadCachedRemote(*m.remoteSrc), fetchRemote(*m.remoteSrc))
	}
	return tea.Batch(cmds...)
}
```

6. In `Update`, replace the `batchMsg` case and add a `remoteMsg` case:

```go
	case batchMsg:
		m.local = append(m.local, msg.sessions...)
		m.pending = msg.rest
		m.rebuild()
		return m, loadNext(m.pending, m.scope)

	case remoteMsg:
		// The cache read and the sync race; stale rows must not replace fresh ones.
		if m.remoteLive && !msg.live {
			return m, nil
		}
		var remote []Session
		for _, s := range msg.sessions {
			if m.scope.coversRemote(s.Cwd) {
				s.index()
				remote = append(remote, s)
			}
		}
		m.remote = remote
		m.remoteSeen, m.remoteLive, m.remoteSynced = true, msg.live, msg.synced
		m.rebuild()
		return m, nil
```

7. In `titleLine`, insert this before `if m.scope.path != "" {`:

```go
	if status := m.remoteStatus(); status != "" {
		title += "  " + ageStyle.Render(status)
	}
```

And add this function after `titleLine`:

```go
// remoteStatus tells whether the other machines' rows are fresh or how old
// they are; it is empty when there are none at all.
func (m model) remoteStatus() string {
	switch {
	case !m.remoteSeen:
		return ""
	case m.remoteLive:
		return "remote: live"
	case m.remoteSynced.IsZero():
		return "remote: cached"
	default:
		return "remote: cached " + timeAgo(m.now.Sub(m.remoteSynced))
	}
}
```

8. Replace `headerSegments` with:

```go
// headerSegments is a row's first line: where the session ran, on which other
// machine if not this one, which account it belongs to, and how long ago it
// was touched. The path gives way to the rest, since they are short and none
// survives being cut.
func (m model) headerSegments(s Session, width int, selected bool) []segment {
	account := s.Account.label()
	room := width - 14
	if s.Host != "" {
		room -= len([]rune(s.Host)) + 2
	}
	if account != "" {
		room -= len([]rune(account)) + 2
	}

	segments := []segment{
		m.gutter(selected),
		{truncate(s.Cwd, room), pathStyle, selectedPath},
	}
	if s.Host != "" {
		segments = append(segments,
			segment{"  ", plainStyle, selectedRow},
			segment{s.Host, hostStyle, selectedHost})
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
```

9. In `main()`, after `m.pending = pending`, add:

```go
	if src, err := newRemoteSource(); err == nil {
		m.remoteSrc = &src
	}
```

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS. `TestSessionsStreamInBatchesInOrder` still passes, because with `remoteSrc` nil,
`tea.Batch` returns `loadNext` directly.

- [ ] **Step 5: Commit**

```bash
git add remote.go remote_test.go main.go sessions_test.go
git commit -m "feat: list the other machines' sessions from a synced mirror

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Resume a session where it ran

**Files:**
- Modify: `sessions.go` (add `resumeArgv`)
- Modify: `main.go` (end of `main`)
- Test: `accounts_test.go`

- [ ] **Step 1: Write the failing tests** (append to `accounts_test.go`)

```go
func TestARemoteSessionResumesOverSSHOnItsHost(t *testing.T) {
	s := Session{ConvID: "abc", Cwd: "/home/me/c/it's", Host: "pc", Account: account{number: 2, email: "me@x.com"}}
	inner := `cd '/home/me/c/it'\''s' && cswap run 2 -- --resume 'abc'`
	want := []string{"ssh", "-t", "pc", `exec "$SHELL" -lic ` + shellQuote(inner)}
	if got := resumeArgv(s, "/bin/zsh"); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q\nwant   %q", got, want)
	}
}

func TestALocalSessionResumesThroughTheShell(t *testing.T) {
	s := Session{ConvID: "abc", Cwd: "/tmp/proj"}
	want := []string{"/bin/zsh", "-c", `cd '/tmp/proj' && claude --resume 'abc'`}
	if got := resumeArgv(s, "/bin/zsh"); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q", got)
	}
}
```

Add `"reflect"` to `accounts_test.go`'s imports if it isn't there yet.

- [ ] **Step 2: Run them and check that they fail**

Run: `go test -run 'Resumes' ./...`
Expected: they fail to compile with `undefined: resumeArgv`.

- [ ] **Step 3: Implement**

In `sessions.go`, after `resumeCommand`:

```go
// resumeArgv is the program and arguments that resume a session. A session
// from another machine resumes on that machine, over ssh with a terminal,
// through its login shell so its PATH finds claude and cswap; that machine has
// the working tree, the transcript, and the account.
func resumeArgv(s Session, shell string) []string {
	if s.Host != "" {
		return []string{"ssh", "-t", s.Host, `exec "$SHELL" -lic ` + shellQuote(resumeCommand(s))}
	}
	return []string{shell, "-c", resumeCommand(s)}
}
```

In `main.go`, add `"os/exec"` to the imports, and replace everything from
`// Report the directory before exec'ing:` to the end of `main` with:

```go
	// Report the directory before exec'ing: after exec there is no more of this
	// process left to run, and the shell function needs the answer regardless of
	// how the session ends. A session on another machine leaves this shell
	// where it is.
	if final.chosen.Host == "" {
		reportCwd(final.chosen.Cwd)
	}

	// Hand the terminal over: exec replaces this process, so the session runs
	// where the TUI was, with no wrapper left behind.
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	argv := resumeArgv(*final.chosen, shell)
	path, err := exec.LookPath(argv[0])
	if err == nil {
		err = syscall.Exec(path, argv, os.Environ())
	}
	fmt.Fprintln(os.Stderr, "claude-sessions: could not resume:", err)
	os.Exit(1)
}
```

- [ ] **Step 4: Run all tests**

Run: `go test ./... && go vet ./...`
Expected: PASS, and vet prints nothing.

- [ ] **Step 5: Commit**

```bash
git add sessions.go main.go accounts_test.go
git commit -m "feat: resume another machine's session over ssh on that machine

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: README

**Files:**
- Modify: `README.md` (new section before `## Running it`)

- [ ] **Step 1: Add the section**

```markdown
## Other machines

The list also holds the sessions of the other machines: immunemac, the
macmini and the PC. Each machine pushes an index of its sessions to the
macmini, holding each session's first and last message, all of its prose and
where it ran, and every launch reads a mirror of the macmini's copy. Those rows
carry the machine's name next to the project path, and the filter matches it,
so `/pc` narrows the list to the PC. The filter matches the full prose of
every session, yours and Claude's, on every machine.

Startup does not wait for the network. The mirror is read as it is, and an
`rsync` in the background brings it up to date; the title says `remote: live`
once it has, or `remote: cached 3h ago` when the macmini is out of reach.

`enter` on another machine's session resumes it there:
`ssh -t <host>` runs the same `cd … && claude --resume` (or `cswap run`) in
that machine's login shell.

Pushes come from a Claude Code hook after every turn and at the end of every
session, and from a 10-minute timer, all installed by `~/c/agents-configs`
(`hooks/claude-sessions-push.sh`). A push sends only the sessions that changed,
and at most one goes out a minute. `claude-sessions --push` runs one by hand;
`~/.cache/claude-sessions/push.log` holds the last errors.

The index covers the newest 5 GB of transcripts across all machines. Past
that, the oldest sessions drop out of the index to make room; their transcripts
stay on disk.
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: README: sessions from the other machines

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: The push hook wrapper (agents-configs)

**Files (in `~/c/agents-configs`):**
- Create: `hooks/claude-sessions-push.sh`
- Test: `tests/claude-sessions-push.test.sh`

- [ ] **Step 1: Write the failing test** (`tests/claude-sessions-push.test.sh`, then `chmod +x`)

```sh
#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
HOOK="$ROOT_DIR/hooks/claude-sessions-push.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

# A stand-in claude-sessions that takes a second and records its arguments.
home="$work/home"
mkdir -p "$home/c/claude-sessions"
cat >"$home/c/claude-sessions/claude-sessions" <<'EOF'
#!/bin/sh
sleep 1
printf '%s\n' "$*" >>"$HOME/calls.log"
EOF
chmod +x "$home/c/claude-sessions/claude-sessions"

# As a hook it returns before the push is done, and the push still runs.
HOME="$home" "$HOOK" </dev/null
[ ! -e "$home/calls.log" ] || fail "the hook waited for the push"
sleep 2
grep -qx -- --push "$home/calls.log" || fail "the detached push did not run"

# With --wait, as the timers run it, the push runs in the foreground.
rm "$home/calls.log"
HOME="$home" "$HOOK" --wait
grep -qx -- --push "$home/calls.log" || fail "--wait did not run the push before returning"

# Without claude-sessions checked out it is a quiet no-op.
HOME="$work/empty" "$HOOK" </dev/null || fail "the hook failed without claude-sessions"

printf 'ok\n'
```

- [ ] **Step 2: Run it and check that it fails**

Run: `cd ~/c/agents-configs && sh tests/claude-sessions-push.test.sh`
Expected: it fails because `hooks/claude-sessions-push.sh` doesn't exist.

- [ ] **Step 3: Implement** (`hooks/claude-sessions-push.sh`, then `chmod +x`)

```sh
#!/bin/sh
# Push this machine's claude-sessions index to the macmini, so the other
# machines can search and resume its sessions. Run by Claude Code's Stop and
# SessionEnd hooks, which must not wait on the network: the push is detached
# and this returns at once. The timers pass --wait to run it in the foreground,
# since launchd and systemd end a job's leftover processes when it exits.
set -u

# Hooks and timers get a bare PATH; the claude-sessions launcher may need Go to
# rebuild, and the push needs ssh.
PATH="/opt/homebrew/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:$PATH"
export PATH

bin="$HOME/c/claude-sessions/claude-sessions"
[ -x "$bin" ] || exit 0

log_dir="$HOME/.cache/claude-sessions"
mkdir -p "$log_dir"

if [ "${1:-}" = --wait ]; then
  exec "$bin" --push </dev/null >>"$log_dir/push.log" 2>&1
fi

nohup "$bin" --push </dev/null >>"$log_dir/push.log" 2>&1 &
exit 0
```

- [ ] **Step 4: Run the test and shellcheck**

```bash
cd ~/c/agents-configs && sh tests/claude-sessions-push.test.sh && shellcheck hooks/claude-sessions-push.sh tests/claude-sessions-push.test.sh
```

Expected: `ok`, and shellcheck prints nothing.

- [ ] **Step 5: Commit**

```bash
cd ~/c/agents-configs && git add hooks/claude-sessions-push.sh tests/claude-sessions-push.test.sh && git commit -m "feat: hook that pushes the claude-sessions index in the background

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Register the hook in settings.json (agents-configs)

**Files (in `~/c/agents-configs`):**
- Modify: `install.sh` (new function after `register_env_guard`, and a call after `register_env_guard`)
- Test: `tests/install.test.sh`

- [ ] **Step 1: Write the failing test** (insert in `tests/install.test.sh` right after the
  "A foreign statusLine command" case)

```sh
# claude-sessions pushes after every turn and at session end: one entry each,
# next to whatever hooks were already there, and never twice.
pushhome="$work/pushhome"
mkdir -p "$(cfg "$pushhome")"
printf '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"other"}]}]}}\n' >"$(cfg "$pushhome")/settings.json"
output=$(run "$pushhome")
assert_contains "session push: registered in settings.json" "$output"
for event in Stop SessionEnd; do
  n=$(jq --arg e "$event" '[.hooks[$e][].hooks[] | select(.command == "~/.claude/hooks/claude-sessions-push.sh")] | length' "$(cfg "$pushhome")/settings.json")
  [ "$n" = 1 ] || fail "expected one $event push hook, got $n"
done
[ "$(jq -r '.hooks.Stop[0].hooks[0].command' "$(cfg "$pushhome")/settings.json")" = other ] || fail "an existing Stop hook was lost"
output=$(run "$pushhome")
assert_contains "session push: already set in settings.json" "$output"
```

- [ ] **Step 2: Run it and check that it fails**

Run: `cd ~/c/agents-configs && sh tests/install.test.sh`
Expected: it fails with `expected output to contain session push: registered in settings.json`.

- [ ] **Step 3: Implement** (in `install.sh`, after `register_env_guard`'s closing `}`)

```bash
# Push this machine's claude-sessions index to the macmini after every turn and
# when a session ends, so every machine can search every machine's sessions.
# Merged into settings.json next to whatever hooks are there; re-running adds
# nothing twice.
register_session_push() {
  local settings="$claude_dir/settings.json"

  if ! command -v jq >/dev/null 2>&1; then
    echo "session push: skipped (jq not installed)"
    return 0
  fi

  if [ ! -f "$settings" ]; then
    echo '{}' >"$settings"
  fi

  # Literal ~: settings.json stays portable across machines' home paths.
  # shellcheck disable=SC2088
  local hook_cmd="~/.claude/hooks/claude-sessions-push.sh"
  local tmp
  tmp=$(mktemp)
  jq --arg cmd "$hook_cmd" '
    reduce ("Stop", "SessionEnd") as $event (.;
      if any(.hooks[$event][]?.hooks[]?; .command == $cmd) then .
      else .hooks[$event] = ((.hooks[$event] // []) + [{
        hooks: [{type: "command", command: $cmd, timeout: 10}]
      }]) end)
  ' "$settings" >"$tmp"

  if cmp -s <(jq -S . "$tmp") <(jq -S . "$settings"); then
    rm "$tmp"
    echo "session push: already set in settings.json"
  else
    mv "$tmp" "$settings"
    echo "session push: registered in settings.json"
  fi
}
```

In the main sequence at the bottom, add `register_session_push` on the line after
`register_env_guard`.

- [ ] **Step 4: Run the tests and shellcheck**

```bash
cd ~/c/agents-configs && sh tests/install.test.sh && shellcheck install.sh
```

Expected: `ok`, and shellcheck reports nothing new. Compare against `git stash; shellcheck install.sh; git stash pop`
if it prints anything.

- [ ] **Step 5: Commit**

```bash
cd ~/c/agents-configs && git add install.sh tests/install.test.sh && git commit -m "feat: register the claude-sessions push on Stop and SessionEnd

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: The 10-minute push timer (agents-configs)

**Files (in `~/c/agents-configs`):**
- Modify: `install.sh` (new function after `register_session_push`, and a call after it)
- Test: `tests/install.test.sh`

- [ ] **Step 1: Add the launchctl and systemctl stubs, and write the failing test**

In `tests/install.test.sh`, right after the `chmod +x "$fakebin/claude"` line, add:

```sh
# Stand-in launchctl and systemctl: log every call into the fake HOME, and
# remember a load or an enable so the next run sees the timer as running.
cat >"$fakebin/launchctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$HOME/launchctl.log"
case "$1" in
  print) [ -e "$HOME/launchctl-loaded" ] ;;
  bootstrap) : >"$HOME/launchctl-loaded" ;;
esac
EOF
cat >"$fakebin/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$HOME/systemctl.log"
case "$2" in
  is-enabled) [ -e "$HOME/systemctl-enabled" ] ;;
  enable) : >"$HOME/systemctl-enabled" ;;
esac
EOF
chmod +x "$fakebin/launchctl" "$fakebin/systemctl"
```

Check that every invocation of `"$SCRIPT"` in the file puts `$fakebin` first on `PATH`, so no test
run touches the real launchctl or systemctl:

```bash
cd ~/c/agents-configs && grep -n '"\$SCRIPT"' tests/install.test.sh
```

Expected: every line either is the `run()` helper or contains `PATH="$fakebin:$PATH"`. Fix any
that doesn't.

Then add the case after the Task 11 case:

```sh
# The push timer: a LaunchAgent on macOS and a systemd user timer on Linux,
# each installed once and run with --wait.
timerhome="$work/timerhome"
mkdir -p "$timerhome"
output=$(HOME="$timerhome" INSTALL_OS=Darwin PATH="$fakebin:$PATH" "$SCRIPT")
assert_contains "session push timer: installed" "$output"
plist="$timerhome/Library/LaunchAgents/com.grillermo.claude-sessions-push.plist"
assert_contains "<string>$timerhome/.claude/hooks/claude-sessions-push.sh</string>" "$(cat "$plist")"
assert_contains "<string>--wait</string>" "$(cat "$plist")"
assert_contains "<integer>600</integer>" "$(cat "$plist")"
assert_contains "bootstrap gui/" "$(cat "$timerhome/launchctl.log")"
output=$(HOME="$timerhome" INSTALL_OS=Darwin PATH="$fakebin:$PATH" "$SCRIPT")
assert_contains "session push timer: already installed" "$output"

linuxhome="$work/linuxhome"
mkdir -p "$linuxhome"
output=$(HOME="$linuxhome" INSTALL_OS=Linux PATH="$fakebin:$PATH" "$SCRIPT")
assert_contains "session push timer: installed" "$output"
units="$linuxhome/.config/systemd/user"
assert_contains "OnUnitActiveSec=10min" "$(cat "$units/claude-sessions-push.timer")"
assert_contains "claude-sessions-push.sh --wait" "$(cat "$units/claude-sessions-push.service")"
assert_contains "--user enable --now claude-sessions-push.timer" "$(cat "$linuxhome/systemctl.log")"
output=$(HOME="$linuxhome" INSTALL_OS=Linux PATH="$fakebin:$PATH" "$SCRIPT")
assert_contains "session push timer: already installed" "$output"
```

- [ ] **Step 2: Run it and check that it fails**

Run: `cd ~/c/agents-configs && sh tests/install.test.sh`
Expected: it fails with `expected output to contain session push timer: installed`.

- [ ] **Step 3: Implement** (in `install.sh`, after `register_session_push`)

```bash
# A 10-minute timer runs the same push, for sessions that end without the hooks
# firing. launchd and systemd end a job's leftover processes when it exits, so
# the timer runs the push with --wait, in the foreground. Rewritten and
# reloaded only when the unit changed or is not loaded.
install_session_push_timer() {
  local os=${INSTALL_OS:-$(uname -s)}
  local script="$HOME/.claude/hooks/claude-sessions-push.sh"
  local state="already installed"
  local tmp
  tmp=$(mktemp -d)

  case "$os" in
    Darwin)
      local label="com.grillermo.claude-sessions-push"
      local plist="$HOME/Library/LaunchAgents/$label.plist"
      local domain
      domain="gui/$(id -u)"
      cat >"$tmp/plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>$script</string>
    <string>--wait</string>
  </array>
  <key>StartInterval</key>
  <integer>600</integer>
</dict>
</plist>
EOF
      if ! cmp -s "$tmp/plist" "$plist"; then
        mkdir -p "$(dirname "$plist")"
        mv "$tmp/plist" "$plist"
        launchctl bootout "$domain/$label" >/dev/null 2>&1 || true
        state="installed"
      fi
      if ! launchctl print "$domain/$label" >/dev/null 2>&1; then
        if launchctl bootstrap "$domain" "$plist"; then
          state="installed"
        else
          state="written but not loaded (run: launchctl bootstrap $domain $plist)"
        fi
      fi
      ;;
    Linux)
      local units="$HOME/.config/systemd/user"
      cat >"$tmp/service" <<'EOF'
[Unit]
Description=Push this machine's claude-sessions index to the macmini

[Service]
Type=oneshot
ExecStart=/bin/sh %h/.claude/hooks/claude-sessions-push.sh --wait
EOF
      cat >"$tmp/timer" <<'EOF'
[Unit]
Description=Push the claude-sessions index every 10 minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=10min

[Install]
WantedBy=timers.target
EOF
      if ! cmp -s "$tmp/service" "$units/claude-sessions-push.service" ||
        ! cmp -s "$tmp/timer" "$units/claude-sessions-push.timer"; then
        mkdir -p "$units"
        mv "$tmp/service" "$units/claude-sessions-push.service"
        mv "$tmp/timer" "$units/claude-sessions-push.timer"
        systemctl --user daemon-reload || true
        state="installed"
      fi
      if ! systemctl --user is-enabled --quiet claude-sessions-push.timer 2>/dev/null; then
        if systemctl --user enable --now claude-sessions-push.timer; then
          state="installed"
        else
          state="written but not enabled (run: systemctl --user enable --now claude-sessions-push.timer)"
        fi
      fi
      ;;
    *)
      state="skipped (no timer support on $os)"
      ;;
  esac

  rm -rf "$tmp"
  echo "session push timer: $state"
}
```

In the main sequence, add `install_session_push_timer` on the line after `register_session_push`.

- [ ] **Step 4: Run the tests and shellcheck**

```bash
cd ~/c/agents-configs && sh tests/install.test.sh && sh tests/claude-sessions-push.test.sh && shellcheck install.sh
```

Expected: `ok` and `ok`, and shellcheck reports nothing new.

- [ ] **Step 5: Commit**

```bash
cd ~/c/agents-configs && git add install.sh tests/install.test.sh && git commit -m "feat: 10-minute claude-sessions push timer (launchd, systemd)

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 13: Roll out to immunemac, macmini and the PC, and check it end to end

Nothing here is destructive. `install.sh` is idempotent, and the remote commands only read state or
run the push. If a host can't be reached, stop and tell the user. Don't retry in a loop.

- [ ] **Step 1: Wait for git-sync to carry both repos**

```bash
for h in macmini pc; do command ssh -o BatchMode=yes -o ConnectTimeout=10 $h 'cd ~/c/claude-sessions && git log -1 --oneline; cd ~/c/agents-configs && git log -1 --oneline'; done
```

Expected: both hosts show the same last commits as `git log -1 --oneline` here in each repo. If they
don't yet, wait a minute and check once more.

- [ ] **Step 2: Run install.sh on each host**

```bash
~/c/agents-configs/install.sh | grep 'session push'
command ssh -o BatchMode=yes macmini '~/c/agents-configs/install.sh' | grep 'session push'
command ssh -o BatchMode=yes pc '~/c/agents-configs/install.sh' | grep 'session push'
```

Expected on each host: `session push: registered in settings.json` and
`session push timer: installed`. If the timer says "written but not loaded/enabled", run the
command it prints on that host.

- [ ] **Step 3: Push from each host, the macmini first**

```bash
command ssh -o BatchMode=yes macmini '~/.claude/hooks/claude-sessions-push.sh --wait; tail -3 ~/.cache/claude-sessions/push.log'
~/.claude/hooks/claude-sessions-push.sh --wait; tail -3 ~/.cache/claude-sessions/push.log
command ssh -o BatchMode=yes pc '~/.claude/hooks/claude-sessions-push.sh --wait; tail -3 ~/.cache/claude-sessions/push.log'
command ssh -o BatchMode=yes macmini 'ls -la ~/.cache/claude-sessions/hosts/'
```

Expected: the logs show no error lines (build output from a first compile is fine). `hosts/` holds
`immunemac`, `macmini` and `pc` `.jsonl` and `.manifest.json` files.

- [ ] **Step 4: Check that a second push is a delta**

```bash
~/c/claude-sessions/claude-sessions --export | wc -l
```

Expected: a small number (the header plus this session's own transcript), not about 380.

- [ ] **Step 5: Check the timers**

```bash
launchctl print "gui/$(id -u)/com.grillermo.claude-sessions-push" | grep -E 'state|run interval'
command ssh -o BatchMode=yes pc 'systemctl --user list-timers claude-sessions-push.timer --no-pager'
```

Expected: the LaunchAgent is loaded with a 600s interval, and the PC lists the timer with a next
run.

- [ ] **Step 6: End-to-end check (the user, at the keyboard)**

On the PC, start a Claude session in `~/c/claude-sessions`, mention a unique phrase in the middle
of it (for example `zebra-quartz-0426`), and end it. A minute later, on immunemac, run
`claude-sessions` and type `/zebra-quartz`. The PC's session should show up with `pc` in its
header, and the title should say `remote: live`. Press enter: it should open over ssh on the PC, in
`~/c/claude-sessions`, resuming that session.

- [ ] **Step 7: Notify**

```bash
~/c/agents-configs/bin/ntfy_machines.sh "claude-sessions" "Cross-machine search is live on immunemac, macmini and pc"
```
