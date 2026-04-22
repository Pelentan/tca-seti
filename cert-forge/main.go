// cert-forge — TCA PKI abstraction layer.
//
// Starts as a running service (not an init container that exits).
// Manages all key material for the constellation — services never
// hold private keys, never perform cryptographic operations directly.
//
// Startup sequence:
//  1. Generate own key pair and self-signed bootstrap cert (in memory)
//  2. Start mTLS HTTP server using bootstrap cert
//  3. Generate constellation CA (in memory — CA key never written to disk)
//  4. Write CA public cert to volume (for bootstrap reference only)
//  5. Generate static certs (star-gazer, etc.) and write to volume
//  6. Mark ready — now accepting /instance-cert and /sign requests
//
// Services call /ca on startup to get the CA cert (unauthenticated),
// then /instance-cert to get their instance cert (mTLS required using
// the bootstrap cert they received at build time from the volume).
//
// Supply chain: Go standard library only. No external dependencies.
package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

var (
	port           = envOr("PORT", "3020")
	enrollmentPort = envOr("ENROLLMENT_PORT", "3021")
	outputDir      = envOr("CERTS_DIR", "/certs")
	configPath     = envOr("CONFIG_PATH", "/forge.json")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Config (forge.json — same format as before)
// ---------------------------------------------------------------------------

type CAConfig struct {
	CommonName      string `json:"common_name"`
	Organization    string `json:"organization"`
	Country         string `json:"country"`
	KeyBits         int    `json:"key_bits"`
	ValidDays       int    `json:"valid_days"`
	BackdateMinutes int    `json:"backdate_minutes"`
}

type StaticCertConfig struct {
	Name      string   `json:"name"`
	SANs      []string `json:"sans"`
	TLSSecret string   `json:"tls_secret,omitempty"` // if set, also write a kubernetes.io/tls Secret for Traefik
}

type RotationDeployment struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type RotationConfig struct {
	InstanceIntervalDays int                  `json:"instance_interval_days"`
	CAIntervalDays       int                  `json:"ca_interval_days"`
	CAOverlapHours       int                  `json:"ca_overlap_hours"`
	Strategy             string               `json:"strategy"`
	Deployments          []RotationDeployment `json:"deployments"`
}

type ForgeConfig struct {
	CA              CAConfig           `json:"ca"`
	StaticCerts     []StaticCertConfig `json:"services"`
	OutputDir       string             `json:"output_dir"`
	KeyBits         int                `json:"key_bits"`
	ValidDays       int                `json:"valid_days"`
	TraefikCASecret string             `json:"traefik_ca_secret,omitempty"`
	Rotation        RotationConfig     `json:"rotation"`
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
	if cfg.CA.KeyBits == 0 { cfg.CA.KeyBits = 4096 }
	if cfg.CA.ValidDays == 0 { cfg.CA.ValidDays = 3650 }
	if cfg.CA.BackdateMinutes == 0 { cfg.CA.BackdateMinutes = 5 }
	if cfg.KeyBits == 0 { cfg.KeyBits = 2048 }
	if cfg.ValidDays == 0 { cfg.ValidDays = 3650 }
	if cfg.OutputDir != "" { outputDir = cfg.OutputDir }
	if cfg.Rotation.InstanceIntervalDays == 0 { cfg.Rotation.InstanceIntervalDays = 30 }
	if cfg.Rotation.CAIntervalDays == 0 { cfg.Rotation.CAIntervalDays = 30 }
	if cfg.Rotation.CAOverlapHours == 0 { cfg.Rotation.CAOverlapHours = 24 }
	if cfg.Rotation.Strategy == "" { cfg.Rotation.Strategy = "simultaneous" }
	return &cfg, nil
}

// ---------------------------------------------------------------------------
// PKI state — held entirely in memory
// ---------------------------------------------------------------------------

type CA struct {
	cert    *x509.Certificate
	certDER []byte
	certPEM []byte
	key     *rsa.PrivateKey
}

// InstanceKey holds the key material for a single container instance.
type InstanceKey struct {
	key         *rsa.PrivateKey
	cert        *x509.Certificate
	certPEM     []byte
	keyPEM      []byte
	fingerprint string
	instanceCN  string
	validUntil  time.Time
}

var (
	ca           *CA
	caReady      bool
	caReadyMu    sync.RWMutex

	// instance key store: "service_name/instance_id" → InstanceKey
	instanceKeys   = map[string]*InstanceKey{}
	instanceKeysMu sync.RWMutex
	certsIssued    atomic.Int64

	startedAt = time.Now()
)

// ---------------------------------------------------------------------------
// Serial number
// ---------------------------------------------------------------------------

func newSerial() *big.Int {
	s, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		log.Fatalf("[cert-forge] serial generation failed: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// CA generation
// ---------------------------------------------------------------------------

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

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	log.Printf("[cert-forge] CA generated (CN=%s, NotBefore=%s)", cfg.CA.CommonName, notBefore.Format(time.RFC3339))
	return &CA{cert: cert, certDER: certDER, certPEM: certPEM, key: key}, nil
}

// ---------------------------------------------------------------------------
// Instance cert generation — called per service startup
// ---------------------------------------------------------------------------

// SubjectOverride allows callers to specify optional DN fields beyond
// CN/O/C which cert-forge derives from config. Any non-empty field here
// overrides the default. CN is always set — if override CN is empty,
// the standard "serviceName-instanceID" pattern is used.
type SubjectOverride struct {
	CommonName         string
	OrganizationalUnit string
	Locality           string
	Province           string
	SANs               []string
}

func issueInstanceCert(cfg *ForgeConfig, serviceName, instanceID string, override *SubjectOverride) (*InstanceKey, error) {
	caReadyMu.RLock()
	ready := caReady
	caReadyMu.RUnlock()
	if !ready {
		return nil, fmt.Errorf("CA not yet initialized")
	}

	key, err := rsa.GenerateKey(rand.Reader, cfg.KeyBits)
	if err != nil {
		return nil, fmt.Errorf("generate key: %v", err)
	}

	instanceCN := fmt.Sprintf("%s-%s", serviceName, instanceID)
	if override != nil && override.CommonName != "" {
		instanceCN = override.CommonName
	}

	subject := pkix.Name{
		CommonName:   instanceCN,
		Organization: []string{cfg.CA.Organization},
		Country:      []string{cfg.CA.Country},
	}
	if override != nil && override.OrganizationalUnit != "" {
		subject.OrganizationalUnit = []string{override.OrganizationalUnit}
	}
	if override != nil && override.Locality != "" {
		subject.Locality = []string{override.Locality}
	}
	if override != nil && override.Province != "" {
		subject.Province = []string{override.Province}
	}

	sans := []string{serviceName, instanceCN, "localhost"}
	if override != nil && len(override.SANs) > 0 {
		sans = override.SANs
	}

	notBefore := time.Now().Add(-time.Duration(cfg.CA.BackdateMinutes) * time.Minute)
	notAfter := time.Now().Add(time.Duration(cfg.ValidDays) * 24 * time.Hour)

	template := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               subject,
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              sans,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign cert: %v", err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(certDER))

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	ik := &InstanceKey{
		key:         key,
		cert:        cert,
		certPEM:     certPEM,
		keyPEM:      keyPEM,
		fingerprint: fingerprint,
		instanceCN:  instanceCN,
		validUntil:  notAfter,
	}

	storeKey := fmt.Sprintf("%s/%s", serviceName, instanceID)
	instanceKeysMu.Lock()
	instanceKeys[storeKey] = ik
	instanceKeysMu.Unlock()
	certsIssued.Add(1)

	log.Printf("[cert-forge] Instance cert issued: %s (fingerprint=%s...)", instanceCN, fingerprint[:8])
	return ik, nil
}

// ---------------------------------------------------------------------------
// Static cert generation — star-gazer and other fixed identities
// ---------------------------------------------------------------------------

// StaticCertMaterial holds the generated cert+key PEM for a static cert.
type StaticCertMaterial struct {
	Name    string
	CertPEM []byte
	KeyPEM  []byte
}

// generateStaticCert generates a static cert+key and returns the PEM material.
// It does not write files — the caller decides where the material goes
// (K8s Secret in Kubernetes, volume files in Docker Compose).
func generateStaticCert(cfg *ForgeConfig, svc StaticCertConfig) (*StaticCertMaterial, error) {
	key, err := rsa.GenerateKey(rand.Reader, cfg.KeyBits)
	if err != nil {
		return nil, fmt.Errorf("generate key for %s: %v", svc.Name, err)
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

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign cert for %s: %v", svc.Name, err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	log.Printf("[cert-forge] Static cert generated: %s", svc.Name)
	return &StaticCertMaterial{Name: svc.Name, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// writeStaticCertFiles writes a static cert+key to the output volume.
// Only called in Docker Compose mode.
func writeStaticCertFiles(mat *StaticCertMaterial) error {
	certPath := filepath.Join(outputDir, mat.Name+".crt")
	keyPath := filepath.Join(outputDir, mat.Name+".key")
	if err := os.WriteFile(certPath, mat.CertPEM, 0644); err != nil {
		return fmt.Errorf("write cert for %s: %v", mat.Name, err)
	}
	if err := os.WriteFile(keyPath, mat.KeyPEM, 0600); err != nil {
		return fmt.Errorf("write key for %s: %v", mat.Name, err)
	}
	log.Printf("[cert-forge] Static cert written to volume: %s", mat.Name)
	return nil
}

// ---------------------------------------------------------------------------
// PEM writer
// ---------------------------------------------------------------------------

func writePEM(path, blockType string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: data})
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func handleCA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	caReadyMu.RLock()
	ready := caReady
	caReadyMu.RUnlock()
	if !ready {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "CA not yet initialized"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"ca_cert":     string(ca.certPEM),
		"common_name": ca.cert.Subject.CommonName,
	})
}

func handleInstanceCert(cfg *ForgeConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			ServiceName        string   `json:"service_name"`
			InstanceID         string   `json:"instance_id"`
			CommonName         string   `json:"common_name,omitempty"`
			OrganizationalUnit string   `json:"organizational_unit,omitempty"`
			Locality           string   `json:"locality,omitempty"`
			Province           string   `json:"province,omitempty"`
			SANs               []string `json:"sans,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if req.ServiceName == "" || req.InstanceID == "" {
			http.Error(w, `{"error":"service_name and instance_id required"}`, http.StatusBadRequest)
			return
		}

		var override *SubjectOverride
		if req.CommonName != "" || req.OrganizationalUnit != "" || req.Locality != "" || req.Province != "" || len(req.SANs) > 0 {
			override = &SubjectOverride{
				CommonName:         req.CommonName,
				OrganizationalUnit: req.OrganizationalUnit,
				Locality:           req.Locality,
				Province:           req.Province,
				SANs:               req.SANs,
			}
		}

		ik, err := issueInstanceCert(cfg, req.ServiceName, req.InstanceID, override)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"cert":        string(ik.certPEM),
			"key":         string(ik.keyPEM),
			"fingerprint": ik.fingerprint,
			"instance_cn": ik.instanceCN,
			"valid_until": ik.validUntil.UTC().Format(time.RFC3339),
		})
	}
}

func handleSign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ServiceName string `json:"service_name"`
		InstanceID  string `json:"instance_id"`
		Payload     string `json:"payload"` // base64-encoded
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.ServiceName == "" || req.InstanceID == "" || req.Payload == "" {
		http.Error(w, `{"error":"service_name, instance_id, and payload required"}`, http.StatusBadRequest)
		return
	}

	// Verify caller identity matches claimed identity
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		callerCN := r.TLS.PeerCertificates[0].Subject.CommonName
		expectedCN := fmt.Sprintf("%s-%s", req.ServiceName, req.InstanceID)
		// Allow bootstrap cert (CN == service_name) on first call before instance cert is obtained
		if callerCN != expectedCN && callerCN != req.ServiceName {
			w.Header().Set("Content-Type", "application/json")
			log.Printf("[cert-forge] /sign: CN mismatch — caller=%q expected=%q service=%q", callerCN, expectedCN, req.ServiceName)
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("caller CN %q does not match claimed identity %q", callerCN, expectedCN),
			})
			return
		}
	}

	storeKey := fmt.Sprintf("%s/%s", req.ServiceName, req.InstanceID)
	instanceKeysMu.RLock()
	ik, ok := instanceKeys[storeKey]
	// Log all known keys for debugging when lookup fails
	var knownKeys []string
	for k := range instanceKeys {
		knownKeys = append(knownKeys, k)
	}
	instanceKeysMu.RUnlock()
	if !ok {
		log.Printf("[cert-forge] /sign: key not found for %q — known keys: %v", storeKey, knownKeys)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("no key on record for %s/%s — call /instance-cert first", req.ServiceName, req.InstanceID),
		})
		return
	}

	payloadBytes, err := base64.StdEncoding.DecodeString(req.Payload)
	if err != nil {
		http.Error(w, `{"error":"payload is not valid base64"}`, http.StatusBadRequest)
		return
	}

	hash := sha256.Sum256(payloadBytes)
	sig, err := rsa.SignPKCS1v15(rand.Reader, ik.key, crypto.SHA256, hash[:])
	if err != nil {
		log.Printf("[cert-forge] Signing failed for %s/%s: %v", req.ServiceName, req.InstanceID, err)
		http.Error(w, `{"error":"signing failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"signature":   base64.StdEncoding.EncodeToString(sig),
		"fingerprint": ik.fingerprint,
		"algorithm":   "RSASSA-PKCS1-v1_5-SHA256",
	})
}

// ---------------------------------------------------------------------------
// TLS — cert-forge uses its own instance cert once the CA is ready.
// Before that, it uses a temporary self-signed bootstrap cert.
// ---------------------------------------------------------------------------

// Hot-swap machinery removed — three-server design eliminates the need

// constellationCAPool and getConfigForClient removed — three-server design

func buildInstanceTLSConfig(ik *InstanceKey, clientCAs *x509.CertPool, requireClientCert bool) *tls.Config {
	tlsCert := tls.Certificate{
		Certificate: [][]byte{ik.cert.Raw},
		PrivateKey:  ik.key,
		Leaf:        ik.cert,
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS13,
	}
	if requireClientCert && clientCAs != nil {
		cfg.ClientCAs = clientCAs
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg
}

// ---------------------------------------------------------------------------
// Router — /ca is unauthenticated, everything else requires mTLS
// ---------------------------------------------------------------------------

// Three servers, three ports, one purpose each:
//  4016 — plain HTTP, /ca only (CA cert is public)
//  4015 — enrollment mTLS, /instance-cert only
//  4014 — constellation mTLS, /sign only

func buildPublicMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/ca", handleCA)
	return mux
}

func buildEnrollmentMux(cfg *ForgeConfig) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/instance-cert", handleInstanceCert(cfg))
	return mux
}

func buildSignMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/sign", handleSign)
	return mux
}

// requireClientCert removed — each server enforces at TLS level

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Rotation — instance cert rotation and CA rotation
// ---------------------------------------------------------------------------

// rollDeployments patches the restartedAt annotation on all listed Deployments.
// Only meaningful in Kubernetes mode — no-op in Docker Compose.
func rollDeployments(cfg *RotationConfig) {
	if cfg.Strategy != "simultaneous" {
		log.Printf("[cert-forge] rotation: strategy %q not yet implemented — falling back to simultaneous", cfg.Strategy)
	}
	for _, d := range cfg.Deployments {
		if err := PatchDeploymentRestart(d.Name, d.Namespace); err != nil {
			log.Printf("[cert-forge] rotation: failed to restart %s/%s: %v", d.Namespace, d.Name, err)
		}
	}
}

// startRotationLoop runs the instance cert and CA rotation tickers.
// Blocks forever — run in a goroutine.
func startRotationLoop(cfg *ForgeConfig, inK8s bool, secretName string) {
	if !inK8s {
		log.Printf("[cert-forge] rotation: not in Kubernetes — rotation loop inactive")
		return
	}
	if len(cfg.Rotation.Deployments) == 0 {
		log.Printf("[cert-forge] rotation: no deployments configured — rotation loop inactive")
		return
	}

	instanceTicker := time.NewTicker(time.Duration(cfg.Rotation.InstanceIntervalDays) * 24 * time.Hour)
	caTicker       := time.NewTicker(time.Duration(cfg.Rotation.CAIntervalDays) * 24 * time.Hour)
	defer instanceTicker.Stop()
	defer caTicker.Stop()

	log.Printf("[cert-forge] rotation: instance every %dd, CA every %dd, strategy=%s, deployments=%d",
		cfg.Rotation.InstanceIntervalDays,
		cfg.Rotation.CAIntervalDays,
		cfg.Rotation.Strategy,
		len(cfg.Rotation.Deployments),
	)

	for {
		select {
		case <-instanceTicker.C:
			log.Printf("[cert-forge] rotation: instance cert rotation firing")
			rollDeployments(&cfg.Rotation)
			log.Printf("[cert-forge] rotation: instance cert rotation complete")

		case <-caTicker.C:
			log.Printf("[cert-forge] rotation: CA rotation firing")

			// Generate new CA
			newCA, err := generateCA(cfg)
			if err != nil {
				log.Printf("[cert-forge] rotation: CA generation failed: %v — skipping rotation", err)
				continue
			}

			// Overlap window: new CA is live immediately for /instance-cert issuance.
			// Services that re-enroll during the overlap get certs signed by the new CA.
			// /ca returns the new CA cert so services can build trust pools against it.
			caReadyMu.Lock()
			oldCA := ca
			ca = newCA
			caReadyMu.Unlock()

			log.Printf("[cert-forge] rotation: new CA active (CN=%s), overlap window %dh",
				newCA.cert.Subject.CommonName, cfg.Rotation.CAOverlapHours)

			// Write new CA to K8s Secret so services can fetch it on restart.
			// Read existing secret and update ca.crt and ca.key in place —
			// enrollment material and static certs are preserved.
			existingData, err := ReadK8sSecret(secretName)
			if err != nil {
				log.Printf("[cert-forge] rotation: could not read existing secret: %v", err)
			} else if existingData != nil {
				existingData["ca.crt"] = newCA.certPEM
				existingData["ca.key"] = caKeyPEM(newCA.key)
				if err := WriteK8sSecret(secretName, existingData); err != nil {
					log.Printf("[cert-forge] rotation: failed to write new CA to secret: %v", err)
				}
			}

			// Update Traefik CA secret if configured.
			if cfg.TraefikCASecret != "" {
				if err := WriteK8sSecret(cfg.TraefikCASecret, map[string][]byte{
					"tls.ca": newCA.certPEM,
				}); err != nil {
					log.Printf("[cert-forge] rotation: failed to update Traefik CA secret: %v", err)
				}
			}

			// Overlap window — services fetch new CA and build trust pools.
			log.Printf("[cert-forge] rotation: entering CA overlap window (%dh) — old CA still valid",
				cfg.Rotation.CAOverlapHours)
			time.Sleep(time.Duration(cfg.Rotation.CAOverlapHours) * time.Hour)

			// Overlap window expired — old CA retired, roll all deployments.
			_ = oldCA // old CA is no longer referenced; GC will collect it
			log.Printf("[cert-forge] rotation: overlap window expired — rolling deployments")
			rollDeployments(&cfg.Rotation)
			log.Printf("[cert-forge] rotation: CA rotation complete")
		}
	}
}

