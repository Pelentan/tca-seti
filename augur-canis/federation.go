package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Federation — SETI registration, session cert issuance, event signing
//
// When SETI registers with this AC:
//   1. AC verifies the request signature against the pre-distributed
//      star-gazer public cert.
//   2. AC generates a session key pair (in memory only — lost on restart).
//   3. AC returns the session public cert to SETI.
//   4. Every health feed event published to tca:augur-canis is signed
//      with the session private key.
//
// If AC restarts, the session cert is gone. SETI detects silence or
// signature failure and re-registers. AC treats re-registration identically
// to first registration.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type FederationRegistrationRequest struct {
	SetiInstanceID    string `json:"seti_instance_id"`
	StarGazerPublicCert string `json:"star_gazer_public_cert"`
	Timestamp         string `json:"timestamp"`
	Signature         string `json:"signature"`
}

type FederationRegistrationResponse struct {
	SessionCert     string `json:"session_cert"`
	SessionCertID   string `json:"session_cert_id"`
	IssuedAt        string `json:"issued_at"`
	FeedChannel     string `json:"feed_channel"`
	ConstellationID string `json:"constellation_id"`
}

type FederationStatusResponse struct {
	Registered      bool   `json:"registered"`
	ConstellationID string `json:"constellation_id"`
	SessionCertID   string `json:"session_cert_id,omitempty"`
	RegisteredAt    string `json:"registered_at,omitempty"`
	EventsSigned    int64  `json:"events_signed"`
}

type ServiceRegistrationRequest struct {
	ServiceName     string   `json:"service_name"`
	NetworkEndpoint string   `json:"network_endpoint"`
	Networks        []string `json:"networks,omitempty"`
	CertFingerprint string   `json:"cert_fingerprint"`
	CertPEM         string   `json:"cert_pem"`
	Timestamp       string   `json:"timestamp"`
	Signature       string   `json:"signature"`
}

type ServiceRegistrationAck struct {
	ServiceName  string `json:"service_name"`
	RegisteredAt string `json:"registered_at"`
	Status       string `json:"status"` // "registered" | "refreshed"
}

// ---------------------------------------------------------------------------
// Federation state — in memory only, lost on restart
// ---------------------------------------------------------------------------

type federationState struct {
	mu              sync.RWMutex
	registered      bool
	setiInstanceID  string
	sessionMat      *CertMaterial
	sessionCert     *x509.Certificate
	sessionCertPEM  []byte // DER
	sessionCertID   string
	registeredAt    time.Time
	eventsSigned    int64
}

var federation = &federationState{}

// ---------------------------------------------------------------------------
// Startup — load pre-distributed star-gazer cert
// ---------------------------------------------------------------------------

var starGazerCert *x509.Certificate // pre-distributed SETI public cert

func loadStarGazerCert() {
	path := os.Getenv("STAR_GAZER_CERT")
	if path == "" {
		log.Printf("[federation] STAR_GAZER_CERT not set — federation registration disabled")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[federation] Could not load star-gazer cert from %s: %v", path, err)
		return
	}
	block, _ := pem.Decode(data)
	if block == nil {
		log.Printf("[federation] star-gazer cert at %s is not valid PEM", path)
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		log.Printf("[federation] Could not parse star-gazer cert: %v", err)
		return
	}
	starGazerCert = cert
	log.Printf("[federation] Star-gazer cert loaded (CN=%s)", cert.Subject.CommonName)
}

// ---------------------------------------------------------------------------
// Session cert generation — signed by the constellation CA
// ---------------------------------------------------------------------------

