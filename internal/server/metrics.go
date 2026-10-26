package server

import (
	"fmt"
	"net/http"
	"strings"

	"sitemonitor/internal/checks"
	"sitemonitor/internal/cluster"
	"sitemonitor/internal/config"
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

	latest := s.Store.Latest()
	labels := func(r checks.Result) string {
		return fmt.Sprintf(`site="%s",site_region="%s",check="%s",checked_from="%s"`, esc(r.Site), esc(r.Region), esc(r.Check), esc(r.NodeRegion))
	}
	rank := map[string]int{checks.StatusOK: 0, checks.StatusWarn: 1, checks.StatusCrit: 2}
	gauge("site_check_status", "0=OK 1=WARN 2=CRIT")
	for _, r := range latest {
		fmt.Fprintf(&b, "site_check_status{%s} %d\n", labels(r), rank[r.Status])
	}
	gauge("site_check_latency_ms", "latency of the latest check")
	for _, r := range latest {
		fmt.Fprintf(&b, "site_check_latency_ms{%s} %d\n", labels(r), r.LatencyMs)
	}
	gauge("site_cert_days_left", "days until the certificate expires")
	for _, r := range latest {
		if d, ok := r.Details["days_left"]; ok && r.Check == config.CheckCert {
			fmt.Fprintf(&b, "site_cert_days_left{site=\"%s\"} %s\n", esc(r.Site), d)
		}
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
