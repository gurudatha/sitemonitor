// Package store persists check results, per-check statistics, events and
// incidents in an embedded SQLite database (one file per server, in data_dir).
//
// Statistics are maintained incrementally: every result (success or failure)
// updates the check's counters, its current state and its hourly/daily
// buckets in the same transaction. The dashboard and /metrics only read these
// stored values; nothing is recomputed from raw history on a request.
package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver (no CGO), registers "sqlite"
)

// Options bound what is kept in the database (from node.yaml limits).
type Options struct {
	HistoryPerCheck int           // raw results kept per site/check
	MaxEvents       int           // events kept
	HourlyRetention time.Duration // hourly statistics kept (24h uptime needs >= 25h)
	DailyRetention  time.Duration // daily statistics kept (30d uptime, yearly reports)
}

// Event is something a human should see: an alert, a recovery, a failover.
type Event struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Node     string    `json:"node"`
	Kind     string    `json:"kind"`     // ALERT | RECOVERY | FAILOVER | HANDBACK | PEER | CONFIG | INFO | AUTH
	Severity string    `json:"severity"` // OK | WARN | CRIT
	Site     string    `json:"site,omitempty"`
	Check    string    `json:"check,omitempty"`
	Message  string    `json:"message"`
	// Key identifies the problem this event is about (e.g. "site:shop|cert").
	// Events with the same key are grouped into one Incident: duplicates of an
	// open problem are dropped, clears and re-appearances are recorded.
	Key string `json:"key,omitempty"`
}

type Store struct {
	db      *sql.DB
	opt     Options
	path    string
	writeMu sync.Mutex // SQLite has one writer; serialise writes instead of retrying on SQLITE_BUSY
}

const schema = `
CREATE TABLE IF NOT EXISTS results (
	id          TEXT PRIMARY KEY,
	site        TEXT NOT NULL,
	check_name  TEXT NOT NULL,
	region      TEXT,
	node        TEXT,
	node_region TEXT,
	status      TEXT NOT NULL,
	message     TEXT,
	latency_ms  INTEGER,
	details     TEXT,
	time_ms     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS results_site_check_time ON results(site, check_name, time_ms);

-- One row per site/check: current state + lifetime counters, updated on every result.
CREATE TABLE IF NOT EXISTS check_state (
	site                 TEXT NOT NULL,
	check_name           TEXT NOT NULL,
	region               TEXT,
	status               TEXT,
	message              TEXT,
	latency_ms           INTEGER,
	details              TEXT,
	node                 TEXT,
	node_region          TEXT,
	result_id            TEXT,
	time_ms              INTEGER,
	total                INTEGER NOT NULL DEFAULT 0,
	ok                   INTEGER NOT NULL DEFAULT 0,
	warn                 INTEGER NOT NULL DEFAULT 0,
	crit                 INTEGER NOT NULL DEFAULT 0,
	latency_sum          INTEGER NOT NULL DEFAULT 0,
	latency_max          INTEGER NOT NULL DEFAULT 0,
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	last_success_ms      INTEGER,
	last_failure_ms      INTEGER,
	first_seen_ms        INTEGER,
	cert_days_left       INTEGER,
	PRIMARY KEY (site, check_name)
);

-- Pre-aggregated buckets for uptime windows (24h from hourly, 30d/yearly from daily).
CREATE TABLE IF NOT EXISTS check_hourly (
	site        TEXT NOT NULL,
	check_name  TEXT NOT NULL,
	hour        INTEGER NOT NULL, -- unix hour (time_ms / 3600000)
	total       INTEGER NOT NULL DEFAULT 0,
	ok          INTEGER NOT NULL DEFAULT 0,
	warn        INTEGER NOT NULL DEFAULT 0,
	crit        INTEGER NOT NULL DEFAULT 0,
	latency_sum INTEGER NOT NULL DEFAULT 0,
	latency_max INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (site, check_name, hour)
);
CREATE TABLE IF NOT EXISTS check_daily (
	site        TEXT NOT NULL,
	check_name  TEXT NOT NULL,
	day         INTEGER NOT NULL, -- unix day (time_ms / 86400000), UTC
	total       INTEGER NOT NULL DEFAULT 0,
	ok          INTEGER NOT NULL DEFAULT 0,
	warn        INTEGER NOT NULL DEFAULT 0,
	crit        INTEGER NOT NULL DEFAULT 0,
	latency_sum INTEGER NOT NULL DEFAULT 0,
	latency_max INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (site, check_name, day)
);

CREATE TABLE IF NOT EXISTS events (
	id         TEXT PRIMARY KEY,
	time_ms    INTEGER NOT NULL,
	node       TEXT,
	kind       TEXT,
	severity   TEXT,
	site       TEXT,
	check_name TEXT,
	key        TEXT,
	message    TEXT
);
CREATE INDEX IF NOT EXISTS events_time ON events(time_ms);

CREATE TABLE IF NOT EXISTS incidents (
	key           TEXT PRIMARY KEY,
	status        TEXT NOT NULL,
	severity      TEXT NOT NULL,
	first_seen_ms INTEGER NOT NULL,
	cleared_at_ms INTEGER,
	data          TEXT NOT NULL -- JSON of Incident (history, seen_by, counters)
);

CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT);
`

