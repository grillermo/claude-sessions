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
