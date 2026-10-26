// Package cluster keeps the IDC and FC servers in sync: heartbeats, failover
// (takeover of the peer's region), handback, witness leases and replication.
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"sitemonitor/internal/alert"
	"sitemonitor/internal/checks"
	"sitemonitor/internal/config"
	"sitemonitor/internal/store"
)

// Peer status.
const (
	PeerUnknown = "UNKNOWN"
	PeerHealthy = "HEALTHY"
	PeerSuspect = "SUSPECT"
	PeerDown    = "DOWN"
)

// Modes.
const (
	ModeNormal   = "NORMAL"   // owns its home region only
	ModeTakeover = "TAKEOVER" // owns both regions (peer is down)
	ModeStandby  = "STANDBY"  // owns nothing (waiting for handback after a restart)
)

// Heartbeat is exchanged in both directions every heartbeat interval.
type Heartbeat struct {
	NodeID             string    `json:"node_id"`
	NodeName           string    `json:"node_name"`
	Region             string    `json:"region"`
	Epoch              int64     `json:"epoch"`
	Mode               string    `json:"mode"`
	OwnedRegions       []string  `json:"owned_regions"`
	ConfigVersion      string    `json:"config_version"`
	ConfigHash         string    `json:"config_hash"`
	ConfigModTime      time.Time `json:"config_mod_time"`
	WorkerSaturation   float64   `json:"worker_saturation"`
	LastCheckCompleted time.Time `json:"last_check_completed"`
	SelfHealthy        bool      `json:"self_healthy"`
	Time               time.Time `json:"time"`
}

// LocalStatus is provided by the scheduler.
type LocalStatus struct {
	ConfigVersion, ConfigHash string
	ConfigModTime             time.Time
	Saturation                float64
	LastCheck                 time.Time
	Healthy                   bool
}

// ReplicationBatch carries results and events to the peer.
type ReplicationBatch struct {
	Results []checks.Result `json:"results"`
	Events  []store.Event   `json:"events"`
}

type ownershipFile struct {
	Node              string    `yaml:"node"`
	NodeName          string    `yaml:"node_name"`
	Peer              string    `yaml:"peer"`
	PeerName          string    `yaml:"peer_name"`
	Region            string    `yaml:"region"`
	Epoch             int64     `yaml:"epoch"`
	Mode              string    `yaml:"mode"`
	OwnedRegions      []string  `yaml:"owned_regions"`
	PeerStatus        string    `yaml:"peer_status"`
	LastPeerHeartbeat time.Time `yaml:"last_peer_heartbeat"`
	UpdatedAt         time.Time `yaml:"updated_at"`
}

// Snapshot is what the dashboard shows about the cluster.
type Snapshot struct {
	Node             string   `json:"node"`
	NodeName         string   `json:"node_name"`
	Region           string   `json:"region"`
	Epoch            int64    `json:"epoch"`
	Mode             string   `json:"mode"`
	OwnedRegions     []string `json:"owned_regions"`
	PeerID           string   `json:"peer_id"`
	PeerName         string   `json:"peer_name"`
	PeerRegion       string   `json:"peer_region"`
	PeerStatus       string   `json:"peer_status"`
	PeerMode         string   `json:"peer_mode"`
	PeerOwned        []string `json:"peer_owned_regions"`
	PeerEpoch        int64    `json:"peer_epoch"`
	HeartbeatAgeSec  float64  `json:"heartbeat_age_seconds"`
	MissedBeats      int      `json:"missed_beats"`
	LastError        string   `json:"last_error,omitempty"`
	ConfigMatch      bool     `json:"config_match"`
	PeerConfigVer    string   `json:"peer_config_version"`
	WitnessEnabled   bool     `json:"witness_enabled"`
	WitnessReachable bool     `json:"witness_reachable"`
	ReplicationQueue int      `json:"replication_queue"`
	ReplicationLag   float64  `json:"replication_lag_seconds"`
	Blocked          string   `json:"takeover_blocked,omitempty"`
	ConfigBehind     bool     `json:"config_behind"`
}

type Manager struct {
	cfg    *config.Node
	client *http.Client
	local  func() LocalStatus
	emit   func(store.Event)

	mu               sync.Mutex
	epoch            int64
	owned            map[string]bool
	decided          bool
	peerStatus       string
	missed           int
	lastBeat         time.Time
	peerHealthySince time.Time
	peer             Heartbeat
	lastErr          string
	configMatch      bool
	witnessOK        bool
	blocked          string
	mismatchBeats    int
	pulling          bool

	// OnConfigBehind is called when the peer has a newer sites.yaml for
	// several heartbeats (config_api.auto_pull_newer).
	OnConfigBehind func()

	repMu       sync.Mutex
	repResults  []checks.Result
	repEvents   []store.Event
	lastPushOK  time.Time
	lastPushTry time.Time
}

