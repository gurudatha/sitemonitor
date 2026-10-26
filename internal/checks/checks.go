// Package checks implements the individual, time-bound website checks.
// Every check receives a context with a deadline and must return promptly.
package checks

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"sitemonitor/internal/config"
)

const (
	StatusOK   = "OK"
	StatusWarn = "WARN"
	StatusCrit = "CRIT"
)

// Size limits, set from node.yaml limits at startup (see Configure).
var (
	maxBody      int64 = 1 << 20 // bytes read from a site
	maxRedirects       = 5
)

// Configure applies limits.max_response_body_kb and limits.max_redirects.
func Configure(l config.Limits) {
	maxBody = l.MaxResponseBodyKB << 10
	maxRedirects = l.MaxRedirects
}

// Result is one execution of one check against one site.
type Result struct {
	ID         string            `json:"id"` // node-sequence, used to de-duplicate replicated results
	Site       string            `json:"site"`
	Region     string            `json:"region"`      // region the site belongs to
	Check      string            `json:"check"`       // availability | tls | cert | headers | api
	Node       string            `json:"node"`        // server that ran the check
	NodeRegion string            `json:"node_region"` // region of that server
	Status     string            `json:"status"`
	Message    string            `json:"message"`
	LatencyMs  int64             `json:"latency_ms"`
	Details    map[string]string `json:"details,omitempty"`
	Time       time.Time         `json:"time"`
}

func worse(a, b string) string {
	rank := map[string]int{StatusOK: 0, StatusWarn: 1, StatusCrit: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// Run executes a single check. The caller sets the deadline on ctx.
func Run(ctx context.Context, sites *config.Sites, site *config.Site, check string) Result {
	start := time.Now()
	r := Result{Site: site.Name, Region: site.Region, Check: check, Status: StatusOK, Details: map[string]string{}, Time: start}
	switch check {
	case config.CheckAvailability:
		availability(ctx, sites, site, &r)
	case config.CheckTLS:
		tlsVersions(ctx, site, &r)
	case config.CheckCert:
		certificate(ctx, sites, site, &r)
	case config.CheckHeaders:
		headers(ctx, sites, site, &r)
	case config.CheckAPI:
		apiSanity(ctx, sites, site, &r)
	default:
		r.Status, r.Message = StatusCrit, "unknown check "+check
	}
	if r.LatencyMs == 0 {
		r.LatencyMs = time.Since(start).Milliseconds()
	}
	if ctx.Err() == context.DeadlineExceeded && r.Status == StatusOK {
		r.Status, r.Message = StatusCrit, "timed out"
	}
	return r
}

func hostPort(u *url.URL) (host, addr string) {
	host = u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return host, net.JoinHostPort(host, port)
}

func httpClient(site *config.Site) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DisableKeepAlives:   true, // measure a fresh connection every time; no idle sockets left on targets
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !site.Verify()},
			TLSHandshakeTimeout: site.Timeout,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects (%d)", len(via))
			}
			return nil
		},
	}
}

func newRequest(ctx context.Context, sites *config.Sites, method, rawURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", sites.Defaults.UserAgent)
	return req, nil
}

// ---------- availability: is the server up, how fast, right content ----------

