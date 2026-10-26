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
// same entries always hash the same, on either machine. It covers only each
// session's mtime and size, not Dropped: another host's push can drop this
// host's old sessions on the index, and that must not void this host's base.
func (m manifest) hash() string {
	type version struct {
		MTime time.Time `json:"mtime"`
		Bytes int64     `json:"bytes"`
	}
	versions := make(map[string]version, len(m))
	for id, e := range m {
		versions[id] = version{e.MTime, e.Bytes}
	}
	data, _ := json.Marshal(versions)
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
