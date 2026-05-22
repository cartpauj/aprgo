// Package tlscert load-or-generates aprgo's self-signed TLS certificate.
//
// First call creates an ECDSA P-256 cert + key under <dir>/{cert.pem,key.pem}
// (key mode 0600) with SANs for localhost, 127.0.0.1, ::1, and the host's
// reported hostname. Subsequent calls load the existing files. Regenerate()
// wipes both and creates a fresh pair — exposed so an operator who moves the
// box to a new hostname can rotate the cert from the UI or a flag.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	certName = "cert.pem"
	keyName  = "key.pem"
	// 10 years — self-signed certs don't get revocation infrastructure
	// so we lean on a long expiry rather than a fragile renew loop.
	validFor = 10 * 365 * 24 * time.Hour
)

// LoadOrGenerate returns a usable tls.Certificate, generating and persisting
// a fresh self-signed pair under dir if one doesn't already exist there.
// The returned fingerprint is the SHA-256 of the DER cert, lowercase hex with
// no separators — log it so operators can verify out-of-band over SSH.
func LoadOrGenerate(dir string) (tls.Certificate, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", fmt.Errorf("mkdir tls dir: %w", err)
	}
	certPath := filepath.Join(dir, certName)
	keyPath := filepath.Join(dir, keyName)
	if _, err := os.Stat(certPath); err == nil {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("load tls keypair: %w", err)
		}
		fp := fingerprint(cert)
		return cert, fp, nil
	} else if !os.IsNotExist(err) {
		return tls.Certificate{}, "", err
	}
	return generate(dir)
}

// Regenerate removes any existing pair and creates a fresh one. Used by
// the --regen-tls flag and (eventually) a Settings button.
func Regenerate(dir string) (tls.Certificate, string, error) {
	_ = os.Remove(filepath.Join(dir, certName))
	_ = os.Remove(filepath.Join(dir, keyName))
	return generate(dir)
}

func generate(dir string) (tls.Certificate, string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate ec key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("serial: %w", err)
	}

	hostname, _ := os.Hostname()
	dns := []string{"localhost"}
	if hostname != "" && hostname != "localhost" {
		dns = append(dns, hostname)
	}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "aprgo",
			Organization: []string{"aprgo self-signed"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("create cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})

	if err := writeFile(filepath.Join(dir, certName), certPEM, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := writeFile(filepath.Join(dir, keyName), keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("reparse cert: %w", err)
	}
	return cert, fingerprintDER(der), nil
}

func fingerprint(c tls.Certificate) string {
	if len(c.Certificate) == 0 {
		return ""
	}
	return fingerprintDER(c.Certificate[0])
}

func fingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(sum)*2)
	for i, b := range sum {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0x0f]
	}
	return string(out)
}

// writeFile commits `data` to `path` via temp+fsync+rename so an
// interrupted write (power loss, kernel crash) can never leave a
// half-written cert or key on disk. Matches the pattern used by
// state.save() and config.save().
func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// fsync the directory so the rename itself is durable.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
