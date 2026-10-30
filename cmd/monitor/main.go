// Command monitor is the single binary for every role:
//
//	monitor run         -config node.yaml        the website monitor (IDC or FC)
//	monitor witness     -cert ... -key ... -ca   lease tie-breaker at a third location
//	monitor gen-certs   -out dir -names a,b,c    create the private CA + mTLS certs
//	monitor demo-target -name x -cert-days 20    fake website for local testing
//	monitor hash-password                        bcrypt hash for auth.local.users
//	monitor demo-ssh                             demo SSH server for remote jobs (docker-compose)
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sitemonitor/internal/alert"
	"sitemonitor/internal/auth"
	"sitemonitor/internal/certs"
	"sitemonitor/internal/checks"
	"sitemonitor/internal/cluster"
	"sitemonitor/internal/config"
	"sitemonitor/internal/configstore"
	"sitemonitor/internal/demotarget"
	"sitemonitor/internal/jobs"
	"sitemonitor/internal/periodic"
	"sitemonitor/internal/scheduler"
	"sitemonitor/internal/server"
	"sitemonitor/internal/startup"
	"sitemonitor/internal/store"
	"sitemonitor/internal/witness"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: monitor run|witness|gen-certs|demo-target|demo-ssh|hash-password [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runMonitor(os.Args[2:])
	case "witness":
		err = runWitness(os.Args[2:])
	case "gen-certs":
		err = genCerts(os.Args[2:])
	case "demo-target":
		err = runDemoTarget(os.Args[2:])
	case "hash-password":
		err = hashPassword()
	case "demo-ssh":
		err = runDemoSSH(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func listen(ctx context.Context, name string, srv *http.Server) {
	go func() {
		slog.Info("listening", "listener", name, "addr", srv.Addr)
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listener failed", "listener", name, "err", err)
			os.Exit(1)
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
}

func runMonitor(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", "/etc/monitor/node.yaml", "path to node.yaml")
	fs.Parse(args)

	cfg, err := config.LoadNode(*path)
	if err != nil {
		return err
	}
	sites, err := config.LoadSites(cfg.Monitor.SitesFile)
	if err == nil {
		err = sites.CheckLimits(cfg.Limits)
	}
	if err != nil {
		return err
	}
	checks.Configure(cfg.Limits)
	if err := os.MkdirAll(cfg.Node.DataDir, 0o700); err != nil {
		return err
	}
	client, err := certs.MTLSClient(cfg.TLS.Cert, cfg.TLS.Key, cfg.TLS.CA)
	if err != nil {
		return fmt.Errorf("peer TLS: %w", err)
	}
	slog.Info("starting monitor", "node", cfg.Node.ID, "name", cfg.Node.Name, "region", cfg.Node.Region,
		"peer", cfg.Peer.ID, "peer_name", cfg.Peer.Name, "sites", len(sites.Sites), "config_version", sites.Version,
		"auth", cfg.Auth.Providers, "workers", cfg.Limits.Workers)

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(sigCtx) // cancel() = graceful restart requested through the API
	defer cancel()

	st, err := store.Open(cfg.Node.DataDir, store.Options{
		HistoryPerCheck: cfg.Limits.HistoryPerCheck, MaxEvents: cfg.Limits.MaxEvents,
		HourlyRetention: cfg.Limits.HourlyStatsRetention, DailyRetention: cfg.Limits.DailyStatsRetention,
	})
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer st.Close()
	slog.Info("database open", "path", st.Path())
	notifier := alert.NewNotifier(os.Getenv("ALERT_WEBHOOK_URL"))

	// Wiring order: cluster needs scheduler status; scheduler needs cluster ownership.
	var sched *scheduler.Scheduler
	var cl *cluster.Manager

	emit := func(e store.Event) { // every event: store locally, notify, replicate
		if st.AddEvent(e) {
			notifier.Send(e)
			cl.EnqueueEvent(e)
		}
	}
	var jobsSvc *jobs.Service
	local := func() cluster.LocalStatus {
		s, stats := sched.Sites(), sched.Stats()
		jobsHash := ""
		if jobsSvc != nil {
			jobsHash = jobsSvc.Hash()
		}
		var mod time.Time
		if fi, err := os.Stat(cfg.Monitor.SitesFile); err == nil {
			mod = fi.ModTime().UTC()
		}
		return cluster.LocalStatus{ConfigVersion: s.Version, ConfigHash: s.Hash, ConfigModTime: mod,
			Saturation: stats.Saturation, LastCheck: stats.LastCheck, Healthy: sched.Healthy(), JobsHash: jobsHash}
	}
	cl = cluster.New(cfg, client, local, emit)
	eval := alert.NewEvaluator(cfg.Node.ID, cfg.Monitor.FailuresToAlert, emit, cl.Verify, st.IncidentLevel)

	onResult := func(r checks.Result) {
		st.AddResult(r)
		cl.EnqueueResult(r)
		eval.Process(r)
	}
	onConfig := func(old, nw *config.Sites) {
		emit(alert.NewEvent(cfg.Node.ID, "CONFIG", "OK", "", "",
			fmt.Sprintf("sites.yaml reloaded: %s -> %s (%d sites)", old.Version, nw.Version, len(nw.Sites))))
	}
	// One periodic engine for everything that runs on a period, with a separate
	// bounded pool per class so jobs can never starve website checks.
	engine := periodic.New(map[string]periodic.PoolConfig{
		scheduler.Class: {Workers: cfg.Limits.Workers, Queue: cfg.Limits.QueueSize},
		jobs.Class:      {Workers: cfg.Jobs.MaxConcurrent, Queue: 4 * cfg.Jobs.MaxConcurrent},
	})
	sched = scheduler.New(cfg, sites, cl.OwnedRegions, engine, st.LastResultTime, onResult, onConfig)

	authMgr, err := auth.New(cfg.Auth, func(kind, sev, msg string) {
		emit(alert.NewEvent(cfg.Node.ID, kind, sev, "", "", msg))
	})
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	go authMgr.Run(ctx)
	// API tokens (agents): "Authorization: Bearer <token>"
	authMgr.TokenLookup = func(token string) (*auth.Identity, bool) {
		t, ok := st.LookupToken(store.TokenHash(token))
		if !ok {
			return nil, false
		}
		return &auth.Identity{Username: "token:" + t.Name, DisplayName: t.Name, Role: t.Role, Provider: "token"}, true
	}

	// User / admin periodic jobs. Runs are stored like check results, so they
	// get counters, incidents, metrics and replication.
	if cfg.Jobs.Enabled {
		jobsSvc, err = jobs.New(cfg.Jobs, cfg.Node.ID, cfg.Node.Region, st, engine, cl.OwnedRegions, onResult,
			func(kind, sev, msg string) { emit(alert.NewEvent(cfg.Node.ID, kind, sev, "", "", msg)) })
		if err != nil {
			return err
		}
	}

	srv := &server.Server{Cfg: cfg, Store: st, Sched: sched, Cluster: cl, Auth: authMgr, Emit: emit, Jobs: jobsSvc,
		Config: configstore.New(*path, cfg, sched.SetSites),
		Restart: func(actor string) {
			time.Sleep(500 * time.Millisecond) // let the HTTP response go out
			slog.Warn("graceful restart requested", "by", actor)
			cancel()
		}}
	if cfg.ConfigAPI.AutoPullNewer {
		cl.OnConfigBehind = srv.PullFromPeer
	}
	if jobsSvc != nil {
		jobsSvc.Changed = srv.NotifyPeerJobs // push: the peer pulls right away
		cl.OnJobsDiffer = srv.PullJobs       // and heartbeats catch anything missed
	}
	peerTLS, err := certs.ServerTLS(cfg.TLS.Cert, cfg.TLS.Key, cfg.TLS.CA, true)
	if err != nil {
		return err
	}
	dashTLS, err := certs.ServerTLS(cfg.TLS.Cert, cfg.TLS.Key, cfg.TLS.CA, false)
	if err != nil {
		return err
	}
	lim := cfg.Limits
	listen(ctx, "peer", &http.Server{Addr: cfg.Node.PeerListen, Handler: srv.PeerHandler(), TLSConfig: peerTLS,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: lim.HTTPReadTimeout, WriteTimeout: lim.HTTPWriteTimeout, MaxHeaderBytes: 64 << 10})
	listen(ctx, "dashboard", &http.Server{Addr: cfg.Node.DashboardListen, Handler: srv.DashboardHandler(), TLSConfig: dashTLS,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: lim.HTTPReadTimeout, WriteTimeout: lim.HTTPWriteTimeout, MaxHeaderBytes: 64 << 10})

	go func() { // periodic database retention clean-up (every result is already committed)
		t := time.NewTicker(cfg.Limits.DBMaintenanceInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := st.Maintain(time.Now()); err != nil {
					slog.Error("database maintenance failed", "err", err)
				}
			}
		}
	}()

	// Region ownership changed (start, takeover, handback): re-declare tasks now.
	cl.OnOwnershipChange = func() {
		sched.Reconcile()
		if jobsSvc != nil {
			jobsSvc.Reconcile()
		}
	}
	go cl.Run(ctx)

	// "Server came up": once this server knows which regions it owns, call the
	// native startup hook (placeholder, cgo) and run the startup jobs.
	ready := make(chan struct{})
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for len(cl.OwnedRegions()) == 0 && time.Now().Before(deadline) && ctx.Err() == nil {
			time.Sleep(500 * time.Millisecond)
		}
		if cfg.Jobs.StartupHook {
			rc, err := startup.Run(startup.Info{Node: cfg.Node.ID, Region: cfg.Node.Region})
			switch {
			case errors.Is(err, startup.ErrNotCompiled):
				slog.Info("startup hook", "status", err.Error())
			case err != nil:
				emit(alert.NewEvent(cfg.Node.ID, "STARTUP", "WARN", "", "", fmt.Sprintf("startup hook failed (rc=%d): %v", rc, err)))
			default:
				emit(alert.NewEvent(cfg.Node.ID, "STARTUP", "OK", "", "", "startup hook completed"))
			}
		}
		close(ready)
	}()
	if jobsSvc != nil {
		go jobsSvc.Run(ctx, ready)
	}
	go sched.Run(ctx)
	engine.Run(ctx) // blocks until SIGTERM, then waits for running checks and jobs

	slog.Info("stopped")
	return nil
}

// hashPassword reads a password from stdin and prints its bcrypt hash.
func hashPassword() error {
	fmt.Fprint(os.Stderr, "password: ")
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && pw == "" {
		return err
	}
	pw = strings.TrimRight(pw, "\r\n")
	if len(pw) < 12 {
		return errors.New("use at least 12 characters")
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Println(h)
	return nil
}

func runWitness(args []string) error {
	fs := flag.NewFlagSet("witness", flag.ExitOnError)
	addr := fs.String("addr", ":9443", "listen address")
	cert := fs.String("cert", "/certs/witness.crt", "")
	key := fs.String("key", "/certs/witness.key", "")
	ca := fs.String("ca", "/certs/ca.crt", "")
	fs.Parse(args)
	tcfg, err := certs.ServerTLS(*cert, *key, *ca, true)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	listen(ctx, "witness", &http.Server{Addr: *addr, Handler: witness.New().Handler(), TLSConfig: tcfg, ReadHeaderTimeout: 5 * time.Second})
	<-ctx.Done()
	return nil
}

func genCerts(args []string) error {
	fs := flag.NewFlagSet("gen-certs", flag.ExitOnError)
	out := fs.String("out", "/certs", "output directory")
	names := fs.String("names", "monitor-idc-01,monitor-fc-01,witness", "comma-separated node names")
	days := fs.Int("days", 365, "certificate validity in days")
	fs.Parse(args)
	return certs.Generate(*out, strings.Split(*names, ","), time.Duration(*days)*24*time.Hour)
}

func runDemoSSH(args []string) error {
	fs := flag.NewFlagSet("demo-ssh", flag.ExitOnError)
	var o demotarget.SSHOptions
	fs.StringVar(&o.Addr, "addr", ":2222", "listen address")
	fs.StringVar(&o.Name, "name", "target-ssh", "host name used by the monitors")
	fs.StringVar(&o.User, "user", "svc", "login user")
	fs.StringVar(&o.HostKeyFile, "host-key", "/data/ssh_host_ed25519", "host key file (created if missing)")
	fs.StringVar(&o.KnownHostsOut, "known-hosts-out", "/shared/known_hosts", "write the host key here for the monitors")
	fs.Parse(args)
	o.Password = os.Getenv("DEMO_SSH_PASSWORD")
	return demotarget.RunSSH(o)
}

func runDemoTarget(args []string) error {
	fs := flag.NewFlagSet("demo-target", flag.ExitOnError)
	var o demotarget.Options
	fs.StringVar(&o.Name, "name", "demo", "site name")
	fs.StringVar(&o.Addr, "addr", ":8443", "listen address")
	fs.IntVar(&o.CertDays, "cert-days", 365, "certificate lifetime")
	fs.BoolVar(&o.LegacyTLS, "legacy-tls", false, "also accept TLS 1.0/1.1")
	fs.BoolVar(&o.NoTLS13, "no-tls13", false, "disable TLS 1.3")
	fs.Parse(args)
	return demotarget.Run(o)
}
