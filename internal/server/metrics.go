package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"sitemonitor/internal/checks"
	"sitemonitor/internal/cluster"
	"sitemonitor/internal/config"
	"sitemonitor/internal/store"
)

// metrics renders the Prometheus text format by hand (no extra dependency).
func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	var b strings.Builder
	esc := func(v string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) }
	gauge := func(name, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }

	c := s.Cluster.Snapshot()
	node := fmt.Sprintf(`node="%s",node_name="%s",region="%s"`, esc(c.Node), esc(c.NodeName), esc(c.Region))
	gauge("monitor_peer_up", "1 if the peer server is HEALTHY")
	fmt.Fprintf(&b, "monitor_peer_up{%s,peer=\"%s\"} %d\n", node, esc(c.PeerID), btoi(c.PeerStatus == cluster.PeerHealthy))
	gauge("monitor_peer_heartbeat_age_seconds", "seconds since the last successful heartbeat")
	fmt.Fprintf(&b, "monitor_peer_heartbeat_age_seconds{%s} %.1f\n", node, c.HeartbeatAgeSec)
	gauge("monitor_ownership_epoch", "ownership epoch; increases on every failover/handback")
	fmt.Fprintf(&b, "monitor_ownership_epoch{%s} %d\n", node, c.Epoch)
	gauge("monitor_owns_region", "1 if this server currently monitors the region")
	for _, r := range []string{config.RegionIDC, config.RegionFC} {
		owned := 0
		for _, o := range c.OwnedRegions {
			if o == r {
				owned = 1
			}
		}
		fmt.Fprintf(&b, "monitor_owns_region{%s,target_region=\"%s\"} %d\n", node, r, owned)
	}
	gauge("monitor_config_hash_match", "1 if sites.yaml is identical on both servers")
	fmt.Fprintf(&b, "monitor_config_hash_match{%s} %d\n", node, btoi(c.ConfigMatch))
	gauge("monitor_replication_lag_seconds", "age of the oldest unreplicated item")
	fmt.Fprintf(&b, "monitor_replication_lag_seconds{%s} %.1f\n", node, c.ReplicationLag)
	gauge("monitor_witness_reachable", "1 if the witness answered the last lease request")
	fmt.Fprintf(&b, "monitor_witness_reachable{%s} %d\n", node, btoi(!c.WitnessEnabled || c.WitnessReachable))

	st := s.Sched.Stats()
	gauge("monitor_queue_depth", "jobs waiting for a worker")
	fmt.Fprintf(&b, "monitor_queue_depth{%s} %d\n", node, st.QueueDepth)
	gauge("monitor_worker_saturation", "busy workers / total workers")
	fmt.Fprintf(&b, "monitor_worker_saturation{%s} %.2f\n", node, st.Saturation)
	fmt.Fprintf(&b, "# HELP monitor_checks_completed_total checks executed\n# TYPE monitor_checks_completed_total counter\nmonitor_checks_completed_total{%s} %d\n", node, st.Completed)
	fmt.Fprintf(&b, "# HELP monitor_checks_dropped_total checks skipped because the queue was full\n# TYPE monitor_checks_dropped_total counter\nmonitor_checks_dropped_total{%s} %d\n", node, st.Dropped)

	// Site metrics: read from the database (check_state + buckets), which is
	// updated on every request success or failure. Nothing is recomputed here.
	now := time.Now()
	latest := s.Store.Latest()
	labels := func(r checks.Result) string {
		return fmt.Sprintf(`site="%s",site_region="%s",check="%s",checked_from="%s"`, esc(r.Site), esc(r.Region), esc(r.Check), esc(r.NodeRegion))
	}
	rank := map[string]int{checks.StatusOK: 0, checks.StatusWarn: 1, checks.StatusCrit: 2}
	gauge("site_check_status", "0=OK 1=WARN 2=CRIT (latest stored result)")
	for _, r := range latest {
		fmt.Fprintf(&b, "site_check_status{%s} %d\n", labels(r), rank[r.Status])
	}
	gauge("site_check_latency_ms", "latency of the latest stored result")
	for _, r := range latest {
		fmt.Fprintf(&b, "site_check_latency_ms{%s} %d\n", labels(r), r.LatencyMs)
	}

	stats := s.Store.Stats(now)
	sl := func(c store.CheckStats) string { return fmt.Sprintf(`site="%s",check="%s"`, esc(c.Site), esc(c.Check)) }
	counter := func(name, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name) }
	counter("site_check_requests_total", "requests made by checks, by outcome (ok, warn = worked but degraded, crit = failed); persisted")
	for _, c := range stats {
		for _, v := range []struct {
			res string
			n   int64
		}{{"ok", c.OK}, {"warn", c.Warn}, {"crit", c.Crit}} {
			fmt.Fprintf(&b, "site_check_requests_total{%s,result=\"%s\"} %d\n", sl(c), v.res, v.n)
		}
	}
	counter("site_check_latency_ms_sum", "sum of request latency (with site_check_requests_total gives the average)")
	for _, c := range stats {
		fmt.Fprintf(&b, "site_check_latency_ms_sum{%s} %d\n", sl(c), c.LatencySumMs)
	}
	gauge("site_check_latency_ms_max", "highest latency seen")
	for _, c := range stats {
		fmt.Fprintf(&b, "site_check_latency_ms_max{%s} %d\n", sl(c), c.MaxLatencyMs)
	}
	gauge("site_check_consecutive_failures", "failed requests in a row (0 after a success)")
	for _, c := range stats {
		fmt.Fprintf(&b, "site_check_consecutive_failures{%s} %d\n", sl(c), c.ConsecutiveFailures)
	}
	gauge("site_check_last_success_timestamp_seconds", "time of the last successful request")
	for _, c := range stats {
		if !c.LastSuccess.IsZero() {
			fmt.Fprintf(&b, "site_check_last_success_timestamp_seconds{%s} %d\n", sl(c), c.LastSuccess.Unix())
		}
	}
	gauge("site_check_last_failure_timestamp_seconds", "time of the last failed request")
	for _, c := range stats {
		if !c.LastFailure.IsZero() {
			fmt.Fprintf(&b, "site_check_last_failure_timestamp_seconds{%s} %d\n", sl(c), c.LastFailure.Unix())
		}
	}
	gauge("site_check_uptime_ratio", "share of requests that did not fail, from stored hourly (24h) and daily (30d) buckets")
	for _, c := range stats {
		for _, v := range []struct {
			win string
			p   *float64
		}{{"24h", c.Uptime24h}, {"30d", c.Uptime30d}} {
			if v.p != nil {
				fmt.Fprintf(&b, "site_check_uptime_ratio{%s,window=\"%s\"} %.4f\n", sl(c), v.win, *v.p/100)
			}
		}
	}
	gauge("site_cert_days_left", "days until the certificate expires")
	for _, c := range stats {
		if c.CertDaysLeft != nil {
			fmt.Fprintf(&b, "site_cert_days_left{site=\"%s\"} %d\n", esc(c.Site), *c.CertDaysLeft)
		}
	}
	gauge("monitor_open_incidents", "open incidents, by severity")
	open := s.Store.OpenIncidentCounts()
	for _, sev := range []string{"WARN", "CRIT"} {
		fmt.Fprintf(&b, "monitor_open_incidents{%s,severity=\"%s\"} %d\n", node, sev, open[sev])
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write([]byte(b.String()))
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}
