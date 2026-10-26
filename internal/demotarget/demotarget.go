// Package demotarget is a fake website used only by docker-compose so the
// whole system can be tried offline. Each instance can be configured to be
// healthy or to have specific problems (expiring cert, legacy TLS, ...).
package demotarget

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"sync/atomic"
	"time"
)

type Options struct {
	Name      string
	Addr      string
	CertDays  int  // certificate lifetime from now
	LegacyTLS bool // also accept TLS 1.0/1.1 (should be flagged CRIT)
	NoTLS13   bool // cap at TLS 1.2 (should be flagged WARN)
}

func selfSigned(name string, days int) (tls.Certificate, error) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		DNSNames:  []string{name, "localhost"},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(time.Duration(days) * 24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func Run(o Options) error {
	cert, err := selfSigned(o.Name, o.CertDays)
	if err != nil {
		return err
	}
	var down atomic.Bool

	mux := http.NewServeMux()
	// POST /admin/toggle flips the site between UP and DOWN (HTTP 503) for demos.
	mux.HandleFunc("POST /admin/toggle", func(w http.ResponseWriter, _ *http.Request) {
		v := !down.Load()
		down.Store(v)
		fmt.Fprintf(w, "%s down=%v\n", o.Name, v)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "UP", "service": o.Name})
		case "/api/v1/items":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"count": 3, "items": []string{"a", "b", "c"}})
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<!doctype html><title>%s</title><h1>Welcome to %s</h1>", o.Name, o.Name)
		}
	})
	secure := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !o.LegacyTLS { // the "badly configured" targets also omit security headers
			h := w.Header()
			h.Set("Strict-Transport-Security", "max-age=31536000")
			h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
		} else {
			w.Header().Set("Server", "Apache/2.2.15")
		}
		mux.ServeHTTP(w, r)
	})

	tcfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12,
		CipherSuites: []uint16{ // AEAD + forward secrecy only (TLS 1.2; TLS 1.3 suites are fixed)
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		}}
	if o.LegacyTLS {
		tcfg.MinVersion = tls.VersionTLS10
		var all []uint16
		for _, c := range append(tls.CipherSuites(), tls.InsecureCipherSuites()...) {
			all = append(all, c.ID)
		}
		tcfg.CipherSuites = all
	}
	if o.NoTLS13 {
		tcfg.MaxVersion = tls.VersionTLS12
	}
	srv := &http.Server{Addr: o.Addr, Handler: secure, TLSConfig: tcfg, ReadHeaderTimeout: 5 * time.Second}
	slog.Info("demo target listening", "name", o.Name, "addr", o.Addr, "cert_days", o.CertDays, "legacy_tls", o.LegacyTLS)
	return srv.ListenAndServeTLS("", "")
}
