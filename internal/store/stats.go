package store

import (
	"database/sql"
	"strings"
	"time"
)

// CheckStats are the stored counters of one site/check. All values are read
// from check_state and the hourly/daily buckets, which are updated on every
// result; nothing here is derived from raw results.
type CheckStats struct {
	Site                string    `json:"site"`
	Check               string    `json:"check"`
	Total               int64     `json:"total"` // requests since first seen
	OK                  int64     `json:"ok"`
	Warn                int64     `json:"warn"` // request worked but degraded (slow, expiring cert, ...)
	Crit                int64     `json:"crit"` // request failed
	AvgLatencyMs        float64   `json:"avg_latency_ms"`
	MaxLatencyMs        int64     `json:"max_latency_ms"`
	LatencySumMs        int64     `json:"latency_sum_ms"`
	ConsecutiveFailures int64     `json:"consecutive_failures"`
	LastSuccess         time.Time `json:"last_success,omitzero"`
	LastFailure         time.Time `json:"last_failure,omitzero"`
	FirstSeen           time.Time `json:"first_seen,omitzero"`
	CertDaysLeft        *int64    `json:"cert_days_left,omitempty"`
	Requests24h         int64     `json:"requests_24h"`
	Failures24h         int64     `json:"failures_24h"`
	Uptime24h           *float64  `json:"uptime_24h"` // % of requests not CRIT; null = no data
	Requests30d         int64     `json:"requests_30d"`
	Uptime30d           *float64  `json:"uptime_30d"`
}

func pct(good, total int64) *float64 {
	if total == 0 {
		return nil
	}
	v := float64(good) / float64(total) * 100
	return &v
}

// Stats returns the stored statistics of every site/check.
func (s *Store) Stats(now time.Time) []CheckStats {
	rows, err := s.db.Query(`
		SELECT s.site, s.check_name, s.total, s.ok, s.warn, s.crit, s.latency_sum, s.latency_max,
		       s.consecutive_failures, s.last_success_ms, s.last_failure_ms, s.first_seen_ms, s.cert_days_left,
		       COALESCE(h.total, 0), COALESCE(h.crit, 0), COALESCE(d.total, 0), COALESCE(d.crit, 0)
		FROM check_state s
		LEFT JOIN (SELECT site, check_name, SUM(total) AS total, SUM(crit) AS crit
		           FROM check_hourly WHERE hour > ? GROUP BY site, check_name) h USING (site, check_name)
		LEFT JOIN (SELECT site, check_name, SUM(total) AS total, SUM(crit) AS crit
		           FROM check_daily WHERE day > ? GROUP BY site, check_name) d USING (site, check_name)
		ORDER BY s.site, s.check_name`,
		hourBucket(now, 24*time.Hour), dayBucket(now, 30*24*time.Hour))
	if err != nil {
		logErr("stats", err)
		return nil
	}
	defer rows.Close()
	var out []CheckStats
	for rows.Next() {
		var c CheckStats
		var lastOK, lastFail, first, cert sql.NullInt64
		var d30, d30crit int64
		if err := rows.Scan(&c.Site, &c.Check, &c.Total, &c.OK, &c.Warn, &c.Crit, &c.LatencySumMs, &c.MaxLatencyMs,
			&c.ConsecutiveFailures, &lastOK, &lastFail, &first, &cert,
			&c.Requests24h, &c.Failures24h, &d30, &d30crit); err != nil {
			logErr("stats scan", err)
			continue
		}
		if c.Total > 0 {
			c.AvgLatencyMs = float64(c.LatencySumMs) / float64(c.Total)
		}
		c.LastSuccess, c.LastFailure, c.FirstSeen = nullMs(lastOK), nullMs(lastFail), nullMs(first)
		if cert.Valid {
			v := cert.Int64
			c.CertDaysLeft = &v
		}
		c.Uptime24h = pct(c.Requests24h-c.Failures24h, c.Requests24h)
		c.Requests30d, c.Uptime30d = d30, pct(d30-d30crit, d30)
		out = append(out, c)
	}
	return out
}