func availability(ctx context.Context, sites *config.Sites, site *config.Site, r *Result) {
	var dnsStart, connStart, tlsStart, reqStart time.Time
	var dnsMs, connMs, tlsMs, ttfbMs int64
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { dnsMs = time.Since(dnsStart).Milliseconds() },
		ConnectStart:         func(_, _ string) { connStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { connMs = time.Since(connStart).Milliseconds() },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { tlsMs = time.Since(tlsStart).Milliseconds() },
		GotFirstResponseByte: func() { ttfbMs = time.Since(reqStart).Milliseconds() },
	}
	req, err := newRequest(httptrace.WithClientTrace(ctx, trace), sites, http.MethodGet, site.URL, nil)
	if err != nil {
		r.Status, r.Message = StatusCrit, err.Error()
		return
	}
	reqStart = time.Now()
	resp, err := httpClient(site).Do(req)
	if err != nil {
		r.Status, r.Message = StatusCrit, "DOWN: "+err.Error()
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	r.LatencyMs = time.Since(reqStart).Milliseconds()

	r.Details["http_status"] = fmt.Sprint(resp.StatusCode)
	r.Details["dns_ms"] = fmt.Sprint(dnsMs)
	r.Details["connect_ms"] = fmt.Sprint(connMs)
	r.Details["tls_ms"] = fmt.Sprint(tlsMs)
	r.Details["ttfb_ms"] = fmt.Sprint(ttfbMs)
	r.Details["bytes"] = fmt.Sprint(len(body))

	want := site.Expect.Status
	okStatus := (want == 0 && resp.StatusCode < 400) || (want != 0 && resp.StatusCode == want)
	switch {
	case !okStatus:
		r.Status, r.Message = StatusCrit, fmt.Sprintf("DOWN: HTTP %d", resp.StatusCode)
	case site.Expect.Contains != "" && !strings.Contains(string(body), site.Expect.Contains):
		r.Status, r.Message = StatusCrit, fmt.Sprintf("content check failed: %q not found", site.Expect.Contains)
	case site.Expect.MaxLatencyMs > 0 && r.LatencyMs > site.Expect.MaxLatencyMs:
		r.Status, r.Message = StatusWarn, fmt.Sprintf("slow: %dms > %dms", r.LatencyMs, site.Expect.MaxLatencyMs)
	default:
		r.Message = fmt.Sprintf("UP: HTTP %d in %dms", resp.StatusCode, r.LatencyMs)
	}
}

// ---------- tls: legacy protocols must be off, TLS 1.2/1.3 on, no weak ciphers ----------

var versionNames = []struct {
	v    uint16
	name string
}{
	{tls.VersionTLS10, "TLS1.0"},
	{tls.VersionTLS11, "TLS1.1"},
	{tls.VersionTLS12, "TLS1.2"},
	{tls.VersionTLS13, "TLS1.3"},
}

func allSuites() []uint16 {
	var ids []uint16
	for _, c := range tls.CipherSuites() {
		ids = append(ids, c.ID)
	}
	for _, c := range tls.InsecureCipherSuites() {
		ids = append(ids, c.ID)
	}
	return ids
}

// weakSuites: Go's "insecure" list plus static-RSA key exchange and CBC suites.
func weakSuites() []uint16 {
	var ids []uint16
	for _, c := range tls.InsecureCipherSuites() {
		ids = append(ids, c.ID)
	}
	for _, c := range tls.CipherSuites() {
		if strings.HasPrefix(c.Name, "TLS_RSA_") || strings.Contains(c.Name, "_CBC_") {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// handshake returns (accepted, reachable, negotiated suite name).
func handshake(ctx context.Context, addr, host string, cfg *tls.Config) (bool, bool, string) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false, false, ""
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cfg.ServerName = host
	cfg.InsecureSkipVerify = true // protocol probe only; the cert check verifies the chain
	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return false, true, ""
	}
	return true, true, tls.CipherSuiteName(tc.ConnectionState().CipherSuite)
}

func tlsVersions(ctx context.Context, site *config.Site, r *Result) {
	u, err := url.Parse(site.URL)
	if err != nil || u.Scheme != "https" {
		r.Status, r.Message = StatusWarn, "site is not HTTPS"
		return
	}
	host, addr := hostPort(u)
	enabled := map[string]bool{}
	reachable := false
	for _, v := range versionNames {
		ok, reach, _ := handshake(ctx, addr, host, &tls.Config{MinVersion: v.v, MaxVersion: v.v, CipherSuites: allSuites()})
		reachable = reachable || reach
		enabled[v.name] = ok
		r.Details[v.name] = map[bool]string{true: "enabled", false: "disabled"}[ok]
	}
	r.Details["SSLv3"] = "not testable (unsupported by client library)"
	if !reachable {
		r.Status, r.Message = StatusCrit, "unreachable: no TCP connection"
		return
	}

	weakOK, _, weakName := handshake(ctx, addr, host, &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS12, CipherSuites: weakSuites()})
	if weakOK {
		r.Details["weak_cipher"] = weakName
	} else {
		r.Details["weak_cipher"] = "none accepted"
	}

	var problems []string
	if enabled["TLS1.0"] || enabled["TLS1.1"] {
		r.Status = worse(r.Status, StatusCrit)
		problems = append(problems, "legacy TLS1.0/1.1 enabled")
	}
	switch {
	case !enabled["TLS1.2"] && !enabled["TLS1.3"]:
		r.Status = worse(r.Status, StatusCrit)
		problems = append(problems, "neither TLS1.2 nor TLS1.3 enabled")
	case !enabled["TLS1.3"]:
		r.Status = worse(r.Status, StatusWarn)
		problems = append(problems, "TLS1.3 not enabled")
	case !enabled["TLS1.2"]:
		r.Status = worse(r.Status, StatusWarn)
		problems = append(problems, "TLS1.2 not enabled (older clients)")
	}
	if weakOK {
		r.Status = worse(r.Status, StatusWarn)
		problems = append(problems, "weak cipher accepted: "+weakName)
	}
	if len(problems) == 0 {
		r.Message = "compliant: TLS1.0/1.1 off, TLS1.2/1.3 on, no weak ciphers"
	} else {
		r.Message = strings.Join(problems, "; ")
	}
}

// ---------- cert: expiry within N days, chain + hostname validity ----------

func certificate(ctx context.Context, sites *config.Sites, site *config.Site, r *Result) {
	u, err := url.Parse(site.URL)
	if err != nil || u.Scheme != "https" {
		r.Status, r.Message = StatusWarn, "site is not HTTPS"
		return
	}
	host, addr := hostPort(u)
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		r.Status, r.Message = StatusCrit, "unreachable: "+err.Error()
		return
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true})
	if err := tc.HandshakeContext(ctx); err != nil {
		r.Status, r.Message = StatusCrit, "TLS handshake failed: "+err.Error()
		return
	}
	state := tc.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		r.Status, r.Message = StatusCrit, "no certificate presented"
		return
	}
	leaf := state.PeerCertificates[0]
	daysLeft := int(time.Until(leaf.NotAfter).Hours() / 24)
	fp := sha256.Sum256(leaf.Raw)
	r.Details["subject"] = leaf.Subject.CommonName
	r.Details["issuer"] = leaf.Issuer.CommonName
	r.Details["not_after"] = leaf.NotAfter.UTC().Format(time.RFC3339)
	r.Details["days_left"] = fmt.Sprint(daysLeft)
	r.Details["sha256"] = hex.EncodeToString(fp[:])
	r.Details["ocsp_stapled"] = fmt.Sprint(len(state.OCSPResponse) > 0)

	// Chain + hostname verification against the system trust store.
	inter := x509.NewCertPool()
	for _, c := range state.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, verr := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter})
	if verr != nil {
		r.Details["chain"] = "INVALID: " + verr.Error()
	} else {
		r.Details["chain"] = "valid"
	}

	warnDays, critDays := sites.Defaults.CertWarnDays, sites.Defaults.CertCritDays
	switch {
	case daysLeft < 0:
		r.Status, r.Message = StatusCrit, fmt.Sprintf("EXPIRED %d days ago", -daysLeft)
	case daysLeft <= critDays:
		r.Status, r.Message = StatusCrit, fmt.Sprintf("expires in %d days (<= %d)", daysLeft, critDays)
	case daysLeft <= warnDays:
		r.Status, r.Message = StatusWarn, fmt.Sprintf("expires in %d days (<= %d)", daysLeft, warnDays)
	default:
		r.Message = fmt.Sprintf("valid for %d more days", daysLeft)
	}
	if verr != nil && site.Verify() {
		r.Status = StatusCrit
		r.Message += "; chain invalid: " + verr.Error()
	}
}

