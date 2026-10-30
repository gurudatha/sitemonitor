package store

import (
	"database/sql"
	"time"
)

// Activity counters for the dashboard charts: events per hour by severity,
// and requests / failed requests per hour across all sites and jobs. They are
// global (not per site), so a year of them is ~26k small rows. They are
// incremented in the same transaction that stores the event or result.
const activitySchema = `
CREATE TABLE IF NOT EXISTS event_hourly (
	hour     INTEGER NOT NULL, -- unix hour (UTC)
	severity TEXT    NOT NULL,
	count    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (hour, severity)
);
CREATE TABLE IF NOT EXISTS result_hourly (
	hour  INTEGER PRIMARY KEY, -- unix hour (UTC)
	total INTEGER NOT NULL DEFAULT 0,
	crit  INTEGER NOT NULL DEFAULT 0
);`

func (s *Store) ensureActivity() error {
	if _, err := s.db.Exec(activitySchema); err != nil {
		return err
	}
	var done string
	if s.db.QueryRow(`SELECT v FROM meta WHERE k = 'activity_backfill'`).Scan(&done) == nil {
		return nil
	}
	// One-time fill from what is already stored: retained events, hourly
	// buckets (last days) and daily buckets (older days, placed at 00:00 UTC).
	return s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO event_hourly (hour, severity, count)
			SELECT time_ms / 3600000, COALESCE(severity, 'OK'), COUNT(*) FROM events GROUP BY 1, 2`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO result_hourly (hour, total, crit)
			SELECT hour, SUM(total), SUM(crit) FROM check_hourly GROUP BY hour`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO result_hourly (hour, total, crit)
			SELECT day * 24, SUM(total), SUM(crit) FROM check_daily
			WHERE day * 24 < COALESCE((SELECT MIN(hour) FROM check_hourly), 1 << 40) GROUP BY day`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO meta (k, v) VALUES ('activity_backfill', 'done')`)
		return err
	})
}

func countEventTx(tx *sql.Tx, e Event) error {
	sev := e.Severity
	if sev == "" {
		sev = "OK"
	}
	_, err := tx.Exec(`INSERT INTO event_hourly (hour, severity, count) VALUES (?, ?, 1)
		ON CONFLICT(hour, severity) DO UPDATE SET count = count + 1`, ms(e.Time)/3600000, sev)
	return err
}

func countResultTx(tx *sql.Tx, t int64, failed bool) error {
	crit := 0
	if failed {
		crit = 1
	}
	_, err := tx.Exec(`INSERT INTO result_hourly (hour, total, crit) VALUES (?, 1, ?)
		ON CONFLICT(hour) DO UPDATE SET total = total + 1, crit = crit + excluded.crit`, t/3600000, crit)
	return err
}

// ActivityBucket is one day or one hour of the activity charts.
type ActivityBucket struct {
	Start    time.Time        `json:"start"` // local start of the bucket
	Label    string           `json:"label"` // "2026-10-30" or "14:00"
	Events   map[string]int64 `json:"events"`
	Requests int64            `json:"requests"`
	Failures int64            `json:"failures"`
}

// Activity groups the stored hourly counters into buckets of the given size
// (24h for days, 1h for hours) in the viewer's time zone, from start (local
// midnight or hour) for n buckets. Counters are per UTC hour; each UTC hour
// belongs to the one bucket that contains its start, so in half-hour time
// zones (e.g. UTC+05:30) days and hours always add up to the same totals.
func (s *Store) Activity(start time.Time, step time.Duration, n int) []ActivityBucket {
	out := make([]ActivityBucket, n)
	for i := range out {
		t := start.Add(time.Duration(i) * step)
		label := t.Format("2006-01-02")
		if step < 24*time.Hour {
			label = t.Format("15:04")
		}
		out[i] = ActivityBucket{Start: t, Label: label, Events: map[string]int64{"CRIT": 0, "WARN": 0, "OK": 0}}
	}
	ceilHour := func(t time.Time) int64 { return (t.Unix() + 3599) / 3600 }
	from, to := ceilHour(start), ceilHour(start.Add(time.Duration(n)*step))
	index := func(hour int64) int {
		i := int(time.Unix(hour*3600, 0).Sub(start) / step)
		if i < 0 || i >= n {
			return -1
		}
		return i
	}
	if rows, err := s.db.Query(`SELECT hour, severity, count FROM event_hourly WHERE hour >= ? AND hour < ?`, from, to); err == nil {
		for rows.Next() {
			var h, c int64
			var sev string
			if rows.Scan(&h, &sev, &c) == nil {
				if i := index(h); i >= 0 {
					out[i].Events[sev] += c
				}
			}
		}
		rows.Close()
	} else {
		logErr("activity events", err)
	}
	if rows, err := s.db.Query(`SELECT hour, total, crit FROM result_hourly WHERE hour >= ? AND hour < ?`, from, to); err == nil {
		for rows.Next() {
			var h, t, c int64
			if rows.Scan(&h, &t, &c) == nil {
				if i := index(h); i >= 0 {
					out[i].Requests += t
					out[i].Failures += c
				}
			}
		}
		rows.Close()
	} else {
		logErr("activity results", err)
	}
	return out
}
