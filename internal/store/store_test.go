package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVerdictRoundTripAndVersion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "x", "landed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, err := s.Verdict("t1", "v1"); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	in := Verdict{TurnID: "t1", Version: "v1", Decision: true, Source: "haiku", Summary: "use blue lid", Keywords: []string{"lid", "blue"}}
	if err := s.PutVerdict(in); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Verdict("t1", "v1")
	if err != nil || !ok || !got.Decision || got.Summary != "use blue lid" || len(got.Keywords) != 2 {
		t.Errorf("got %+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := s.Verdict("t1", "v2"); ok {
		t.Errorf("a new classifier version must classify again")
	}
}

// The database and its write-ahead log hold summaries of private text: both
// must be owner-only, and so must a folder Open creates.
func TestFilesAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	path := filepath.Join(dir, "l.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PutVerdict(Verdict{TurnID: "t", Version: "v"}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600, path + "-wal": 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got&0o077 != 0 || got&want != want&0o700 {
			t.Errorf("%s mode %o, want %o", filepath.Base(p), got, want)
		}
	}
}

// A database made by an older landed has no kind column: Open adds it, and
// the old rows read back with kind "".
func TestOpenAddsKindToOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE verdict (turn_id TEXT NOT NULL, version TEXT NOT NULL, decision INTEGER NOT NULL,
		source TEXT NOT NULL, reason TEXT NOT NULL, summary TEXT NOT NULL, keywords TEXT NOT NULL, at TEXT NOT NULL,
		PRIMARY KEY (turn_id, version));
		INSERT INTO verdict VALUES ('t1','v4',1,'haiku','','old row','[]','2026-11-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, ok, err := s.Verdict("t1", "v4")
	if err != nil || !ok || v.Kind != "" || v.Summary != "old row" {
		t.Fatalf("old row: %+v %v %v", v, ok, err)
	}
	if err := s.PutVerdict(Verdict{TurnID: "t2", Version: "v5", Decision: true, Kind: "action"}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.Verdict("t2", "v5"); v.Kind != "action" {
		t.Errorf("kind not kept: %+v", v)
	}
}

// A decision once seen missing and later landed counts as rescued, once, at
// the time it was first seen landed. One that landed straight away is not.
func TestRescued(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.SeenMissing("lid", t0))
	must(st.SeenMissing("lid", t0.Add(time.Hour))) // still missing: no change
	must(st.SeenLanded("spout", t0))               // landed at once: never missing
	must(st.SeenLanded("lid", t0.Add(2*time.Hour)))
	must(st.SeenLanded("lid", t0.Add(30*time.Hour))) // again: still one rescue, at its first time
	week, all, err := st.Rescued(t0.Add(24 * time.Hour))
	if err != nil || week != 0 || all != 1 {
		t.Errorf("since the next day: %d, all %d, err %v; want 0 and 1", week, all, err)
	}
	week, _, err = st.Rescued(t0)
	if err != nil || week != 1 {
		t.Errorf("since t0: %d, err %v; want 1", week, err)
	}
}
