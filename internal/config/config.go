// Package config loads the per-server node.yaml and the shared sites.yaml.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Regions known to the cluster.
const (
	RegionIDC = "IDC"
	RegionFC  = "FC"
)

func validRegion(r string) bool { return r == RegionIDC || r == RegionFC }

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces only ${VAR} references. Unlike os.ExpandEnv it leaves
// other "$" characters alone, so bcrypt hashes ($2a$12$...) survive.
func ExpandEnv(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

// ---------- node.yaml (different on every server) ----------

type Node struct {
	Node      NodeSelf      `yaml:"node"`
	Peer      PeerConfig    `yaml:"peer"`
	Witness   WitnessConf   `yaml:"witness"`
	TLS       TLSConfig     `yaml:"tls"`
	Monitor   MonitorConf   `yaml:"monitor"`
	Limits    Limits        `yaml:"limits"`
	Auth      AuthConfig    `yaml:"auth"`
	ConfigAPI ConfigAPIConf `yaml:"config_api"`
	Jobs      JobsConf      `yaml:"jobs"`
}

// JobsConf configures user/admin periodic jobs: binaries run locally or on
// remote Linux / Windows (Cygwin) servers over SSH.
type JobsConf struct {
	Enabled         bool                `yaml:"enabled"`
	BinariesDir     string              `yaml:"binaries_dir"` // the one directory holding every uploaded program
	WorkDir         string              `yaml:"work_dir"`     // per-run scratch directories (deleted after each run)
	MaxBinarySizeMB int64               `yaml:"max_binary_size_mb"`
	MaxConcurrent   int                 `yaml:"max_concurrent"` // jobs running at the same time
	DefaultTimeout  time.Duration       `yaml:"default_timeout"`
	MaxTimeout      time.Duration       `yaml:"max_timeout"`
	MaxOutputKB     int64               `yaml:"max_output_kb"`    // stdout/stderr kept per run
	UserMaxRunFor   time.Duration       `yaml:"user_max_run_for"` // longest window a user job may run (24h)
	RequireApproval bool                `yaml:"require_approval"` // user jobs wait for an admin
	Interpreters    map[string][]string `yaml:"interpreters"`     // file extension -> command prefix (local runs)
	ExitCodes       ExitCodeMap         `yaml:"exit_codes"`
	StartupHook     bool                `yaml:"startup_hook"` // call the (cgo) startup hook when the server comes up
	Remote          RemoteConf          `yaml:"remote"`
}

// ExitCodeMap maps program exit codes to OK / WARN; anything else is CRIT.
type ExitCodeMap struct {
	OK   []int `yaml:"ok"`
	Warn []int `yaml:"warn"`
}

type RemoteConf struct {
	KnownHosts     string                `yaml:"known_hosts"` // OpenSSH known_hosts; host keys are always verified
	ConnectTimeout time.Duration         `yaml:"connect_timeout"`
	RemoteDir      string                `yaml:"remote_dir"`  // where programs are pushed (/tmp; works on Cygwin too)
	Credentials    map[string]Credential `yaml:"credentials"` // jobs reference these by name; secrets never go through the API
}

type Credential struct {
	User       string `yaml:"user"`
	Password   string `yaml:"password"`    // use ${ENV}
	PrivateKey string `yaml:"private_key"` // path to an OpenSSH private key
	Passphrase string `yaml:"passphrase"`  // use ${ENV}
}

type NodeSelf struct {
	ID              string `yaml:"id"`   // identity; must equal the TLS certificate CN
	Name            string `yaml:"name"` // human-friendly display name
	Region          string `yaml:"region"`
	PeerListen      string `yaml:"peer_listen"`      // mTLS listener for peer traffic
	DashboardListen string `yaml:"dashboard_listen"` // HTTPS listener for humans, APIs and /metrics
	DataDir         string `yaml:"data_dir"`
}

type PeerConfig struct {
	ID                string        `yaml:"id"`
	Name              string        `yaml:"name"`
	Region            string        `yaml:"region"`
	URL               string        `yaml:"url"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	HeartbeatTimeout  time.Duration `yaml:"heartbeat_timeout"`
	SuspectAfter      int           `yaml:"suspect_after"`
	DownAfter         int           `yaml:"down_after"`
	HandbackStableFor time.Duration `yaml:"handback_stable_for"`
}

type WitnessConf struct {
	URL                 string        `yaml:"url"` // empty = no witness
	RequiredForTakeover bool          `yaml:"required_for_takeover"`
	LeaseTTL            time.Duration `yaml:"lease_ttl"`
}

type TLSConfig struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
	CA   string `yaml:"ca"`
}

type MonitorConf struct {
	SitesFile       string `yaml:"sites_file"`
	FailuresToAlert int    `yaml:"failures_to_alert"`
}

// Limits bounds every pool, queue and buffer in the process.
type Limits struct {
	Workers               int           `yaml:"workers"`                 // goroutines executing checks
	PerHostLimit          int           `yaml:"per_host_limit"`          // concurrent connections to one site
	QueueSize             int           `yaml:"queue_size"`              // pending check jobs
	VerifyConcurrency     int           `yaml:"verify_concurrency"`      // concurrent cross-region verify requests served
	MaxSites              int           `yaml:"max_sites"`               // reject sites.yaml with more sites
	HistoryPerCheck       int           `yaml:"history_per_check"`       // results kept per site/check (~0.6 KB each)
	MaxEvents             int           `yaml:"max_events"`              // events kept
	MaxResponseBodyKB     int64         `yaml:"max_response_body_kb"`    // bytes read from a monitored site
	MaxRedirects          int           `yaml:"max_redirects"`           // redirects followed by checks
	ReplicationQueue      int           `yaml:"replication_queue"`       // results+events waiting for the peer
	ReplicationBatch      int           `yaml:"replication_batch"`       // items per push
	ReplicationInterval   time.Duration `yaml:"replication_interval"`    // push period
	MaxReplicationBodyMB  int64         `yaml:"max_replication_body_mb"` // accepted from the peer
	MaxAPIBodyKB          int64         `yaml:"max_api_body_kb"`         // login + config API request bodies
	MaxConfigSizeKB       int64         `yaml:"max_config_size_kb"`      // sites.yaml / node.yaml size
	SnapshotInterval      time.Duration `yaml:"snapshot_interval"`       // deprecated: used as db_maintenance_interval if that is unset
	DBMaintenanceInterval time.Duration `yaml:"db_maintenance_interval"` // retention clean-up of the database
	HourlyStatsRetention  time.Duration `yaml:"hourly_stats_retention"`  // hourly buckets kept (24h uptime)
	DailyStatsRetention   time.Duration `yaml:"daily_stats_retention"`   // daily buckets kept (30d uptime, yearly reports)
	HTTPReadTimeout       time.Duration `yaml:"http_read_timeout"`
	HTTPWriteTimeout      time.Duration `yaml:"http_write_timeout"`
}

// AuthConfig selects how users of the dashboard and APIs are authenticated.
type AuthConfig struct {
	// Providers are tried in order: [local] for a POC, [ldap] for Active
	// Directory, [ldap, local] for AD with a local break-glass account.
	Providers       []string      `yaml:"providers"`
	SessionTTL      time.Duration `yaml:"session_ttl"`
	MaxSessions     int           `yaml:"max_sessions"`
	BasicCacheTTL   time.Duration `yaml:"basic_cache_ttl"` // cache successful Basic-auth logins (avoids an AD bind per API call)
	MaxFailedLogins int           `yaml:"max_failed_logins"`
	Lockout         time.Duration `yaml:"lockout"`
	Local           LocalAuth     `yaml:"local"`
	LDAP            LDAPAuth      `yaml:"ldap"`
}

type LocalAuth struct {
	Users []LocalUser `yaml:"users"`
}

type LocalUser struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"` // bcrypt; create with: monitor hash-password
	Role         string `yaml:"role"`          // viewer | operator | admin
	DisplayName  string `yaml:"display_name"`
}

type LDAPAuth struct {
	URL                string              `yaml:"url"`       // ldaps://dc01.corp.example.com:636 or ldap://...:389 with start_tls
	StartTLS           bool                `yaml:"start_tls"` // upgrade ldap:// to TLS (required for ldap://)
	CAFile             string              `yaml:"ca_file"`   // AD CA certificate; empty = system trust store
	InsecureSkipVerify bool                `yaml:"insecure_skip_verify"`
	BindDN             string              `yaml:"bind_dn"`       // service account used to search for users
	BindPassword       string              `yaml:"bind_password"` // use ${LDAP_BIND_PASSWORD}
	BaseDN             string              `yaml:"base_dn"`
	UserFilter         string              `yaml:"user_filter"` // {username} is replaced (escaped)
	DisplayAttr        string              `yaml:"display_attr"`
	NestedGroups       bool                `yaml:"nested_groups"` // AD LDAP_MATCHING_RULE_IN_CHAIN
	RoleGroups         map[string][]string `yaml:"role_groups"`   // role -> group DNs
	DefaultRole        string              `yaml:"default_role"`  // role when no group matches; empty = deny
	Timeout            time.Duration       `yaml:"timeout"`
}

type ConfigAPIConf struct {
	Enabled       bool `yaml:"enabled"`
	SyncToPeer    bool `yaml:"sync_to_peer"`    // push sites.yaml changes to the peer
	AutoPullNewer bool `yaml:"auto_pull_newer"` // fetch a newer sites.yaml from the peer on mismatch
	Backups       int  `yaml:"backups"`         // previous versions kept next to the file
}

// Roles, lowest to highest privilege.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

func RoleRank(r string) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

func LoadNode(path string) (*Node, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseNode(b)
}

// ParseNode parses and validates node.yaml content (${ENV} is expanded).
func ParseNode(b []byte) (*Node, error) {
	var n Node
	n.ConfigAPI = ConfigAPIConf{Enabled: true, SyncToPeer: true, AutoPullNewer: true, Backups: 10}
	dec := yaml.NewDecoder(strings.NewReader(ExpandEnv(string(b))))
	dec.KnownFields(true) // typos in node.yaml are errors, not silently ignored
	if err := dec.Decode(&n); err != nil {
		return nil, fmt.Errorf("node.yaml: %w", err)
	}
	n.applyDefaults()
	return &n, n.validate()
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

func (n *Node) applyDefaults() {
	setDefault(&n.Node.Name, n.Node.ID)
	setDefault(&n.Peer.Name, n.Peer.ID)
	setDefault(&n.Node.PeerListen, ":8443")
	setDefault(&n.Node.DashboardListen, ":8080")
	setDefault(&n.Node.DataDir, "/data")
	setDefault(&n.Peer.HeartbeatInterval, 5*time.Second)
	setDefault(&n.Peer.HeartbeatTimeout, 2*time.Second)
	setDefault(&n.Peer.SuspectAfter, 3)
	setDefault(&n.Peer.DownAfter, 6)
	setDefault(&n.Peer.HandbackStableFor, 2*time.Minute)
	setDefault(&n.Witness.LeaseTTL, 20*time.Second)
	setDefault(&n.Monitor.FailuresToAlert, 2)

	l := &n.Limits
	setDefault(&l.Workers, 20)
	setDefault(&l.PerHostLimit, 2)
	setDefault(&l.QueueSize, 500)
	setDefault(&l.VerifyConcurrency, 4)
	setDefault(&l.MaxSites, 2000)
	setDefault(&l.HistoryPerCheck, 200)
	setDefault(&l.MaxEvents, 500)
	setDefault(&l.MaxResponseBodyKB, 1024)
	setDefault(&l.MaxRedirects, 5)
	setDefault(&l.ReplicationQueue, 20000)
	setDefault(&l.ReplicationBatch, 1000)
	setDefault(&l.ReplicationInterval, 2*time.Second)
	setDefault(&l.MaxReplicationBodyMB, 32)
	setDefault(&l.MaxAPIBodyKB, 1024)
	setDefault(&l.MaxConfigSizeKB, 1024)
	setDefault(&l.SnapshotInterval, 5*time.Minute)
	setDefault(&l.DBMaintenanceInterval, l.SnapshotInterval)
	setDefault(&l.HourlyStatsRetention, 7*24*time.Hour)
	setDefault(&l.DailyStatsRetention, 400*24*time.Hour)
	setDefault(&l.HTTPReadTimeout, 30*time.Second)
	setDefault(&l.HTTPWriteTimeout, 60*time.Second)

	a := &n.Auth
	if len(a.Providers) == 0 {
		a.Providers = []string{"local"}
	}
	setDefault(&a.SessionTTL, 8*time.Hour)
	setDefault(&a.MaxSessions, 1000)
	setDefault(&a.BasicCacheTTL, time.Minute)
	setDefault(&a.MaxFailedLogins, 5)
	setDefault(&a.Lockout, 5*time.Minute)
	setDefault(&a.LDAP.UserFilter, "(&(objectClass=user)(sAMAccountName={username}))")
	setDefault(&a.LDAP.DisplayAttr, "displayName")
	setDefault(&a.LDAP.Timeout, 5*time.Second)

	j := &n.Jobs
	setDefault(&j.BinariesDir, n.Node.DataDir+"/jobs/bin")
	setDefault(&j.WorkDir, n.Node.DataDir+"/jobs/work")
	setDefault(&j.MaxBinarySizeMB, 50)
	setDefault(&j.MaxConcurrent, 4)
	setDefault(&j.DefaultTimeout, 5*time.Minute)
	setDefault(&j.MaxTimeout, 30*time.Minute)
	setDefault(&j.MaxOutputKB, 64)
	setDefault(&j.UserMaxRunFor, 24*time.Hour)
	if j.Interpreters == nil {
		j.Interpreters = map[string][]string{
			".py":   {"python3"},
			".pl":   {"perl"},
			".ps1":  {"pwsh", "-NoLogo", "-NoProfile", "-NonInteractive", "-File"},
			".sh":   {"sh"},
			".bash": {"bash"},
		}
	}
	if j.ExitCodes.OK == nil {
		j.ExitCodes.OK = []int{0}
	}
	if j.ExitCodes.Warn == nil {
		j.ExitCodes.Warn = []int{1}
	}
	setDefault(&j.Remote.KnownHosts, n.Node.DataDir+"/jobs/known_hosts")
	setDefault(&j.Remote.ConnectTimeout, 10*time.Second)
	setDefault(&j.Remote.RemoteDir, "/tmp")
}

func (n *Node) validate() error {
	l := n.Limits
	switch {
	case n.Node.ID == "":
		return fmt.Errorf("node.id is required")
	case !validRegion(n.Node.Region):
		return fmt.Errorf("node.region must be IDC or FC, got %q", n.Node.Region)
	case n.Peer.ID == "" || n.Peer.URL == "":
		return fmt.Errorf("peer.id and peer.url are required")
	case n.Peer.ID == n.Node.ID:
		return fmt.Errorf("peer.id must differ from node.id")
	case !validRegion(n.Peer.Region) || n.Peer.Region == n.Node.Region:
		return fmt.Errorf("peer.region must be the other region (IDC/FC), got %q", n.Peer.Region)
	case n.TLS.Cert == "" || n.TLS.Key == "" || n.TLS.CA == "":
		return fmt.Errorf("tls.cert, tls.key and tls.ca are required (peer traffic is mTLS)")
	case n.Monitor.SitesFile == "":
		return fmt.Errorf("monitor.sites_file is required")
	case n.Peer.DownAfter < n.Peer.SuspectAfter:
		return fmt.Errorf("peer.down_after must be >= peer.suspect_after")
	case n.Peer.HeartbeatTimeout > n.Peer.HeartbeatInterval:
		return fmt.Errorf("peer.heartbeat_timeout must be <= peer.heartbeat_interval")
	case l.Workers < 1 || l.Workers > 1000:
		return fmt.Errorf("limits.workers must be 1..1000")
	case l.PerHostLimit < 1 || l.PerHostLimit > 20:
		return fmt.Errorf("limits.per_host_limit must be 1..20")
	case l.QueueSize < l.Workers:
		return fmt.Errorf("limits.queue_size must be >= limits.workers")
	case l.HistoryPerCheck < 1 || l.HistoryPerCheck > 100000:
		return fmt.Errorf("limits.history_per_check must be 1..100000")
	case l.ReplicationBatch < 1 || l.ReplicationBatch > l.ReplicationQueue:
		return fmt.Errorf("limits.replication_batch must be 1..replication_queue")
	case l.HourlyStatsRetention < 25*time.Hour:
		return fmt.Errorf("limits.hourly_stats_retention must be at least 25h (24h uptime)")
	case l.DailyStatsRetention < 31*24*time.Hour:
		return fmt.Errorf("limits.daily_stats_retention must be at least 31 days (30d uptime)")
	}
	if err := n.validateJobs(); err != nil {
		return err
	}
	return n.validateAuth()
}

func (n *Node) validateJobs() error {
	j := n.Jobs
	switch {
	case j.MaxConcurrent < 1 || j.MaxConcurrent > 100:
		return fmt.Errorf("jobs.max_concurrent must be 1..100")
	case j.DefaultTimeout > j.MaxTimeout:
		return fmt.Errorf("jobs.default_timeout must be <= jobs.max_timeout")
	case j.UserMaxRunFor <= 0 || j.UserMaxRunFor > 24*time.Hour:
		return fmt.Errorf("jobs.user_max_run_for must be between 1s and 24h")
	case j.MaxBinarySizeMB < 1 || j.MaxBinarySizeMB > 1024:
		return fmt.Errorf("jobs.max_binary_size_mb must be 1..1024")
	}
	for name, c := range j.Remote.Credentials {
		// The secret may come from an environment variable that is not set on
		// every server; a credential without one fails when a job uses it.
		if c.User == "" {
			return fmt.Errorf("jobs.remote.credentials.%s: user is required", name)
		}
	}
	return nil
}

func (n *Node) validateAuth() error {
	a := n.Auth
	for _, p := range a.Providers {
		switch p {
		case "local":
			if len(a.Local.Users) == 0 {
				return fmt.Errorf("auth: provider local needs at least one auth.local.users entry")
			}
			seen := map[string]bool{}
			for _, u := range a.Local.Users {
				if u.Username == "" || seen[u.Username] {
					return fmt.Errorf("auth.local.users: empty or duplicate username %q", u.Username)
				}
				seen[u.Username] = true
				if !strings.HasPrefix(u.PasswordHash, "$2") {
					return fmt.Errorf("auth.local.users[%s]: password_hash must be a bcrypt hash (monitor hash-password)", u.Username)
				}
				if RoleRank(u.Role) == 0 {
					return fmt.Errorf("auth.local.users[%s]: role must be viewer, operator or admin", u.Username)
				}
			}
		case "ldap":
			l := a.LDAP
			if l.URL == "" || l.BaseDN == "" {
				return fmt.Errorf("auth.ldap: url and base_dn are required")
			}
			if strings.HasPrefix(l.URL, "ldap://") && !l.StartTLS {
				return fmt.Errorf("auth.ldap: ldap:// without start_tls would send passwords in clear text; use ldaps:// or start_tls: true")
			}
			if !strings.Contains(l.UserFilter, "{username}") {
				return fmt.Errorf("auth.ldap.user_filter must contain {username}")
			}
			for role := range l.RoleGroups {
				if RoleRank(role) == 0 {
					return fmt.Errorf("auth.ldap.role_groups: unknown role %q", role)
				}
			}
			if l.DefaultRole != "" && RoleRank(l.DefaultRole) == 0 {
				return fmt.Errorf("auth.ldap.default_role: unknown role %q", l.DefaultRole)
			}
			if len(l.RoleGroups) == 0 && l.DefaultRole == "" {
				return fmt.Errorf("auth.ldap: configure role_groups (or default_role), otherwise nobody can log in")
			}
		default:
			return fmt.Errorf("auth.providers: unknown provider %q (use local or ldap)", p)
		}
	}
	return nil
}

// ---------- sites.yaml (identical on both servers) ----------

// Check types.
const (
	CheckAvailability = "availability"
	CheckTLS          = "tls"
	CheckCert         = "cert"
	CheckHeaders      = "headers"
	CheckAPI          = "api"
)

var AllChecks = []string{CheckAvailability, CheckTLS, CheckCert, CheckHeaders, CheckAPI}

type Sites struct {
	Version  string   `yaml:"version"`
	Defaults Defaults `yaml:"defaults"`
	Sites    []Site   `yaml:"sites"`

	Hash string `yaml:"-"` // sha256 of the file, compared between peers
}

type Defaults struct {
	Timeout      time.Duration            `yaml:"timeout,omitempty"`
	UserAgent    string                   `yaml:"user_agent,omitempty"`
	Intervals    map[string]time.Duration `yaml:"intervals,omitempty"`
	CertWarnDays int                      `yaml:"cert_warn_days,omitempty"`
	CertCritDays int                      `yaml:"cert_crit_days,omitempty"`
}

type Site struct {
	Name        string                   `yaml:"name"`
	Region      string                   `yaml:"region"`
	URL         string                   `yaml:"url"`
	Owner       string                   `yaml:"owner,omitempty"`
	Criticality string                   `yaml:"criticality,omitempty"` // high | medium | low
	Checks      []string                 `yaml:"checks,omitempty"`
	Timeout     time.Duration            `yaml:"timeout,omitempty"`
	Intervals   map[string]time.Duration `yaml:"intervals,omitempty"`
	TLSVerify   *bool                    `yaml:"tls_verify,omitempty"` // false only for self-signed test targets
	Expect      Expect                   `yaml:"expect,omitempty"`
	API         []APICall                `yaml:"api,omitempty"`
}

type Expect struct {
	Status       int    `yaml:"status,omitempty"`
	Contains     string `yaml:"contains,omitempty"`
	MaxLatencyMs int64  `yaml:"max_latency_ms,omitempty"`
}

type APICall struct {
	Name    string            `yaml:"name"`
	Method  string            `yaml:"method,omitempty"`
	Path    string            `yaml:"path"`
	Body    string            `yaml:"body,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"` // values may use ${ENV_VAR} for secrets
	Expect  APIExpect         `yaml:"expect,omitempty"`
}

type APIExpect struct {
	Status   int    `yaml:"status,omitempty"`
	JSONPath string `yaml:"json_path,omitempty"` // simple dotted path: $.a.b
	Equals   string `yaml:"equals,omitempty"`
	Exists   bool   `yaml:"exists,omitempty"`
}

var builtinIntervals = map[string]time.Duration{
	CheckAvailability: time.Minute,
	CheckAPI:          5 * time.Minute,
	CheckCert:         6 * time.Hour,
	CheckTLS:          24 * time.Hour,
	CheckHeaders:      24 * time.Hour,
}

func LoadSites(path string) (*Sites, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseSites(b)
}

// ParseSites parses and validates sites.yaml content and applies defaults.
func ParseSites(b []byte) (*Sites, error) {
	var s Sites
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("sites.yaml: %w", err)
	}
	sum := sha256.Sum256(b)
	s.Hash = hex.EncodeToString(sum[:])

	d := &s.Defaults
	setDefault(&d.Timeout, 15*time.Second)
	setDefault(&d.UserAgent, "SiteMonitor/1.0")
	setDefault(&d.CertWarnDays, 30)
	setDefault(&d.CertCritDays, 7)
	if d.Intervals == nil {
		d.Intervals = map[string]time.Duration{}
	}
	for k, v := range builtinIntervals {
		if d.Intervals[k] == 0 {
			d.Intervals[k] = v
		}
	}

	seen := map[string]bool{}
	for i := range s.Sites {
		st := &s.Sites[i]
		if err := st.validate(); err != nil {
			return nil, fmt.Errorf("site #%d: %w", i+1, err)
		}
		if seen[st.Name] {
			return nil, fmt.Errorf("duplicate site name %q", st.Name)
		}
		seen[st.Name] = true
		if len(st.Checks) == 0 {
			st.Checks = []string{CheckAvailability, CheckTLS, CheckCert}
		}
		setDefault(&st.Criticality, "medium")
		setDefault(&st.Timeout, d.Timeout)
	}
	return &s, nil
}

func (st *Site) validate() error {
	if st.Name == "" || st.URL == "" {
		return fmt.Errorf("name and url are required")
	}
	if !validRegion(st.Region) {
		return fmt.Errorf("site %q: region must be IDC or FC, got %q", st.Name, st.Region)
	}
	if !strings.HasPrefix(st.URL, "https://") && !strings.HasPrefix(st.URL, "http://") {
		return fmt.Errorf("site %q: url must start with http(s)://", st.Name)
	}
	for _, c := range st.Checks {
		if _, ok := builtinIntervals[c]; !ok {
			return fmt.Errorf("site %q: unknown check %q", st.Name, c)
		}
	}
	switch st.Criticality {
	case "", "high", "medium", "low":
	default:
		return fmt.Errorf("site %q: criticality must be high, medium or low", st.Name)
	}
	return nil
}

// CheckLimits applies the node's limits to a sites file.
func (s *Sites) CheckLimits(l Limits) error {
	if len(s.Sites) > l.MaxSites {
		return fmt.Errorf("%d sites exceeds limits.max_sites (%d)", len(s.Sites), l.MaxSites)
	}
	return nil
}

// Interval returns how often the given check should run for a site.
func (s *Sites) Interval(site *Site, check string) time.Duration {
	if v := site.Intervals[check]; v > 0 {
		return v
	}
	return s.Defaults.Intervals[check]
}

// Verify reports whether certificate chain verification is enabled for a site.
func (st *Site) Verify() bool { return st.TLSVerify == nil || *st.TLSVerify }
