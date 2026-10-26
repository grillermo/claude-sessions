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