func New(cfg *config.Node, client *http.Client, local func() LocalStatus, emit func(store.Event)) *Manager {
	m := &Manager{cfg: cfg, client: client, local: local, emit: emit,
		owned: map[string]bool{}, peerStatus: PeerUnknown, configMatch: true, witnessOK: true}
	m.restore()
	return m
}

func (m *Manager) home() string       { return m.cfg.Node.Region }
func (m *Manager) peerRegion() string { return m.cfg.Peer.Region }
func (m *Manager) witness() bool      { return m.cfg.Witness.URL != "" }

// OwnedRegions is polled by the scheduler every second.
func (m *Manager) OwnedRegions() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool, len(m.owned))
	for r, v := range m.owned {
		if v {
			out[r] = true
		}
	}
	return out
}

func (m *Manager) PeerHealthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peerStatus == PeerHealthy
}

func (m *Manager) ownedList() []string {
	var l []string
	for r, v := range m.owned {
		if v {
			l = append(l, r)
		}
	}
	sort.Strings(l)
	return l
}

func (m *Manager) modeLocked() string {
	switch {
	case m.owned[m.home()] && m.owned[m.peerRegion()]:
		return ModeTakeover
	case m.owned[m.home()]:
		return ModeNormal
	case m.owned[m.peerRegion()]:
		return ModeTakeover
	default:
		return ModeStandby
	}
}

// LocalHeartbeat is what we send to (and answer) the peer.
func (m *Manager) LocalHeartbeat() Heartbeat {
	ls := m.local()
	m.mu.Lock()
	defer m.mu.Unlock()
	return Heartbeat{
		NodeID: m.cfg.Node.ID, NodeName: m.cfg.Node.Name, Region: m.home(), Epoch: m.epoch, Mode: m.modeLocked(),
		OwnedRegions: m.ownedList(), ConfigVersion: ls.ConfigVersion, ConfigHash: ls.ConfigHash, ConfigModTime: ls.ConfigModTime,
		WorkerSaturation: ls.Saturation, LastCheckCompleted: ls.LastCheck, SelfHealthy: ls.Healthy,
		Time: time.Now().UTC(),
	}
}

// ObserveInbound records a heartbeat the peer sent us (only the epoch is used;
// liveness is decided by our own outbound heartbeats).
func (m *Manager) ObserveInbound(hb Heartbeat) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if hb.Epoch > m.epoch {
		m.epoch = hb.Epoch
	}
}

