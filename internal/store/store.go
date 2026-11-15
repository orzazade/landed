// Package store keeps landed's state in one SQLite file (no cgo).
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Verdict is the decision call for one owner turn.
type Verdict struct {
	TurnID   string
	Version  string // classifier version; a new version classifies again
	Decision bool
	Kind     string // decisions: "action" (one-off) or "rule" (lasting); "" when the model gave none
	Source   string // "rule:<name>" or "haiku"
	Reason   string
	Summary  string
	Keywords []string
	At       time.Time
}

type Store struct{ db *sql.DB }

// DefaultPath is ~/.local/share/landed/landed.db.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "landed", "landed.db")
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Create the file owner-only before SQLite opens it: SQLite gives its
	// -wal and -shm files the database file's mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("chmod %s: %w", p, err)
		}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS verdict (
		turn_id  TEXT NOT NULL,
		version  TEXT NOT NULL,
		decision INTEGER NOT NULL,
		source   TEXT NOT NULL,
		reason   TEXT NOT NULL,
		summary  TEXT NOT NULL,
		keywords TEXT NOT NULL,
		at       TEXT NOT NULL,
		kind     TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (turn_id, version)
	);
	CREATE TABLE IF NOT EXISTS landing (
		key      TEXT PRIMARY KEY,
		evidence TEXT NOT NULL,
		why      TEXT NOT NULL,
		at       TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS seen (
		decision_id TEXT PRIMARY KEY,
		missing_at  TEXT NOT NULL,
		rescued_at  TEXT
	);
	CREATE TABLE IF NOT EXISTS judgment (
		key    TEXT PRIMARY KEY,
		record INTEGER NOT NULL,
		replaced INTEGER NOT NULL DEFAULT -1,
		why    TEXT NOT NULL,
		at     TEXT NOT NULL
	)`)
	if err == nil {
		err = addColumn(db, "verdict", "kind", `TEXT NOT NULL DEFAULT ''`)
	}
	if err == nil {
		err = addColumn(db, "judgment", "replaced", `INTEGER NOT NULL DEFAULT -1`)
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// addColumn adds a column that a database made by an older landed lacks.
func addColumn(db *sql.DB, table, col, def string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Verdict returns the cached verdict, or ok=false when the turn was never classified.
func (s *Store) Verdict(turnID, version string) (Verdict, bool, error) {
	v := Verdict{TurnID: turnID, Version: version}
	var dec int
	var kw, at string
	err := s.db.QueryRow(`SELECT decision, kind, source, reason, summary, keywords, at FROM verdict WHERE turn_id=? AND version=?`,
		turnID, version).Scan(&dec, &v.Kind, &v.Source, &v.Reason, &v.Summary, &kw, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	if err != nil {
		return v, false, err
	}
	v.Decision = dec == 1
	if err := json.Unmarshal([]byte(kw), &v.Keywords); err != nil {
		return v, false, fmt.Errorf("verdict %s: keywords: %w", turnID, err)
	}
	v.At, _ = time.Parse(time.RFC3339, at)
	return v, true, nil
}

func (s *Store) PutVerdict(v Verdict) error {
	if v.Keywords == nil {
		v.Keywords = []string{}
	}
	kw, _ := json.Marshal(v.Keywords)
	if v.At.IsZero() {
		v.At = time.Now()
	}
	dec := 0
	if v.Decision {
		dec = 1
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO verdict (turn_id, version, decision, kind, source, reason, summary, keywords, at)
		VALUES (?,?,?,?,?,?,?,?,?)`, v.TurnID, v.Version, dec, v.Kind, v.Source, v.Reason, v.Summary, string(kw), v.At.UTC().Format(time.RFC3339))
	return err
}

// Landing is a decision's final landed outcome. A decision that landed stays
// landed, so later runs reuse it instead of judging again. Evidence is the
// landing.Evidence as JSON (the store does not import landing).
type Landing struct {
	Key      string
	Evidence string
	Why      string
}

func (s *Store) Landing(key string) (Landing, bool, error) {
	l := Landing{Key: key}
	err := s.db.QueryRow(`SELECT evidence, why FROM landing WHERE key=?`, key).Scan(&l.Evidence, &l.Why)
	if errors.Is(err, sql.ErrNoRows) {
		return l, false, nil
	}
	return l, err == nil, err
}

func (s *Store) PutLanding(l Landing) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO landing (key, evidence, why, at) VALUES (?,?,?,?)`,
		l.Key, l.Evidence, l.Why, time.Now().UTC().Format(time.RFC3339))
	return err
}

// Judgment is the model's answer to "which of these candidates records the
// decision". Key covers the decision, the prompt version and the candidates,
// so a new candidate asks again.
type Judgment struct {
	Key      string
	Record   int // index of the candidate, or -1 for none
	Replaced int // index of the candidate that replaces or withdraws the decision, or -1
	Why      string
}

func (s *Store) Judgment(key string) (Judgment, bool, error) {
	j := Judgment{Key: key}
	err := s.db.QueryRow(`SELECT record, replaced, why FROM judgment WHERE key=?`, key).Scan(&j.Record, &j.Replaced, &j.Why)
	if errors.Is(err, sql.ErrNoRows) {
		return j, false, nil
	}
	return j, err == nil, err
}

func (s *Store) PutJudgment(j Judgment) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO judgment (key, record, replaced, why, at) VALUES (?,?,?,?,?)`,
		j.Key, j.Record, j.Replaced, j.Why, time.Now().UTC().Format(time.RFC3339))
	return err
}

// SeenMissing notes that a run showed decision id as missing. Only the first
// time is kept.
func (s *Store) SeenMissing(id string, at time.Time) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO seen (decision_id, missing_at) VALUES (?,?)`, id, at.UTC().Format(time.RFC3339))
	return err
}

// SeenLanded notes that a run found decision id landed. When a run showed it
// missing before, it is rescued now (only the first time counts).
func (s *Store) SeenLanded(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE seen SET rescued_at=? WHERE decision_id=? AND rescued_at IS NULL`, at.UTC().Format(time.RFC3339), id)
	return err
}

// Rescued counts the decisions rescued at or after since, and all of them.
func (s *Store) Rescued(since time.Time) (recent, all int, err error) {
	err = s.db.QueryRow(`SELECT COUNT(CASE WHEN rescued_at >= ? THEN 1 END), COUNT(rescued_at) FROM seen`,
		since.UTC().Format(time.RFC3339)).Scan(&recent, &all)
	return
}
