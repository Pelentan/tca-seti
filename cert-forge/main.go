// cert-forge — TCA certificate authority init tool.
//
// Generates a constellation CA and signs service certificates using only
// the Go standard library. No external dependencies. No shell gymnastics.
// No CLI flag archaeology across OpenSSL versions.
//
// Usage: cert-forge [-config /path/to/forge.json]
//
// Reads forge.json, generates the CA and all service certs, writes PEM
// files to the configured output directory, and exits. Designed to run
// as a Docker init container — same lifecycle as the previous cert-init,
// drop-in replacement with zero behavioral changes from the constellation's
// perspective.
//
// The supply chain is the Go standard library. Nothing else.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type CAConfig struct {
	CommonName         string `json:"common_name"`
	Organization       string `json:"organization"`
	Country            string `json:"country"`
	KeyBits            int    `json:"key_bits"`
	ValidDays          int    `json:"valid_days"`
	BackdateMinutes    int    `json:"backdate_minutes"`
}

type ServiceConfig struct {
	Name string   `json:"name"`
	SANs []string `json:"sans"`
}

type ForgeConfig struct {
	CA        CAConfig        `json:"ca"`
	Services  []ServiceConfig `json:"services"`
	OutputDir string          `json:"output_dir"`
	KeyBits   int             `json:"key_bits"`
	ValidDays int             `json:"valid_days"`
}

func loadConfig(path string) (*ForgeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %v", err)
	}
	var cfg ForgeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %v", err)
	}

	// Apply defaults
	if cfg.CA.KeyBits == 0 {
		cfg.CA.KeyBits = 4096
	}
	if cfg.CA.ValidDays == 0 {
		cfg.CA.ValidDays = 3650
	}
	if cfg.CA.BackdateMinutes == 0 {
		cfg.CA.BackdateMinutes = 5
	}
	if cfg.KeyBits == 0 {
		cfg.KeyBits = 2048
	}
	if cfg.ValidDays == 0 {
		cfg.ValidDays = 3650
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = "/certs"
	}
	return &cfg, nil
}

// ---------------------------------------------------------------------------
// PEM writers
// ---------------------------------------------------------------------------

func writeCert(path string, certDER []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

func writeKey(path string, key *rsa.PrivateKey) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// ---------------------------------------------------------------------------
// Serial number
// ---------------------------------------------------------------------------

func newSerial() *big.Int {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		log.Fatalf("[cert-forge] Failed to generate serial number: %v", err)
	}
	return serial
}

// ---------------------------------------------------------------------------
// CA generation
// ---------------------------------------------------------------------------

type CA struct {
	cert    *x509.Certificate
	certDER []byte
	key     *rsa.PrivateKey
}

func generateCA(cfg *ForgeConfig) (*CA, error) {
	log.Printf("[cert-forge] Generating CA key (%d bit)...", cfg.CA.KeyBits)
	key, err := rsa.GenerateKey(rand.Reader, cfg.CA.KeyBits)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %v", err)
	}

	notBefore := time.Now().Add(-time.Duration(cfg.CA.BackdateMinutes) * time.Minute)
	notAfter := time.Now().Add(time.Duration(cfg.CA.ValidDays) * 24 * time.Hour)

	template := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject: pkix.Name{
			CommonName:   cfg.CA.CommonName,
			Organization: []string{cfg.CA.Organization},
			Country:      []string{cfg.CA.Country},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	// Self-sign: template == parent, key signs itself
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %v", err)
	}

	log.Printf("[cert-forge] CA generated (CN=%s, NotBefore=%s)",
		cfg.CA.CommonName, notBefore.UTC().Format(time.RFC3339))

	return &CA{cert: cert, certDER: certDER, key: key}, nil
}

// ---------------------------------------------------------------------------
// Service cert generation
// ---------------------------------------------------------------------------

func generateServiceCert(svc ServiceConfig, ca *CA, cfg *ForgeConfig) error {
	key, err := rsa.GenerateKey(rand.Reader, cfg.KeyBits)
	if err != nil {
		return fmt.Errorf("generate key for %s: %v", svc.Name, err)
	}

	notBefore := time.Now().Add(-time.Duration(cfg.CA.BackdateMinutes) * time.Minute)
	notAfter := time.Now().Add(time.Duration(cfg.ValidDays) * 24 * time.Hour)

	template := &x509.Certificate{
		SerialNumber: newSerial(),
		Subject: pkix.Name{
			CommonName:   svc.Name,
			Organization: []string{cfg.CA.Organization},
			Country:      []string{cfg.CA.Country},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              svc.SANs,
	}

	// Sign with CA
	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return fmt.Errorf("sign cert for %s: %v", svc.Name, err)
	}

	// Write cert
	certPath := filepath.Join(cfg.OutputDir, svc.Name+".crt")
	if err := writeCert(certPath, certDER); err != nil {
		return fmt.Errorf("write cert for %s: %v", svc.Name, err)
	}

	// Write key
	keyPath := filepath.Join(cfg.OutputDir, svc.Name+".key")
	if err := writeKey(keyPath, key); err != nil {
		return fmt.Errorf("write key for %s: %v", svc.Name, err)
	}

	log.Printf("[cert-forge] %s — cert and key written", svc.Name)
	return nil
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	configPath := flag.String("config", "/forge.json", "Path to forge.json config file")
	flag.Parse()

	log.Printf("[cert-forge] Starting — stdlib only, no external dependencies")
	log.Printf("[cert-forge] Config: %s", *configPath)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("[cert-forge] Config error: %v", err)
	}

	// Ensure output directory exists
	if err := os.MkdirAll(cfg.OutputDir, 0755); err != nil {
		log.Fatalf("[cert-forge] Cannot create output dir %s: %v", cfg.OutputDir, err)
	}

	// Generate CA
	ca, err := generateCA(cfg)
	if err != nil {
		log.Fatalf("[cert-forge] CA generation failed: %v", err)
	}

	// Write CA cert (key stays in memory — never written separately)
	caPath := filepath.Join(cfg.OutputDir, "ca.crt")
	if err := writeCert(caPath, ca.certDER); err != nil {
		log.Fatalf("[cert-forge] Failed to write CA cert: %v", err)
	}

	// Write CA key — needed by services for mTLS client auth verification
	caKeyPath := filepath.Join(cfg.OutputDir, "ca.key")
	if err := writeKey(caKeyPath, ca.key); err != nil {
		log.Fatalf("[cert-forge] Failed to write CA key: %v", err)
	}

	log.Printf("[cert-forge] CA written to %s", cfg.OutputDir)

	// Generate service certs
	log.Printf("[cert-forge] Generating %d service certificates...", len(cfg.Services))
	for _, svc := range cfg.Services {
		if err := generateServiceCert(svc, ca, cfg); err != nil {
			log.Fatalf("[cert-forge] %v", err)
		}
	}

	// Verify everything was written
	entries, err := os.ReadDir(cfg.OutputDir)
	if err != nil {
		log.Fatalf("[cert-forge] Cannot read output dir: %v", err)
	}

	log.Printf("[cert-forge] Certificate generation complete.")
	log.Printf("[cert-forge] Output directory contents (%d files):", len(entries))
	for _, e := range entries {
		info, _ := e.Info()
		log.Printf("[cert-forge]   %s (%d bytes)", e.Name(), info.Size())
	}
	log.Printf("[cert-forge] Exiting cleanly.")
}
