package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sitemonitor/internal/checks"
)

var rseq int

func result(site, check, status string, at time.Time, latency int64) checks.Result {
	rseq++
	return checks.Result{ID: fmt.Sprintf("r-%d", rseq), Site: site, Region: "FC", Check: check, Node: "fc", NodeRegion: "FC",
		Status: status, Message: status, LatencyMs: latency, Time: at, Details: map[string]string{"days_left": "12"}}
}

func TestCountersUpdatedOnEverySuccessAndFailure(t *testing.T) {
	s := open(t, t.TempDir())
	now := time.Now().UTC()

	for i, st := range []string{"OK", "OK", "CRIT", "CRIT", "WARN", "OK", "CRIT"} {
		if !s.AddResult(result("shop", "availability", st, now.Add(time.Duration(i-10)*time.Minute), int64(100*(i+1)))) {
			t.Fatal("new result must be stored")
		}
	}
	dup := result("shop", "availability", "CRIT", now, 1)
	s.AddResult(dup)
	if s.AddResult(dup) {
		t.Fatal("replicated duplicate must not be counted twice")
	}
	// a late, older result: counted, but must not replace the current state
	s.AddResult(result("shop", "availability", "OK", now.Add(-time.Hour), 50))

	stats := s.Stats(now)
	if len(stats) != 1 {
		t.Fatalf("want 1 check, got %d", len(stats))
	}
	c := stats[0]
	if c.Total != 9 || c.OK != 4 || c.Warn != 1 || c.Crit != 4 {
		t.Fatalf("counters: total=%d ok=%d warn=%d crit=%d", c.Total, c.OK, c.Warn, c.Crit)
	}
	if c.ConsecutiveFailures != 2 {
		t.Fatalf("consecutive failures = %d, want 2 (CRIT at -4m, then CRIT now; late OK ignored)", c.ConsecutiveFailures)
	}

	// same results delivered in a different order (replica) give the same streak
	s2 := open(t, t.TempDir())
	base := now.Add(-time.Hour)
	order := []struct {
		st string
		m  int
	}{{"CRIT", 5}, {"OK", 1}, {"CRIT", 3}, {"CRIT", 4}, {"OK", 2}, {"CRIT", 6}}
	for _, o := range order { // newest success is at +2m; failures after it: +3,+4,+5,+6 = 4
		s2.AddResult(result("x", "availability", o.st, base.Add(time.Duration(o.m)*time.Minute), 1))
	}
	if got := s2.Stats(now)[0].ConsecutiveFailures; got != 4 {
		t.Fatalf("out-of-order streak = %d, want 4", got)
	}
	if c.MaxLatencyMs != 700 || c.LatencySumMs != 100+200+300+400+500+600+700+1+50 {
		t.Fatalf("latency: max=%d sum=%d", c.MaxLatencyMs, c.LatencySumMs)
	}
	if c.LastFailure.Unix() != now.Unix() || c.LastSuccess.Unix() != now.Add(-5*time.Minute).Unix() {
		t.Fatalf("last success %v / failure %v", c.LastSuccess, c.LastFailure)
	}
	if c.Requests24h != 9 || c.Failures24h != 4 || c.Uptime24h == nil || *c.Uptime24h < 55.5 || *c.Uptime24h > 55.6 {
		t.Fatalf("24h window: %d requests, %d failures, uptime %v", c.Requests24h, c.Failures24h, c.Uptime24h)
	}
	latest := s.Latest()
	if len(latest) != 1 || latest[0].Status != "CRIT" || latest[0].ID != dup.ID {
		t.Fatalf("current state must be the newest result, got %+v", latest)
	}
}

func TestSummaryAndCertDaysFromStoredState(t *testing.T) {
	s := open(t, t.TempDir())
	now := time.Now().UTC()
	s.AddResult(result("a", "availability", "OK", now, 10))
	s.AddResult(result("b", "availability", "OK", now, 10))
	s.AddResult(result("b", "cert", "WARN", now, 10)) // days_left 12
	s.AddResult(result("c", "availability", "CRIT", now, 10))
	s.AddResult(result("removed", "availability", "CRIT", now, 10)) // not in config: ignored

	sum := s.Summary([]string{"a", "b", "c", "d"}, 30, now)
	if sum.Websites != 4 || sum.Healthy != 1 || sum.Warning != 1 || sum.Critical != 1 || sum.Pending != 1 {
		t.Fatalf("summary %+v", sum)
	}
	if sum.CertsExpiring != 1 || sum.Requests24h != 4 || sum.Failures24h != 1 {
		t.Fatalf("summary %+v", sum)
	}
	if sum.Availability24h == nil || *sum.Availability24h < 66.6 || *sum.Availability24h > 66.7 {
		t.Fatalf("availability %v", sum.Availability24h)
	}
}

func TestMaintainRetention(t *testing.T) {
	s := open(t, t.TempDir())
	now := time.Now().UTC()
	for i := 0; i < 15; i++ { // HistoryPerCheck = 10
		s.AddResult(result("a", "availability", "OK", now.Add(-time.Duration(i)*time.Minute), 1))
	}
	old := now.Add(-500 * 24 * time.Hour) // beyond daily (400d) and hourly (7d) retention
	s.AddResult(result("a", "availability", "OK", old, 1))
	if err := s.Maintain(now); err != nil {
		t.Fatal(err)
	}
	if n := len(s.History("a", "availability", 100)); n != 10 {
		t.Fatalf("raw results kept = %d, want 10", n)
	}
	var hourly, daily int
	s.db.QueryRow(`SELECT COUNT(*) FROM check_hourly WHERE hour < ?`, ms(now.Add(-8*24*time.Hour))/3600000).Scan(&hourly)
	s.db.QueryRow(`SELECT COUNT(*) FROM check_daily WHERE day < ?`, ms(now.Add(-401*24*time.Hour))/86400000).Scan(&daily)
	if hourly != 0 || daily != 0 {
		t.Fatalf("old buckets not pruned: hourly=%d daily=%d", hourly, daily)
	}
	if c := s.Stats(now)[0]; c.Total != 16 {
		t.Fatalf("lifetime counters must survive pruning, total=%d", c.Total)
	}
}

func TestImportLegacySnapshot(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	snap := legacySnapshot{
		History: map[string][]checks.Result{"a|availability": {result("a", "availability", "CRIT", now, 5)}},
		Events:  []Event{{ID: "e1", Time: now, Kind: "INFO", Severity: "OK", Message: "old event"}},
	}
	b, _ := json.Marshal(snap)
	os.WriteFile(filepath.Join(dir, "results.json"), b, 0o600)

	s := open(t, dir)
	if c := s.Stats(now); len(c) != 1 || c[0].Crit != 1 {
		t.Fatalf("imported results must build counters: %+v", c)
	}
	if len(s.Events(10)) != 1 {
		t.Fatal("events not imported")
	}
	if _, err := os.Stat(filepath.Join(dir, "results.json.imported")); err != nil {
		t.Fatal("results.json must be renamed after import")
	}
}