func (m *Manager) Run(ctx context.Context) {
	go m.replicationLoop(ctx)
	t := time.NewTicker(m.cfg.Peer.HeartbeatInterval)
	defer t.Stop()
	m.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			m.releaseOnShutdown()
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

func (m *Manager) event(kind, sev, msg string) {
	m.emit(alert.NewEvent(m.cfg.Node.ID, kind, sev, "", "", msg))
}

func (m *Manager) tick(ctx context.Context) {
	hb, err := m.sendHeartbeat(ctx)
	ls := m.local()
	now := time.Now()

	// ---- 1. peer liveness ----
	m.mu.Lock()
	prev := m.peerStatus
	if err == nil && hb.SelfHealthy {
		m.missed = 0
		m.lastBeat = now
		m.peer = hb
		m.lastErr = ""
		if hb.Epoch > m.epoch {
			m.epoch = hb.Epoch
		}
	} else {
		m.missed++
		if err != nil {
			m.lastErr = err.Error()
		} else {
			m.lastErr = "peer reports itself unhealthy (not completing checks)"
		}
	}
	switch {
	case m.missed == 0:
		m.peerStatus = PeerHealthy
	case m.missed >= m.cfg.Peer.DownAfter:
		m.peerStatus = PeerDown
	case m.missed >= m.cfg.Peer.SuspectAfter:
		m.peerStatus = PeerSuspect
	case prev == PeerUnknown:
		m.peerStatus = PeerUnknown
	}
	status := m.peerStatus
	if status == PeerHealthy && prev != PeerHealthy {
		m.peerHealthySince = now
	}
	match := err != nil || hb.ConfigHash == ls.ConfigHash
	configChanged := match != m.configMatch
	m.configMatch = match
	if match {
		m.mismatchBeats = 0
	} else {
		m.mismatchBeats++
	}
	pull := !match && m.mismatchBeats >= 3 && !m.pulling && m.OnConfigBehind != nil &&
		hb.ConfigModTime.Truncate(time.Millisecond).After(ls.ConfigModTime.Truncate(time.Millisecond))
	if pull {
		m.pulling = true
	}
	peerClaimsHome := status == PeerHealthy && slices.Contains(m.peer.OwnedRegions, m.home())
	takenOver := m.owned[m.peerRegion()]
	healthyFor := now.Sub(m.peerHealthySince)
	lastErr := m.lastErr
	m.mu.Unlock()

	if status != prev && !(prev == PeerUnknown && status == PeerHealthy) {
		sev := map[string]string{PeerHealthy: "OK", PeerSuspect: "WARN", PeerDown: "CRIT"}[status]
		msg := fmt.Sprintf("peer %s (%s) is %s", m.cfg.Peer.ID, m.peerRegion(), status)
		if lastErr != "" && status != PeerHealthy {
			msg += ": " + lastErr
		}
		m.event("PEER", sev, msg)
	}
	if configChanged {
		if match {
			m.event("CONFIG", "OK", "sites.yaml identical on both servers again")
		} else {
			m.event("CONFIG", "WARN", fmt.Sprintf("sites.yaml differs: local %s vs peer %s", ls.ConfigVersion, hb.ConfigVersion))
		}
	}

	if pull {
		go func() {
			m.OnConfigBehind()
			m.mu.Lock()
			m.pulling = false
			m.mismatchBeats = 0
			m.mu.Unlock()
		}()
	}

	// ---- 2. home region ----
	// With a witness the lease decides; without one (or if it is unreachable)
	// we monitor our own region unless the healthy peer still holds it.
	wantHome := !peerClaimsHome
	if m.witness() {
		granted, _, werr := m.lease(ctx, m.home())
		m.setWitnessOK(werr == nil)
		if werr == nil {
			wantHome = granted
		}
	}

	// ---- 3. peer region: takeover / renew / handback ----
	wantPeer := takenOver
	blocked := ""
	if takenOver {
		if status == PeerHealthy && healthyFor >= m.cfg.Peer.HandbackStableFor {
			wantPeer = false // handback
		} else if m.witness() {
			granted, holder, werr := m.lease(ctx, m.peerRegion())
			if werr == nil && !granted && holder == m.cfg.Peer.ID {
				wantPeer = false // peer reclaimed its region through the witness
			}
		}
	} else if status == PeerDown {
		switch {
		case !m.witness():
			wantPeer = true
		default:
			granted, holder, werr := m.lease(ctx, m.peerRegion())
			switch {
			case werr != nil && m.cfg.Witness.RequiredForTakeover:
				blocked = "witness unreachable; takeover requires witness confirmation"
			case werr != nil:
				wantPeer = true
			case granted:
				wantPeer = true
			default:
				blocked = fmt.Sprintf("peer unreachable from here but %s still holds the %s lease (network split?)", holder, m.peerRegion())
			}
		}
	}

	// ---- 4. apply ----
	m.mu.Lock()
	first := !m.decided
	changed := first || m.owned[m.home()] != wantHome || m.owned[m.peerRegion()] != wantPeer
	hadHome, hadPeer := m.owned[m.home()], m.owned[m.peerRegion()]
	m.owned[m.home()] = wantHome
	m.owned[m.peerRegion()] = wantPeer
	m.decided = true
	if changed {
		m.epoch++
	}
	blockedChanged := blocked != m.blocked
	m.blocked = blocked
	epoch := m.epoch
	m.mu.Unlock()

	if blockedChanged && blocked != "" {
		m.event("FAILOVER", "CRIT", "takeover of "+m.peerRegion()+" blocked: "+blocked)
	}
	if !changed {
		return
	}
	switch {
	case wantPeer && !hadPeer:
		m.event("FAILOVER", "CRIT", fmt.Sprintf("%s is DOWN: %s now monitoring %s sites (epoch %d)", m.cfg.Peer.ID, m.cfg.Node.ID, m.peerRegion(), epoch))
	case !wantPeer && hadPeer:
		if m.witness() {
			m.release(ctx, m.peerRegion())
		}
		m.event("HANDBACK", "OK", fmt.Sprintf("%s healthy for %s: handing %s sites back (epoch %d)", m.cfg.Peer.ID, healthyFor.Round(time.Second), m.peerRegion(), epoch))
	}
	switch {
	case first && wantHome:
		m.event("INFO", "OK", fmt.Sprintf("started: monitoring home region %s (epoch %d)", m.home(), epoch))
	case !wantHome && (first || hadHome):
		m.event("INFO", "WARN", fmt.Sprintf("STANDBY: %s sites are still owned by %s; waiting for handback", m.home(), m.cfg.Peer.ID))
	case wantHome && !hadHome:
		m.event("INFO", "OK", fmt.Sprintf("resumed monitoring home region %s (epoch %d)", m.home(), epoch))
	}
	m.persist()
}

func (m *Manager) setWitnessOK(ok bool) {
	m.mu.Lock()
	changed := ok != m.witnessOK
	m.witnessOK = ok
	m.mu.Unlock()
	if changed {
		if ok {
			m.event("PEER", "OK", "witness reachable again")
		} else {
			m.event("PEER", "WARN", "witness unreachable: "+m.cfg.Witness.URL)
		}
	}
}

// ---------- peer HTTP ----------

func (m *Manager) post(ctx context.Context, path string, timeout time.Duration, in, out any) error {
	return m.request(ctx, http.MethodPost, path, timeout, in, out)
}

func (m *Manager) request(ctx context.Context, method, path string, timeout time.Duration, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, m.cfg.Peer.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: HTTP %d %s", path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, m.cfg.Limits.MaxConfigSizeKB<<10+(1<<20))).Decode(out)
	}
	return nil
}