func main() {
	log.Printf("[cert-forge] Starting — stdlib only, no external dependencies")

	inK8s := InKubernetes()
	if inK8s {
		log.Printf("[cert-forge] Kubernetes mode — cert material will be written to K8s Secret")
	} else {
		log.Printf("[cert-forge] Docker mode — cert material will be written to volume")
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("[cert-forge] Config error: %v", err)
	}
	log.Printf("[cert-forge] Rotation config: instance=%dd ca=%dd overlap=%dh strategy=%s deployments=%d",
		cfg.Rotation.InstanceIntervalDays,
		cfg.Rotation.CAIntervalDays,
		cfg.Rotation.CAOverlapHours,
		cfg.Rotation.Strategy,
		len(cfg.Rotation.Deployments),
	)

	// In Docker mode, ensure the output directory exists.
	// In K8s mode, there is no volume — skip directory creation.
	if !inK8s {
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			log.Fatalf("[cert-forge] Cannot create output dir: %v", err)
		}
	}

	// Phase 1: Establish CA — load existing from Secret or generate fresh.
	secretName := cfEnvOr("K8S_SECRET_NAME", "seti-certs")
	var generatedCA *CA
	freshCA := false
	if inK8s {
		existingCA, err := LoadExistingCA(secretName)
		if err != nil {
			log.Printf("[cert-forge] Warning: could not check for existing CA: %v — generating fresh", err)
		} else if existingCA != nil {
			generatedCA = existingCA
		}
	}
	if generatedCA == nil {
		var err error
		generatedCA, err = generateCA(cfg)
		if err != nil {
			log.Fatalf("[cert-forge] CA generation failed: %v", err)
		}
		freshCA = true
	}
	ca = generatedCA
	caReadyMu.Lock()
	caReady = true
	caReadyMu.Unlock()

	// Phase 2: Issue cert-forge's own instance cert (for sign + enrollment servers)
	selfID := os.Getenv("HOSTNAME")
	if selfID == "" {
		selfID = "local"
	}
	selfIK, err := issueInstanceCert(cfg, "cert-forge", selfID, nil)
	if err != nil {
		log.Fatalf("[cert-forge] Self cert issuance failed: %v", err)
	}
	log.Printf("[cert-forge] Instance cert issued: %s", selfIK.instanceCN)

	// Phase 3: Generate enrollment CA and cert
	generatedEnrollCA, err := generateEnrollmentCA(cfg)
	if err != nil {
		log.Fatalf("[cert-forge] Enrollment CA generation failed: %v", err)
	}
	enrollmentCA = generatedEnrollCA
	enrollCert, err := generateEnrollmentCert(cfg, enrollmentCA)
	if err != nil {
		log.Fatalf("[cert-forge] Enrollment cert generation failed: %v", err)
	}

	// Phase 4: Generate static certs (star-gazer, etc.)
	staticMaterials := make([]*StaticCertMaterial, 0, len(cfg.StaticCerts))
	for _, svc := range cfg.StaticCerts {
		mat, err := generateStaticCert(cfg, svc)
		if err != nil {
			log.Fatalf("[cert-forge] %v", err)
		}
		staticMaterials = append(staticMaterials, mat)
	}

	// Phase 4b: Symmetric secrets — postgres password and JWT secret.
	// Generated once on first startup, persisted in the K8s Secret or on the
	// certs volume. Loaded from storage on every subsequent startup so they
	// are stable across cert-forge restarts.
	postgresPassword, jwtSecret, err := loadOrGenerateSymmetricSecrets(inK8s, secretName)
	if err != nil {
		log.Fatalf("[cert-forge] Failed to establish symmetric secrets: %v", err)
	}

	// Phase 5: Write cert material — Secret (K8s) or files (Docker Compose)
	if inK8s {
		secretData := map[string][]byte{
			"ca.crt":            ca.certPEM,
			"ca.key":            caKeyPEM(ca.key),
			"enrollment-ca.crt": enrollmentCA.certPEM,
			"enrollment.crt":    enrollCert.certPEM,
			"enrollment.key":    enrollCert.keyPEM,
			"postgres-password": postgresPassword,
			"jwt-secret":        jwtSecret,
		}
		for _, mat := range staticMaterials {
			secretData[mat.Name+".crt"] = mat.CertPEM
			secretData[mat.Name+".key"] = mat.KeyPEM
		}
		if err := WriteK8sSecret(secretName, secretData); err != nil {
			log.Fatalf("[cert-forge] Failed to write K8s Secret: %v", err)
		}

		// If we generated a fresh CA, roll all constellation deployments so
		// they re-enroll and pick up the new CA cert. Without this, pods
		// started before the Secret was written would have stale trust pools.
		if freshCA && len(cfg.Rotation.Deployments) > 0 {
			log.Printf("[cert-forge] Fresh CA — rolling %d deployments to pick up new CA cert", len(cfg.Rotation.Deployments))
			// Small delay to ensure the Secret is fully propagated before pods restart
			time.Sleep(5 * time.Second)
			rollDeployments(&cfg.Rotation)
		}

		// Write kubernetes.io/tls Secrets for any static cert that declared tls_secret.
		// Traefik requires this type for TLS termination — it will not read Opaque secrets.
		for i, svc := range cfg.StaticCerts {
			if svc.TLSSecret == "" {
				continue
			}
			mat := staticMaterials[i]
			if err := WriteTLSSecret(svc.TLSSecret, mat.CertPEM, mat.KeyPEM); err != nil {
				log.Fatalf("[cert-forge] Failed to write TLS Secret for %s: %v", svc.Name, err)
			}
		}

		// Write the CA cert as tls.ca for Traefik ServersTransport backend verification.
		if cfg.TraefikCASecret != "" {
			if err := WriteK8sSecret(cfg.TraefikCASecret, map[string][]byte{
				"tls.ca": ca.certPEM,
			}); err != nil {
				log.Fatalf("[cert-forge] Failed to write Traefik CA Secret: %v", err)
			}
		}
	} else {
		// Docker Compose: write all material to the certs volume.
		caPath := filepath.Join(outputDir, "ca.crt")
		if err := writePEM(caPath, "CERTIFICATE", ca.certDER); err != nil {
			log.Fatalf("[cert-forge] Failed to write CA cert: %v", err)
		}
		log.Printf("[cert-forge] CA cert written to %s", caPath)

		if err := writeEnrollmentMaterial(enrollCert); err != nil {
			log.Fatalf("[cert-forge] Failed to write enrollment material: %v", err)
		}

		// Write symmetric secrets as plain files on the certs volume
		pgPassPath := filepath.Join(outputDir, "postgres-password")
		if err := os.WriteFile(pgPassPath, postgresPassword, 0600); err != nil {
			log.Fatalf("[cert-forge] Failed to write postgres-password: %v", err)
		}
		log.Printf("[cert-forge] postgres-password written to %s", pgPassPath)

		jwtSecretPath := filepath.Join(outputDir, "jwt-secret")
		if err := os.WriteFile(jwtSecretPath, jwtSecret, 0600); err != nil {
			log.Fatalf("[cert-forge] Failed to write jwt-secret: %v", err)
		}
		log.Printf("[cert-forge] jwt-secret written to %s", jwtSecretPath)

		for _, mat := range staticMaterials {
			if err := writeStaticCertFiles(mat); err != nil {
				log.Fatalf("[cert-forge] %v", err)
			}
		}
	}

	// Build TLS configs — each server has exactly one purpose
	caPool := x509.NewCertPool()
	caPool.AddCert(ca.cert)
	enrollPool := x509.NewCertPool()
	enrollPool.AddCert(enrollmentCA.cert)

	signTLS   := buildInstanceTLSConfig(selfIK, caPool, true)    // constellation mTLS
	enrollTLS := buildInstanceTLSConfig(selfIK, enrollPool, true) // enrollment mTLS

	// Server 1: plain HTTP on publicPort — /ca only (CA cert is public)
	publicPort := cfEnvOr("PUBLIC_PORT", "3022")
	go func() {
		log.Printf("[cert-forge] Public server on :%s (plain HTTP — /ca only)", publicPort)
		if err := http.ListenAndServe(":"+publicPort, buildPublicMux()); err != nil {
			log.Fatalf("[cert-forge] Public server error: %v", err)
		}
	}()

	// Server 2: enrollment mTLS on enrollmentPort — /instance-cert only
	go func() {
		log.Printf("[cert-forge] Enrollment server on :%s (enrollment mTLS — /instance-cert only)", enrollmentPort)
		enrollServer := &http.Server{Addr: ":" + enrollmentPort, Handler: buildEnrollmentMux(cfg), TLSConfig: enrollTLS}
		if err := enrollServer.ListenAndServeTLS("", ""); err != nil {
			log.Fatalf("[cert-forge] Enrollment server error: %v", err)
		}
	}()

	// Start rotation loop — runs forever in background, no-op in Docker Compose
	go startRotationLoop(cfg, inK8s, secretName)

	// Server 3: constellation mTLS on port — /sign only
	log.Printf("[cert-forge] Sign server on :%s (constellation mTLS — /sign only)", port)
	log.Printf("[cert-forge] cert-forge fully operational.")
	signServer := &http.Server{Addr: ":" + port, Handler: buildSignMux(), TLSConfig: signTLS}
	if err := signServer.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[cert-forge] Sign server error: %v", err)
	}
}

func cfEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
