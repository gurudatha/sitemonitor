// Package certs generates a private CA + node certificates for mTLS between
// the monitors and the witness, and builds the TLS configs that use them.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// Generate creates dir/ca.{crt,key} and dir/<name>.{crt,key} for each name.
// Existing files are kept, so it is safe to run on every start.
func Generate(dir string, names []string, validity time.Duration) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	caCrt, caKeyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	var ca *x509.Certificate
	var caKey *ecdsa.PrivateKey

	if _, err := os.Stat(caCrt); errors.Is(err, os.ErrNotExist) {
		caKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: "sitemonitor-ca"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(validity * 2),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
		if err != nil {
			return err
		}
		kb, _ := x509.MarshalECPrivateKey(caKey)
		if err := writePEM(caKeyPath, "EC PRIVATE KEY", kb, 0o600); err != nil {
			return err
		}
		if err := writePEM(caCrt, "CERTIFICATE", der, 0o644); err != nil {
			return err
		}
		ca, _ = x509.ParseCertificate(der)
		fmt.Println("created", caCrt)
	} else {
		pair, err := tls.LoadX509KeyPair(caCrt, caKeyPath)
		if err != nil {
			return err
		}
		ca, _ = x509.ParseCertificate(pair.Certificate[0])
		caKey = pair.PrivateKey.(*ecdsa.PrivateKey)
	}

	for _, name := range names {
		crt := filepath.Join(dir, name+".crt")
		if _, err := os.Stat(crt); err == nil {
			continue
		}
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: serial(), Subject: pkix.Name{CommonName: name},
			DNSNames:    []string{name, "localhost"},
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
			NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(validity),
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			return err
		}
		kb, _ := x509.MarshalECPrivateKey(key)
		if err := writePEM(filepath.Join(dir, name+".key"), "EC PRIVATE KEY", kb, 0o600); err != nil {
			return err
		}
		if err := writePEM(crt, "CERTIFICATE", der, 0o644); err != nil {
			return err
		}
		fmt.Println("created", crt)
	}
	return nil
}

func loadCA(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("no certificates in %s", path)
	}
	return pool, nil
}

// ServerTLS: TLS 1.3 only. requireClient=true enforces mTLS (peer and witness ports).
func ServerTLS(certFile, keyFile, caFile string, requireClient bool) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	if requireClient {
		pool, err := loadCA(caFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// MTLSClient returns an HTTP client that presents our cert and trusts only our CA.
func MTLSClient(certFile, keyFile, caFile string) (*http.Client, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	pool, err := loadCA(caFile)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{pair}},
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}, nil
}

// ClientCN returns the verified client certificate's common name.
func ClientCN(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		return ""
	}
	return r.TLS.VerifiedChains[0][0].Subject.CommonName
}