func (m *Manager) sendHeartbeat(ctx context.Context) (Heartbeat, error) {
	var hb Heartbeat
	err := m.post(ctx, "/peer/heartbeat", m.cfg.Peer.HeartbeatTimeout, m.LocalHeartbeat(), &hb)
	if err == nil && hb.NodeID != m.cfg.Peer.ID {
		err = fmt.Errorf("unexpected peer identity %q", hb.NodeID)
	}
	return hb, err
}

// Verify asks the peer to run the same check from its region.
func (m *Manager) Verify(site, check string) (checks.Result, bool) {
	if !m.PeerHealthy() {
		return checks.Result{}, false
	}
	var r checks.Result
	err := m.post(context.Background(), "/peer/verify", 40*time.Second, map[string]string{"site": site, "check": check}, &r)
	if err != nil {
		slog.Warn("cross-region verify failed", "site", site, "err", err)
		return r, false
	}
	return r, true
}

// ---------- sites.yaml sync ----------

// SitesPayload carries sites.yaml between the servers.
type SitesPayload struct {
	Content string    `json:"content"`
	Hash    string    `json:"hash"`
	ModTime time.Time `json:"mod_time"`
	Actor   string    `json:"actor"`
}

// PushSites sends a sites.yaml that was just changed through the API to the peer.
func (m *Manager) PushSites(ctx context.Context, p SitesPayload) error {
	return m.request(ctx, http.MethodPut, "/peer/config/sites", 15*time.Second, p, nil)
}

// FetchPeerSites downloads the peer's current sites.yaml.
func (m *Manager) FetchPeerSites(ctx context.Context) (SitesPayload, error) {
	var p SitesPayload
	err := m.request(ctx, http.MethodGet, "/peer/config/sites", 15*time.Second, nil, &p)
	return p, err
}

// ---------- replication ----------

func (m *Manager) EnqueueResult(r checks.Result) {
	m.repMu.Lock()
	defer m.repMu.Unlock()
	m.repResults = append(m.repResults, r)
	if over := len(m.repResults) - m.cfg.Limits.ReplicationQueue; over > 0 {
		m.repResults = m.repResults[over:] // bounded: drop oldest
	}
}

func (m *Manager) EnqueueEvent(e store.Event) {
	m.repMu.Lock()
	defer m.repMu.Unlock()
	m.repEvents = append(m.repEvents, e)
	if over := len(m.repEvents) - m.cfg.Limits.ReplicationQueue; over > 0 {
		m.repEvents = m.repEvents[over:]
	}
}

func (m *Manager) replicationLoop(ctx context.Context) {
	t := time.NewTicker(m.cfg.Limits.ReplicationInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.flush(ctx)
		}
	}
}

func (m *Manager) flush(ctx context.Context) {
	m.repMu.Lock()
	if len(m.repResults) == 0 && len(m.repEvents) == 0 {
		m.lastPushOK = time.Now()
		m.repMu.Unlock()
		return
	}
	// While the peer is down, only probe every 15s instead of every 2s.
	if !m.PeerHealthy() && time.Since(m.lastPushTry) < 15*time.Second {
		m.repMu.Unlock()
		return
	}
	n, e := min(len(m.repResults), m.cfg.Limits.ReplicationBatch), min(len(m.repEvents), m.cfg.Limits.ReplicationBatch)
	batch := ReplicationBatch{Results: append([]checks.Result(nil), m.repResults[:n]...), Events: append([]store.Event(nil), m.repEvents[:e]...)}
	m.lastPushTry = time.Now()
	m.repMu.Unlock()

	if err := m.post(ctx, "/peer/replicate", 10*time.Second, batch, nil); err != nil {
		return // keep in queue; retried later (results are de-duplicated by ID)
	}
	m.repMu.Lock()
	m.repResults = m.repResults[n:]
	m.repEvents = m.repEvents[e:]
	m.lastPushOK = time.Now()
	m.repMu.Unlock()
}