// Open opens (or creates) <dataDir>/sitemonitor.db.
func Open(dataDir string, opt Options) (*Store, error) {
	if opt.HistoryPerCheck <= 0 {
		opt.HistoryPerCheck = 200
	}
	if opt.MaxEvents <= 0 {
		opt.MaxEvents = 500
	}
	if opt.HourlyRetention < 25*time.Hour {
		opt.HourlyRetention = 7 * 24 * time.Hour
	}
	if opt.DailyRetention < 31*24*time.Hour {
		opt.DailyRetention = 400 * 24 * time.Hour
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "sitemonitor.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=temp_store(MEMORY)" // read-only root fs: no temp files
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8) // WAL: many readers, one writer (writeMu)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("database schema: %w", err)
	}
	s := &Store{db: db, opt: opt, path: path}
	if err := s.ensureTokens(); err != nil {
		db.Close()
		return nil, fmt.Errorf("database schema: %w", err)
	}
	if _, err := db.Exec(activitySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("database schema: %w", err)
	}
	if err := s.importLegacy(dataDir); err != nil {
		db.Close()
		return nil, fmt.Errorf("import results.json: %w", err)
	}
	if err := s.ensureActivity(); err != nil { // one-time fill of the chart counters
		db.Close()
		return nil, fmt.Errorf("activity counters: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Path is the database file.
func (s *Store) Path() string { return s.path }

func logErr(op string, err error) { slog.Error("database", "op", op, "err", err) }

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }
func nullMs(v sql.NullInt64) time.Time {
	if !v.Valid || v.Int64 == 0 {
		return time.Time{}
	}
	return fromMs(v.Int64)
}

// write runs fn in a write transaction.
func (s *Store) write(fn func(tx *sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Maintain enforces retention: raw results per check, stats buckets, events
// and cleared incidents. Run it periodically (limits.db_maintenance_interval).
func (s *Store) Maintain(now time.Time) error {
	return s.write(func(tx *sql.Tx) error {
		// keep the newest HistoryPerCheck raw results of every site/check
		if _, err := tx.Exec(`
			DELETE FROM results WHERE id IN (
				SELECT id FROM (
					SELECT id, ROW_NUMBER() OVER (PARTITION BY site, check_name ORDER BY time_ms DESC) AS rn FROM results
				) WHERE rn > ?)`, s.opt.HistoryPerCheck); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM check_hourly WHERE hour < ?`, hourBucket(now, s.opt.HourlyRetention)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM check_daily WHERE day < ?`, dayBucket(now, s.opt.DailyRetention)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY time_ms DESC LIMIT ?)`, s.opt.MaxEvents); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM event_hourly WHERE hour < ?`, ms(now.Add(-s.opt.DailyRetention))/3600000); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM result_hourly WHERE hour < ?`, ms(now.Add(-s.opt.DailyRetention))/3600000); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM incidents WHERE key IN (
			SELECT key FROM incidents WHERE status = 'CLEARED' ORDER BY cleared_at_ms ASC
			LIMIT MAX(0, (SELECT COUNT(*) FROM incidents) - ?))`, maxIncidents)
		return err
	})
}

// DB exposes the database to packages that keep their own tables (jobs).
func (s *Store) DB() *sql.DB { return s.db }

// Write runs fn in a serialised write transaction.
func (s *Store) Write(fn func(tx *sql.Tx) error) error { return s.write(fn) }