func generateSessionCert() error {
	// Request a session cert from cert-forge — AC never holds the CA key
	// The session cert is used to sign health feed events for SETI verification
	if certMat == nil {
		return fmt.Errorf("certMat not ready — AC not fully initialized")
	}
	// Use cert-forge to issue a dedicated session cert for feed signing
	// Session ID combines service name with a random suffix
	sessionInstanceID := fmt.Sprintf("session-%d", time.Now().UnixNano())
	sessionMat := requestSessionCert(sessionInstanceID)
	if sessionMat == nil {
		return fmt.Errorf("failed to obtain session cert from cert-forge")
	}
	// sessionMat contains the cert and key for this session
	// Extract a cert ID from the fingerprint
	certID := sessionMat.Fingerprint
	if len(certID) > 8 {
		certID = certID[:8]
	}

	federation.mu.Lock()
	federation.sessionMat = sessionMat
	federation.sessionCert = sessionMat.InstanceCert.Leaf
	federation.sessionCertPEM = sessionMat.InstanceCert.Certificate[0]
	federation.sessionCertID = certID
	federation.mu.Unlock()

	log.Printf("[federation] Session cert obtained from cert-forge (id=%s)", certID)
	return nil
}

// requestSessionCert obtains a short-lived cert from cert-forge for signing feed events.
func requestSessionCert(sessionInstanceID string) *CertMaterial {
	if certMat == nil {
		return nil
	}
	forgeURL    := cfEnvOr("CERT_FORGE_URL", "https://cert-forge:4014")
	enrollPort  := cfEnvOr("ENROLLMENT_PORT", "4015")
	enrollCert  := cfEnvOr("ENROLLMENT_CERT", "/certs/enrollment.crt")
	enrollKey   := cfEnvOr("ENROLLMENT_KEY", "/certs/enrollment.key")
	enrollURL   := deriveEnrollmentURL(forgeURL, enrollPort)

	certPEM, keyPEM, fingerprint, instanceCN, validUntil :=
		requestInstanceCert(enrollURL, enrollCert, enrollKey, "augur-canis-session", sessionInstanceID)
	if certPEM == nil {
		return nil
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Printf("[federation] Failed to parse session cert: %v", err)
		return nil
	}
	return &CertMaterial{
		CACertPEM:    certMat.CACertPEM,
		InstanceCert: tlsCert,
		Fingerprint:  fingerprint,
		InstanceCN:   instanceCN,
		InstanceID:   sessionInstanceID,
		ServiceName:  "augur-canis-session",
		ValidUntil:   validUntil,
	}
}

// ---------------------------------------------------------------------------
// Event signing — called before every health feed publish
// ---------------------------------------------------------------------------

// signEventPayload delegates signing to cert-forge /sign using the session cert.
// If no session is active the event is published unsigned.
func signEventPayload(payload []byte) (string, string, error) {
	federation.mu.RLock()
	sessionMat := federation.sessionMat
	certID := federation.sessionCertID
	federation.mu.RUnlock()

	if sessionMat == nil {
		return "", "", nil // no active session — publish unsigned
	}

	sig, err := signPayload(sessionMat, payload)
	if err != nil {
		return "", "", fmt.Errorf("sign event: %v", err)
	}

	federation.mu.Lock()
	federation.eventsSigned++
	federation.mu.Unlock()

	return certID, sig, nil
}