// ---------- headers: security headers and information leakage ----------

func headers(ctx context.Context, sites *config.Sites, site *config.Site, r *Result) {
	req, err := newRequest(ctx, sites, http.MethodGet, site.URL, nil)
	if err != nil {
		r.Status, r.Message = StatusCrit, err.Error()
		return
	}
	resp, err := httpClient(site).Do(req)
	if err != nil {
		r.Status, r.Message = StatusCrit, "DOWN: "+err.Error()
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()

	h := resp.Header
	var missing []string
	need := map[string]bool{
		"Strict-Transport-Security": strings.HasPrefix(site.URL, "https://"),
		"Content-Security-Policy":   true,
		"X-Content-Type-Options":    true,
		"Referrer-Policy":           true,
	}
	for name, required := range need {
		if required && h.Get(name) == "" {
			missing = append(missing, name)
		}
	}
	if h.Get("X-Frame-Options") == "" && !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors") {
		missing = append(missing, "X-Frame-Options/frame-ancestors")
	}
	var issues []string
	if len(missing) > 0 {
		r.Details["missing"] = strings.Join(missing, ", ")
		issues = append(issues, fmt.Sprintf("%d missing security headers", len(missing)))
	}
	for _, name := range []string{"Server", "X-Powered-By"} {
		if v := h.Get(name); v != "" && strings.ContainsAny(v, "0123456789") {
			r.Details["leak_"+name] = v
			issues = append(issues, name+" header leaks version")
		}
	}
	for _, c := range resp.Cookies() {
		if !c.Secure || !c.HttpOnly {
			r.Details["cookie_"+c.Name] = "missing Secure/HttpOnly"
			issues = append(issues, "cookie "+c.Name+" not Secure/HttpOnly")
		}
	}
	if len(issues) > 0 {
		r.Status, r.Message = StatusWarn, strings.Join(issues, "; ")
	} else {
		r.Message = "all recommended security headers present"
	}
}

