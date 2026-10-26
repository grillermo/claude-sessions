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