// ---------- witness ----------

func (m *Manager) lease(ctx context.Context, region string) (granted bool, holder string, err error) {
	body, _ := json.Marshal(map[string]any{"region": region, "node": m.cfg.Node.ID, "ttl_seconds": int(m.cfg.Witness.LeaseTTL.Seconds())})
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.Witness.URL+"/v1/lease", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	var out struct {
		Granted bool   `json:"granted"`
		Holder  string `json:"holder"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out); err != nil {
		return false, "", err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		return false, "", fmt.Errorf("witness: HTTP %d", resp.StatusCode)
	}
	return out.Granted, out.Holder, nil
}

func (m *Manager) release(ctx context.Context, region string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	q := url.Values{"region": {region}, "node": {m.cfg.Node.ID}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, m.cfg.Witness.URL+"/v1/lease?"+q.Encode(), nil)
	if resp, err := m.client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// On graceful shutdown release leases so the peer can take over immediately.
func (m *Manager) releaseOnShutdown() {
	if !m.witness() {
		return
	}
	for r, v := range m.OwnedRegions() {
		if v {
			m.release(context.Background(), r)
		}
	}
}

// ---------- ownership.yaml ----------

func (m *Manager) ownershipPath() string { return filepath.Join(m.cfg.Node.DataDir, "ownership.yaml") }

func (m *Manager) persist() {
	m.mu.Lock()
	f := ownershipFile{Node: m.cfg.Node.ID, NodeName: m.cfg.Node.Name, Peer: m.cfg.Peer.ID, PeerName: m.cfg.Peer.Name,
		Region: m.home(), Epoch: m.epoch, Mode: m.modeLocked(), OwnedRegions: m.ownedList(), PeerStatus: m.peerStatus, LastPeerHeartbeat: m.lastBeat.UTC(), UpdatedAt: time.Now().UTC()}
	m.mu.Unlock()
	b, _ := yaml.Marshal(f)
	b = append([]byte("# Written by the monitor. Do not edit by hand.\n"), b...)
	tmp := m.ownershipPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		_ = os.Rename(tmp, m.ownershipPath())
	} else {
		slog.Error("cannot write ownership.yaml", "err", err)
	}
}

// restore keeps the epoch, and a takeover in progress, across restarts.
func (m *Manager) restore() {
	b, err := os.ReadFile(m.ownershipPath())
	if err != nil {
		return
	}
	var f ownershipFile
	if yaml.Unmarshal(b, &f) != nil || f.Node != m.cfg.Node.ID {
		return
	}
	m.epoch = f.Epoch
	if slices.Contains(f.OwnedRegions, m.peerRegion()) {
		m.owned[m.peerRegion()] = true
		m.owned[m.home()] = slices.Contains(f.OwnedRegions, m.home())
	}
}

func (m *Manager) Snapshot() Snapshot {
	lm := m.local().ConfigModTime
	m.repMu.Lock()
	q := len(m.repResults) + len(m.repEvents)
	lag := 0.0
	if q > 0 {
		lag = time.Since(m.lastPushOK).Seconds()
	}
	m.repMu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	age := -1.0
	if !m.lastBeat.IsZero() {
		age = time.Since(m.lastBeat).Seconds()
	}
	return Snapshot{
		Node: m.cfg.Node.ID, NodeName: m.cfg.Node.Name, Region: m.home(), Epoch: m.epoch, Mode: m.modeLocked(), OwnedRegions: m.ownedList(),
		PeerID: m.cfg.Peer.ID, PeerName: m.cfg.Peer.Name, PeerRegion: m.peerRegion(), PeerStatus: m.peerStatus, PeerMode: m.peer.Mode,
		PeerOwned: m.peer.OwnedRegions, PeerEpoch: m.peer.Epoch, HeartbeatAgeSec: age, MissedBeats: m.missed,
		LastError: m.lastErr, ConfigMatch: m.configMatch, PeerConfigVer: m.peer.ConfigVersion,
		WitnessEnabled: m.witness(), WitnessReachable: m.witnessOK, ReplicationQueue: q, ReplicationLag: lag,
		Blocked: m.blocked, ConfigBehind: !m.configMatch && m.peer.ConfigModTime.After(lm),
	}
}
