package store

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"sitemonitor/internal/checks"
)

// AddResult stores a result and, in the same transaction, updates the
// check's counters, current state and hourly/daily buckets. It returns false
// (and changes nothing) if the result was already stored (replication).
func (s *Store) AddResult(r checks.Result) bool {
	stored := false
	err := s.write(func(tx *sql.Tx) error {
		details, _ := json.Marshal(r.Details)
		t := ms(r.Time)
		res, err := tx.Exec(`INSERT OR IGNORE INTO results
			(id, site, check_name, region, node, node_region, status, message, latency_ms, details, time_ms)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, r.Site, r.Check, r.Region, r.Node, r.NodeRegion, r.Status, r.Message, r.LatencyMs, string(details), t)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // duplicate
		}
		stored = true

		var ok, warn, crit int
		var lastOK, lastFail sql.NullInt64
		switch r.Status {
		case checks.StatusOK:
			ok, lastOK = 1, sql.NullInt64{Int64: t, Valid: true}
		case checks.StatusWarn:
			warn, lastOK = 1, sql.NullInt64{Int64: t, Valid: true} // the request worked, degraded
		default:
			crit, lastFail = 1, sql.NullInt64{Int64: t, Valid: true}
		}

		// 1. lifetime counters (always, even for late-arriving results)
		if _, err := tx.Exec(`INSERT INTO check_state
			(site, check_name, region, total, ok, warn, crit, latency_sum, latency_max, last_success_ms, last_failure_ms, first_seen_ms)
			VALUES (?,?,?,1,?,?,?,?,?,?,?,?)
			ON CONFLICT(site, check_name) DO UPDATE SET
				region          = excluded.region,
				total           = total + 1,
				ok              = ok + excluded.ok,
				warn            = warn + excluded.warn,
				crit            = crit + excluded.crit,
				latency_sum     = latency_sum + excluded.latency_sum,
				latency_max     = MAX(latency_max, excluded.latency_max),
				last_success_ms = MAX(COALESCE(last_success_ms, 0), COALESCE(excluded.last_success_ms, 0)),
				last_failure_ms = MAX(COALESCE(last_failure_ms, 0), COALESCE(excluded.last_failure_ms, 0)),
				first_seen_ms   = MIN(COALESCE(first_seen_ms, excluded.first_seen_ms), excluded.first_seen_ms)`,
			r.Site, r.Check, r.Region, ok, warn, crit, r.LatencyMs, r.LatencyMs, lastOK, lastFail, t); err != nil {
			return err
		}

		// 2. current state (only if this is the newest result for the check)
		var certDays sql.NullInt64
		if r.Check == "cert" {
			if v, err := strconv.Atoi(r.Details["days_left"]); err == nil {
				certDays = sql.NullInt64{Int64: int64(v), Valid: true}
			}
		}
		if _, err := tx.Exec(`UPDATE check_state SET
				status = ?, message = ?, latency_ms = ?, details = ?, node = ?, node_region = ?, result_id = ?, time_ms = ?,
				cert_days_left = COALESCE(?, cert_days_left)
			WHERE site = ? AND check_name = ? AND (time_ms IS NULL OR time_ms <= ?)`,
			r.Status, r.Message, r.LatencyMs, string(details), r.Node, r.NodeRegion, r.ID, t,
			certDays, r.Site, r.Check, t); err != nil {
			return err
		}

		// 2b. failure streak, independent of arrival order (replication can deliver late):
		//     a failure newer than the last success extends it; the newest success resets
		//     it to the failures recorded after that success.
		if crit == 1 {
			_, err = tx.Exec(`UPDATE check_state SET consecutive_failures = consecutive_failures + 1
				WHERE site = ? AND check_name = ? AND COALESCE(last_success_ms, 0) < ?`, r.Site, r.Check, t)
		} else {
			_, err = tx.Exec(`UPDATE check_state SET consecutive_failures = (
					SELECT COUNT(*) FROM results WHERE site = ?1 AND check_name = ?2 AND status = 'CRIT' AND time_ms > ?3)
				WHERE site = ?1 AND check_name = ?2 AND last_success_ms = ?3`, r.Site, r.Check, t)
		}
		if err != nil {
			return err
		}

		// dashboard chart counter (all sites and jobs together)
		if err := countResultTx(tx, t, crit == 1); err != nil {
			return err
		}

		// 3. hourly and daily buckets
		for _, b := range []struct {
			table, col string
			v          int64
		}{
			{"check_hourly", "hour", t / 3600000},
			{"check_daily", "day", t / 86400000},
		} {
			if _, err := tx.Exec(`INSERT INTO `+b.table+` (site, check_name, `+b.col+`, total, ok, warn, crit, latency_sum, latency_max)
				VALUES (?,?,?,1,?,?,?,?,?)
				ON CONFLICT(site, check_name, `+b.col+`) DO UPDATE SET
					total = total + 1, ok = ok + excluded.ok, warn = warn + excluded.warn, crit = crit + excluded.crit,
					latency_sum = latency_sum + excluded.latency_sum, latency_max = MAX(latency_max, excluded.latency_max)`,
				r.Site, r.Check, b.v, ok, warn, crit, r.LatencyMs, r.LatencyMs); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		logErr("store result", err)
		return false
	}
	return stored
}

func scanResult(sc interface{ Scan(...any) error }) (checks.Result, error) {
	var r checks.Result
	var details, region, node, nodeRegion, msg, id sql.NullString
	var latency, t sql.NullInt64
	if err := sc.Scan(&id, &r.Site, &r.Check, &region, &node, &nodeRegion, &r.Status, &msg, &latency, &details, &t); err != nil {
		return r, err
	}
	r.ID, r.Region, r.Node, r.NodeRegion, r.Message = id.String, region.String, node.String, nodeRegion.String, msg.String
	r.LatencyMs, r.Time = latency.Int64, fromMs(t.Int64)
	r.Details = map[string]string{}
	_ = json.Unmarshal([]byte(details.String), &r.Details)
	return r, nil
}

// Latest returns the stored current result of every site/check.
func (s *Store) Latest() []checks.Result {
	rows, err := s.db.Query(`SELECT result_id, site, check_name, region, node, node_region, status, message, latency_ms, details, time_ms
		FROM check_state WHERE status IS NOT NULL ORDER BY site, check_name`)
	if err != nil {
		logErr("latest", err)
		return nil
	}
	defer rows.Close()
	var out []checks.Result
	for rows.Next() {
		if r, err := scanResult(rows); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// History returns up to n most recent raw results for a site/check, oldest first.
func (s *Store) History(site, check string, n int) []checks.Result {
	rows, err := s.db.Query(`SELECT * FROM (
			SELECT id, site, check_name, region, node, node_region, status, message, latency_ms, details, time_ms
			FROM results WHERE site = ? AND check_name = ? ORDER BY time_ms DESC LIMIT ?
		) ORDER BY time_ms ASC`, site, check, n)
	if err != nil {
		logErr("history", err)
		return nil
	}
	defer rows.Close()
	out := []checks.Result{}
	for rows.Next() {
		if r, err := scanResult(rows); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// windowStart returns the first hour / day bucket of a window ending now.
func hourBucket(now time.Time, d time.Duration) int64 { return ms(now.Add(-d)) / 3600000 }
func dayBucket(now time.Time, d time.Duration) int64  { return ms(now.Add(-d)) / 86400000 }

// LastResultTime returns when the newest stored result of a site/check was
// taken (zero if none). The job scheduler uses it so a restart or failover
// does not run a job again before its interval has passed.
func (s *Store) LastResultTime(site, check string) time.Time {
	var t sql.NullInt64
	s.db.QueryRow(`SELECT time_ms FROM check_state WHERE site = ? AND check_name = ?`, site, check).Scan(&t)
	return nullMs(t)
}

// RecentResults returns the newest n raw results of one check type across all
// sites (e.g. every job run), newest first.
func (s *Store) RecentResults(check string, n int) []checks.Result {
	rows, err := s.db.Query(`SELECT id, site, check_name, region, node, node_region, status, message, latency_ms, details, time_ms
		FROM results WHERE check_name = ? ORDER BY time_ms DESC LIMIT ?`, check, n)
	if err != nil {
		logErr("recent results", err)
		return nil
	}
	defer rows.Close()
	out := []checks.Result{}
	for rows.Next() {
		if r, err := scanResult(rows); err == nil {
			out = append(out, r)
		}
	}
	return out
}
