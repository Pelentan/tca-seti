package main

import (
	"context"
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
	ServiceName     string `json:"service_name"`
	NetworkEndpoint string `json:"network_endpoint"`
	Networks        []string `json:"networks,omitempty"`
	CertFingerprint string `json:"cert_fingerprint"`
	Timestamp       string `json:"timestamp"`
	Signature       string `json:"signature"`
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
	sessionKey      *rsa.PrivateKey
	sessionCert     []byte // PEM
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
	// Load constellation CA cert and key
	caPath := "/certs/ca.crt"
	caKeyPath := "/certs/ca.key"

	caCertPEM, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("load CA cert: %v", err)
	}
	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return fmt.Errorf("load CA key: %v", err)
	}

	tlsCert, err := tls.X509KeyPair(caCertPEM, caKeyPEM)
	if err != nil {
		return fmt.Errorf("parse CA keypair: %v", err)
	}
	caCert, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse CA cert: %v", err)
	}
	caKey, ok := tlsCert.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return fmt.Errorf("CA key is not RSA")
	}

	// Generate session key pair
	sessionKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate session key: %v", err)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("ac-session-%s", constellationID()),
			Organization: []string{"SETI Federation"},
		},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &sessionKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create session cert: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	// Cert ID: first 8 chars of base64-encoded serial
	certID := base64.RawURLEncoding.EncodeToString(serial.Bytes())
	if len(certID) > 8 {
		certID = certID[:8]
	}

	federation.mu.Lock()
	federation.sessionKey = sessionKey
	federation.sessionCert = certPEM
	federation.sessionCertID = certID
	federation.mu.Unlock()

	log.Printf("[federation] Session cert generated (id=%s, valid 48h)", certID)
	return nil
}

// ---------------------------------------------------------------------------
// Event signing — called before every health feed publish
// ---------------------------------------------------------------------------

// signEvent adds session_cert_id and signature to an event map before
// it is marshaled and published to tca:augur-canis.
// If no session is active the event is published unsigned.
func signEventPayload(payload []byte) (string, string, error) {
	federation.mu.RLock()
	key := federation.sessionKey
	certID := federation.sessionCertID
	federation.mu.RUnlock()

	if key == nil {
		return "", "", nil // no active session — publish unsigned
	}

	hash := sha256.Sum256(payload)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		return "", "", fmt.Errorf("sign event: %v", err)
	}

	federation.mu.Lock()
	federation.eventsSigned++
	federation.mu.Unlock()

	return certID, base64.StdEncoding.EncodeToString(sig), nil
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
	certPEM := federation.sessionCert
	certID := federation.sessionCertID
	federation.mu.Unlock()

	log.Printf("[federation] SETI registered (instance=%s, session=%s)", req.SetiInstanceID, certID)

	resp := FederationRegistrationResponse{
		SessionCert:     string(certPEM),
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
	}
	federation.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
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

	// STUB: signature verification against constellation CA deferred.
	// Full implementation verifies req.Signature against ca.crt before
	// accepting the registration. For now, accept all well-formed requests
	// and log visibly.
	log.Printf("[federation] STUB: service self-registration from %s at %s (signature verification not yet enforced)",
		req.ServiceName, req.NetworkEndpoint)

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
	ctx := context.Background()
	key := fmt.Sprintf("ac:job:%s", serviceName)
	v, err := rdb.Get(ctx, key).Result()
	return err == nil && v != ""
}

func registerJob(serviceName, endpoint string) {
	ctx := context.Background()
	key := fmt.Sprintf("ac:job:%s", serviceName)
	rdb.Set(ctx, key, endpoint, 0)
}