// Summary is the dashboard's KPI row, aggregated by the database from the
// stored check states and buckets.
type Summary struct {
	Websites         int64    `json:"websites"`
	Healthy          int64    `json:"healthy"`  // every check OK
	Warning          int64    `json:"warning"`  // worst check WARN
	Critical         int64    `json:"critical"` // worst check CRIT
	Pending          int64    `json:"pending"`  // no result yet
	CertsExpiring    int64    `json:"certs_expiring"`
	CertWarnDays     int      `json:"cert_warn_days"`
	ChecksTotal      int64    `json:"checks_total"` // requests since first seen
	Requests24h      int64    `json:"requests_24h"`
	Failures24h      int64    `json:"failures_24h"`
	Availability24h  *float64 `json:"availability_24h"` // % of availability checks not CRIT
	OpenIncidents    int64    `json:"open_incidents"`
	CriticalIncident int64    `json:"critical_incidents"`
}

// Summary aggregates the stored state of the configured sites.
func (s *Store) Summary(sites []string, certWarnDays int, now time.Time) Summary {
	sum := Summary{CertWarnDays: certWarnDays}
	if len(sites) == 0 {
		return sum
	}
	ph := strings.TrimSuffix(strings.Repeat("(?),", len(sites)), ",")
	args := make([]any, 0, len(sites)+3)
	for _, st := range sites {
		args = append(args, st)
	}
	h24 := hourBucket(now, 24*time.Hour)
	args = append(args, certWarnDays, h24, h24, h24, h24) // in placeholder order
	q := `
		WITH cfg(site) AS (VALUES ` + ph + `),
		worst AS (
			SELECT cfg.site, MAX(CASE st.status WHEN 'CRIT' THEN 2 WHEN 'WARN' THEN 1 WHEN 'OK' THEN 0 END) AS w
			FROM cfg LEFT JOIN check_state st ON st.site = cfg.site GROUP BY cfg.site
		)
		SELECT
			(SELECT COUNT(*) FROM worst),
			(SELECT COUNT(*) FROM worst WHERE w = 0),
			(SELECT COUNT(*) FROM worst WHERE w = 1),
			(SELECT COUNT(*) FROM worst WHERE w = 2),
			(SELECT COUNT(*) FROM worst WHERE w IS NULL),
			(SELECT COUNT(*) FROM check_state WHERE check_name = 'cert' AND cert_days_left <= ? AND site IN (SELECT site FROM cfg)),
			(SELECT COALESCE(SUM(total), 0) FROM check_state WHERE site IN (SELECT site FROM cfg)),
			(SELECT COALESCE(SUM(total), 0) FROM check_hourly WHERE hour > ? AND site IN (SELECT site FROM cfg)),
			(SELECT COALESCE(SUM(crit), 0)  FROM check_hourly WHERE hour > ? AND site IN (SELECT site FROM cfg)),
			(SELECT COALESCE(SUM(total), 0) FROM check_hourly WHERE hour > ? AND check_name = 'availability' AND site IN (SELECT site FROM cfg)),
			(SELECT COALESCE(SUM(crit), 0)  FROM check_hourly WHERE hour > ? AND check_name = 'availability' AND site IN (SELECT site FROM cfg)),
			(SELECT COUNT(*) FROM incidents WHERE status = 'OPEN'),
			(SELECT COUNT(*) FROM incidents WHERE status = 'OPEN' AND severity = 'CRIT')`
	var availTotal, availCrit int64
	if err := s.db.QueryRow(q, args...).Scan(&sum.Websites, &sum.Healthy, &sum.Warning, &sum.Critical, &sum.Pending,
		&sum.CertsExpiring, &sum.ChecksTotal, &sum.Requests24h, &sum.Failures24h, &availTotal, &availCrit,
		&sum.OpenIncidents, &sum.CriticalIncident); err != nil {
		logErr("summary", err)
	}
	sum.Availability24h = pct(availTotal-availCrit, availTotal)
	return sum
}

// WorstBySite returns each site's worst stored check status (OK, WARN, CRIT).
func (s *Store) WorstBySite() map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT site, CASE MAX(CASE status WHEN 'CRIT' THEN 2 WHEN 'WARN' THEN 1 ELSE 0 END)
			WHEN 2 THEN 'CRIT' WHEN 1 THEN 'WARN' ELSE 'OK' END
		FROM check_state WHERE status IS NOT NULL GROUP BY site`)
	if err != nil {
		logErr("worst by site", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var site, st string
		if rows.Scan(&site, &st) == nil {
			out[site] = st
		}
	}
	return out
}