// publishSignedFeedEvent marshals the event map, adds a signature if
// a session is active, and publishes to the health feed channel.
func publishSignedFeedEvent(event map[string]interface{}) {
	// Marshal unsigned payload first (signature is over this)
	unsigned, err := json.Marshal(event)
	if err != nil {
		log.Printf("[federation] Failed to marshal feed event: %v", err)
		return
	}

	certID, sig, err := signEventPayload(unsigned)
	if err != nil {
		log.Printf("[federation] Failed to sign feed event: %v", err)
		// Publish unsigned rather than drop the event
	}

	if certID != "" {
		event["session_cert_id"] = certID
		event["signature"] = sig
	}

	signed, _ := json.Marshal(event)
	ctx := context.Background()
	if err := rdb.Publish(ctx, healthFeedChannel, signed).Err(); err != nil {
		log.Printf("[federation] Failed to publish to %s: %v", healthFeedChannel, err)
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func handleFederationRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if starGazerCert == nil {
		http.Error(w, `{"error":"federation not configured — STAR_GAZER_CERT not loaded"}`, http.StatusServiceUnavailable)
		return
	}

	var req FederationRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.SetiInstanceID == "" || req.Timestamp == "" || req.Signature == "" {
		http.Error(w, `{"error":"missing required fields"}`, http.StatusBadRequest)
		return
	}

	// Verify signature: signed payload is (seti_instance_id + timestamp)
	payload := req.SetiInstanceID + req.Timestamp
	hash := sha256.Sum256([]byte(payload))
	sigBytes, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		http.Error(w, `{"error":"invalid signature encoding"}`, http.StatusBadRequest)
		return
	}

	pubKey, ok := starGazerCert.PublicKey.(*rsa.PublicKey)
	if !ok {
		http.Error(w, `{"error":"star-gazer cert has unexpected key type"}`, http.StatusInternalServerError)
		return
	}
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash[:], sigBytes); err != nil {
		log.Printf("[federation] Registration rejected — signature verification failed for %s: %v", req.SetiInstanceID, err)
		http.Error(w, `{"error":"signature verification failed"}`, http.StatusUnauthorized)
		return
	}

	// Valid — generate session cert
	if err := generateSessionCert(); err != nil {
		log.Printf("[federation] Session cert generation failed: %v", err)
		http.Error(w, `{"error":"session cert generation failed"}`, http.StatusInternalServerError)
		return
	}

	federation.mu.Lock()
	federation.registered = true
	federation.setiInstanceID = req.SetiInstanceID
	federation.registeredAt = time.Now()
	federation.eventsSigned = 0
	sessionCertPEM := federation.sessionCertPEM
	certID := federation.sessionCertID
	sessionMat := federation.sessionMat
	federation.mu.Unlock()

	_ = sessionCertPEM
	_ = sessionMat

	log.Printf("[federation] SETI registered (instance=%s, session=%s)", req.SetiInstanceID, certID)

	// Return PEM-encoded session cert to SETI for feed event verification
	var sessionPEM string
	if federation.sessionMat != nil {
		federation.mu.RLock()
		if len(federation.sessionMat.InstanceCert.Certificate) > 0 {
			sessionPEM = string(pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: federation.sessionMat.InstanceCert.Certificate[0],
			}))
		}
		federation.mu.RUnlock()
	}

	resp := FederationRegistrationResponse{
		SessionCert:     sessionPEM,
		SessionCertID:   certID,
		IssuedAt:        time.Now().UTC().Format(time.RFC3339),
		FeedChannel:     healthFeedChannel,
		ConstellationID: constellationID(),
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleFederationStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	federation.mu.RLock()
	resp := FederationStatusResponse{
		Registered:      federation.registered,
		ConstellationID: constellationID(),
		EventsSigned:    federation.eventsSigned,
	}
	if federation.registered {
		resp.SessionCertID = federation.sessionCertID
		resp.RegisteredAt = federation.registeredAt.UTC().Format(time.RFC3339)
		if federation.sessionMat != nil {
			resp.RegisteredAt = federation.registeredAt.UTC().Format(time.RFC3339)
		}
	}
	federation.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// verifyServiceRegistration verifies a self-registration request.
//
// Strategy: the service sends its cert fingerprint. We fetch the cert from
// the certs volume (all constellation certs live there, generated by cert-forge).
// We verify the request signature using that cert's public key. If the signature
// verifies, the service has the corresponding private key — proving it is the
// legitimate holder of a cert issued by this constellation's CA.
//
// This is a deliberate simplification appropriate for the PoC: in production,
// the cert would be presented over mTLS and the TLS layer would verify it against
// the CA automatically. The explicit check here makes the verification visible
// and auditable.
func verifyServiceRegistration(req ServiceRegistrationRequest) error {
	// Services no longer have static certs in the volume — they obtain instance
	// certs from cert-forge. The service sends its cert PEM in the registration
	// request so AC can verify the signature without reading files.
	if req.CertPEM == "" {
		return fmt.Errorf("cert_pem required for signature verification — service must send its instance cert")
	}

	block, _ := pem.Decode([]byte(req.CertPEM))
	if block == nil {
		return fmt.Errorf("cert_pem for %s is not valid PEM", req.ServiceName)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse cert for %s: %v", req.ServiceName, err)
	}

	// Verify the cert fingerprint matches what the service claimed
	actualFingerprint := fmt.Sprintf("%x", sha256.Sum256(cert.Raw))
	if actualFingerprint != req.CertFingerprint {
		return fmt.Errorf("cert fingerprint mismatch for %s", req.ServiceName)
	}

	// Verify the cert was signed by the constellation CA
	if certMat == nil {
		return fmt.Errorf("AC not fully initialized — no CA cert available")
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(certMat.CACertPEM)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: caPool}); err != nil {
		return fmt.Errorf("cert for %s not signed by constellation CA: %v", req.ServiceName, err)
	}

	// Verify the request signature using the cert's public key
	pubKey, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("cert for %s has non-RSA key", req.ServiceName)
	}

	payload := req.ServiceName + req.NetworkEndpoint + req.CertFingerprint + req.Timestamp
	hash := sha256.Sum256([]byte(payload))

	sigBytes, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %v", err)
	}

	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash[:], sigBytes); err != nil {
		return fmt.Errorf("invalid signature: %v", err)
	}

	return nil
}

func handleServicesRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ServiceRegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.ServiceName == "" || req.NetworkEndpoint == "" || req.CertFingerprint == "" {
		http.Error(w, `{"error":"missing required fields"}`, http.StatusBadRequest)
		return
	}

	// Verify the signature against the constellation CA.
	// The signed payload is: service_name + network_endpoint + cert_fingerprint + timestamp
	// The signing key is the service's private key (cert issued by constellation CA).
	// We verify by loading the service's cert directly — the cert itself proves the
	// signing key is CA-signed, so verifying the signature with the cert's public key
	// is sufficient to establish constellation membership.
	if req.Signature == "" {
		log.Printf("[federation] Registration rejected — missing signature from %s", req.ServiceName)
		http.Error(w, `{"error":"signature required"}`, http.StatusUnauthorized)
		return
	}

	if err := verifyServiceRegistration(req); err != nil {
		log.Printf("[federation] Registration rejected from %s: %v", req.ServiceName, err)
		http.Error(w, `{"error":"signature verification failed"}`, http.StatusUnauthorized)
		return
	}

	// Register or refresh the job in AC's registry
	status := "registered"
	if isJobRegistered(req.ServiceName) {
		status = "refreshed"
	}
	registerJob(req.ServiceName, req.NetworkEndpoint)

	ack := ServiceRegistrationAck{
		ServiceName:  req.ServiceName,
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
		Status:       status,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ack)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func constellationID() string {
	if v := os.Getenv("CONSTELLATION_ID"); v != "" {
		return v
	}
	return os.Getenv("CONSTELLATION")
}

func isJobRegistered(serviceName string) bool {
	jobsMu.RLock()
	_, ok := registeredJobs[serviceName]
	jobsMu.RUnlock()
	return ok
}

// jobsMu guards concurrent access to registeredJobs from self-registration
// requests arriving while the main goroutine may also be reading the map.
var jobsMu sync.RWMutex

func registerJob(serviceName, endpoint string) {
	now := time.Now().UTC().Format(time.RFC3339)
	record := &RegisteredJobRecord{
		ServiceName:     serviceName,
		NetworkEndpoint: endpoint,
		Description:     "self-registered",
		RegisteredAt:    now,
	}

	// Update in-memory map
	jobsMu.Lock()
	registeredJobs[serviceName] = record
	jobsMu.Unlock()

	// Persist to Redis so registration survives AC restart
	ctx := context.Background()
	key := fmt.Sprintf("ac:job:%s", serviceName)
	data, _ := json.Marshal(record)
	rdb.Set(ctx, key, data, 0)

	log.Printf("[federation] Job registered: %s at %s", serviceName, endpoint)
}
