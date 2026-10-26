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
	// The haystack holds the text now; keeping it twice doubles the memory a
	// few gigabytes of other machines' sessions take.
	s.Text = ""
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