// ---------- api: read-only sanity calls with simple assertions ----------

func apiSanity(ctx context.Context, sites *config.Sites, site *config.Site, r *Result) {
	if len(site.API) == 0 {
		r.Status, r.Message = StatusWarn, "api check enabled but no api calls configured"
		return
	}
	base := strings.TrimRight(site.URL, "/")
	client := httpClient(site)
	var failed []string
	for _, call := range site.API {
		method := call.Method
		if method == "" {
			method = http.MethodGet
		}
		var body io.Reader
		if call.Body != "" {
			body = strings.NewReader(config.ExpandEnv(call.Body))
		}
		req, err := newRequest(ctx, sites, method, base+call.Path, body)
		if err != nil {
			failed = append(failed, call.Name+": "+err.Error())
			continue
		}
		for k, v := range call.Headers {
			req.Header.Set(k, config.ExpandEnv(v)) // secrets come from env, never from the yaml
		}
		t0 := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			failed = append(failed, call.Name+": "+err.Error())
			r.Details[call.Name] = "error"
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		ms := time.Since(t0).Milliseconds()

		if msg := assertAPI(call.Expect, resp.StatusCode, data); msg != "" {
			failed = append(failed, call.Name+": "+msg)
			r.Details[call.Name] = fmt.Sprintf("FAIL (%dms) %s", ms, msg)
		} else {
			r.Details[call.Name] = fmt.Sprintf("OK (%dms)", ms)
		}
	}
	if len(failed) > 0 {
		r.Status = StatusCrit
		r.Message = fmt.Sprintf("%d/%d API calls failed: %s", len(failed), len(site.API), strings.Join(failed, "; "))
	} else {
		r.Message = fmt.Sprintf("%d/%d API calls passed", len(site.API), len(site.API))
	}
}

func assertAPI(e config.APIExpect, status int, body []byte) string {
	want := e.Status
	if want == 0 {
		want = 200
	}
	if status != want {
		return fmt.Sprintf("HTTP %d, want %d", status, want)
	}
	if e.JSONPath == "" {
		return ""
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "response is not JSON"
	}
	v, ok := lookup(doc, e.JSONPath)
	if !ok {
		if e.Exists || e.Equals != "" {
			return e.JSONPath + " missing"
		}
		return ""
	}
	if e.Equals != "" && fmt.Sprint(v) != e.Equals {
		return fmt.Sprintf("%s = %v, want %s", e.JSONPath, v, e.Equals)
	}
	return ""
}

// lookup resolves a dotted path like "$.data.status".
func lookup(doc any, path string) (any, bool) {
	path = strings.TrimPrefix(strings.TrimPrefix(path, "$"), ".")
	cur := doc
	if path == "" {
		return cur, true
	}
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}
