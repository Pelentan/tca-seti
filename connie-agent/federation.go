package main

// ---------------------------------------------------------------------------
// federation.go — TCA Federation Protocol Handshake
//
// connie-agent owns the three-message handshake. It is the only Job that
// holds the Star-Gazer private key.
//
// Cryptographic model:
//   Messages 1 and 3: connie-agent SIGNS with Star-Gazer private key.
//                     Target AC VERIFIES with Star-Gazer public cert.
//                     Uses rsa.SignPKCS1v15 / rsa.VerifyPKCS1v15.
//
//   Message 2:        Target AC ENCRYPTS with Star-Gazer public cert.
//                     connie-agent DECRYPTS with Star-Gazer private key.
//                     Uses rsa.EncryptOAEP / rsa.DecryptOAEP.
//
// After a successful handshake, connie-agent posts session material to
// signal-aggregator POST /federation/subscriptions.
// ---------------------------------------------------------------------------

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Star-Gazer key material
// ---------------------------------------------------------------------------

var (
	starGazerPrivKey *rsa.PrivateKey
	starGazerPubKey  *rsa.PublicKey
)

func loadStarGazerKey() error {
	keyPath  := envOr("STAR_GAZER_KEY",  "/certs/star-gazer.key")
	certPath := envOr("STAR_GAZER_CERT", "/certs/star-gazer.crt")

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read star-gazer key from %s: %v", keyPath, err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return fmt.Errorf("decode star-gazer key PEM")
	}
	privKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		parsed, err2 := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err2 != nil {
			return fmt.Errorf("parse star-gazer private key: %v", err)
		}
		var ok bool
		privKey, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return fmt.Errorf("star-gazer key is not RSA")
		}
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("read star-gazer cert from %s: %v", certPath, err)
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return fmt.Errorf("decode star-gazer cert PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parse star-gazer cert: %v", err)
	}
	pubKey, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("star-gazer cert has non-RSA public key")
	}

	starGazerPrivKey = privKey
	starGazerPubKey  = pubKey
	log.Printf("[federation] Star-Gazer key material loaded")
	return nil
}

// ---------------------------------------------------------------------------
// Crypto helpers
// ---------------------------------------------------------------------------

// signPayload signs payload with the Star-Gazer private key (PKCS1v15).
// Target AC verifies with VerifyPKCS1v15 using the pre-loaded public cert.
func signStarGazer(payload []byte) (string, error) {
	if starGazerPrivKey == nil {
		return "", fmt.Errorf("star-gazer private key not loaded")
	}
	hash := sha256.Sum256(payload)
	sig, err := rsa.SignPKCS1v15(rand.Reader, starGazerPrivKey, crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("sign payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// hybridDecrypt decrypts a payload produced by hybridEncrypt.
// Unwraps the AES key with RSA-OAEP (private key), then decrypts the
// ciphertext with AES-256-GCM.
func hybridDecrypt(encrypted string) ([]byte, error) {
	if starGazerPrivKey == nil {
		return nil, fmt.Errorf("star-gazer private key not loaded")
	}

	// Decode outer base64
	outer, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, fmt.Errorf("decode outer base64: %v", err)
	}

	// Parse JSON envelope
	var envelope struct {
		Key string `json:"key"`
		CT  string `json:"ct"`
	}
	if err := json.Unmarshal(outer, &envelope); err != nil {
		return nil, fmt.Errorf("parse hybrid envelope: %v", err)
	}

	// Unwrap AES key with RSA private key
	wrappedKey, err := base64.StdEncoding.DecodeString(envelope.Key)
	if err != nil {
		return nil, fmt.Errorf("decode wrapped key: %v", err)
	}
	aesKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, starGazerPrivKey, wrappedKey, nil)
	if err != nil {
		return nil, fmt.Errorf("unwrap AES key: %v", err)
	}

	// Decrypt ciphertext with AES-256-GCM
	ciphertext, err := base64.StdEncoding.DecodeString(envelope.CT)
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %v", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %v", err)
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// ---------------------------------------------------------------------------
// Handshake types
// ---------------------------------------------------------------------------

type message2Payload struct {
	SessionCert     string `json:"session_cert"`
	SessionCertID   string `json:"session_cert_id"`
	IssuedAt        string `json:"issued_at"`
	ConstellationID string `json:"constellation_id"`
}

type federationSession struct {
	SessionCert     *x509.Certificate
	SessionCertPEM  string
	SessionCertID   string
	ConstellationID string
	CACertPEM       string // Vox CA cert — for signal-aggregator to verify AC's mTLS cert
}

// ---------------------------------------------------------------------------
// The Three-Message Handshake
// ---------------------------------------------------------------------------

func performHandshake(federationEndpoint, caURL string) (*federationSession, error) {
	if starGazerPrivKey == nil {
		return nil, fmt.Errorf("star-gazer key not loaded — cannot federate")
	}

	// Fetch constellation CA to verify federation port's server cert
	caPool, caPEM, err := fetchConstellationCA(caURL)
	if err != nil {
		return nil, fmt.Errorf("fetch constellation CA: %v", err)
	}
	client := federationClient(caPool)

	// -------------------------------------------------------------------
	// Message 1 — sign {seti_instance_id, timestamp} with private key
	// -------------------------------------------------------------------
	m1Payload, _ := json.Marshal(map[string]string{
		"seti_instance_id": setiInstanceID,
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
	})
	m1PayloadB64 := base64.StdEncoding.EncodeToString(m1Payload)

	m1Sig, err := signStarGazer(m1Payload)
	if err != nil {
		return nil, fmt.Errorf("message 1 sign: %v", err)
	}

	m1Body, _ := json.Marshal(map[string]string{
		"payload":   m1PayloadB64,
		"signature": m1Sig,
	})

	log.Printf("[federation] Sending Message 1 to %s", federationEndpoint)
	m2Raw, err := postToAC(client, federationEndpoint+"/federation/register", m1Body)
	if err != nil {
		return nil, fmt.Errorf("message 1: %v", err)
	}

	// -------------------------------------------------------------------
	// Message 2 — decrypt OAEP response with private key
	// -------------------------------------------------------------------
	var m2Envelope struct {
		EncryptedPayload string `json:"encrypted_payload"`
	}
	if err := json.Unmarshal(m2Raw, &m2Envelope); err != nil || m2Envelope.EncryptedPayload == "" {
		return nil, fmt.Errorf("parse message 2 envelope: %v", err)
	}

	m2Plain, err := hybridDecrypt(m2Envelope.EncryptedPayload)
	if err != nil {
		return nil, fmt.Errorf("decrypt message 2 (possible spoofed response): %v", err)
	}

	var m2 message2Payload
	if err := json.Unmarshal(m2Plain, &m2); err != nil || m2.SessionCert == "" || m2.SessionCertID == "" {
		return nil, fmt.Errorf("parse message 2 payload: %v", err)
	}

	block, _ := pem.Decode([]byte(m2.SessionCert))
	if block == nil {
		return nil, fmt.Errorf("decode session cert PEM from message 2")
	}
	sessionCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse session cert: %v", err)
	}

	log.Printf("[federation] Message 2 decrypted — AC verified (constellation=%s session=%s)",
		m2.ConstellationID, m2.SessionCertID)

	// -------------------------------------------------------------------
	// Message 3 — sign {seti_instance_id, session_cert_id, monitor_cert_public}
	// -------------------------------------------------------------------
	monitorCertPEM := extractInstanceCertPEM()
	if monitorCertPEM == "" {
		return nil, fmt.Errorf("could not extract connie-agent instance cert for message 3")
	}

	m3Payload, _ := json.Marshal(map[string]string{
		"seti_instance_id":    setiInstanceID,
		"session_cert_id":     m2.SessionCertID,
		"monitor_cert_public": monitorCertPEM,
	})
	m3PayloadB64 := base64.StdEncoding.EncodeToString(m3Payload)

	m3Sig, err := signStarGazer(m3Payload)
	if err != nil {
		return nil, fmt.Errorf("message 3 sign: %v", err)
	}

	m3Body, _ := json.Marshal(map[string]string{
		"payload":   m3PayloadB64,
		"signature": m3Sig,
	})

	log.Printf("[federation] Sending Message 3 to %s", federationEndpoint)
	if _, err = postToAC(client, federationEndpoint+"/federation/acknowledge", m3Body); err != nil {
		return nil, fmt.Errorf("message 3: %v", err)
	}

	log.Printf("[federation] Handshake complete — %s (constellation=%s session=%s)",
		federationEndpoint, m2.ConstellationID, m2.SessionCertID)

	return &federationSession{
		SessionCert:     sessionCert,
		SessionCertPEM:  m2.SessionCert,
		SessionCertID:   m2.SessionCertID,
		ConstellationID: m2.ConstellationID,
		CACertPEM:       caPEM,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func extractInstanceCertPEM() string {
	if certMat == nil || len(certMat.InstanceCert.Certificate) == 0 {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certMat.InstanceCert.Certificate[0],
	}))
}

func extractInstanceKeyPEM() string {
	if certMat == nil || len(certMat.InstanceKeyPEM) == 0 {
		return ""
	}
	return string(certMat.InstanceKeyPEM)
}

// fetchConstellationCA fetches the constellation's CA cert from cert-forge's
// public endpoint and returns it as a parsed cert pool.
func fetchConstellationCA(caURL string) (*x509.CertPool, string, error) {
	if caURL == "" {
		return nil, "", fmt.Errorf("ca_url not configured in remote-apps.json")
	}
	resp, err := http.Get(caURL)
	if err != nil {
		return nil, "", fmt.Errorf("fetch CA from %s: %v", caURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("CA endpoint returned %d", resp.StatusCode)
	}
	var result struct {
		CACert string `json:"ca_cert"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.CACert == "" {
		return nil, "", fmt.Errorf("parse CA response: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(result.CACert)) {
		return nil, "", fmt.Errorf("parse CA cert PEM")
	}
	log.Printf("[federation] Fetched constellation CA from %s", caURL)
	return pool, result.CACert, nil
}

// federationClient builds an HTTP client that trusts the constellation's CA cert.
// Used for the federation port handshake — the federation port uses AC's instance
// cert (signed by Vox CA) as its server cert, so we need to trust Vox's CA.
// No client cert is presented — application-layer signatures handle authentication.
func federationClient(caPool *x509.CertPool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    caPool,
				MinVersion: tls.VersionTLS13,
			},
		},
	}
}

func postToAC(client *http.Client, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, raw)
	}
	return raw, nil
}

func notifySignalAggregator(tag, acEndpoint string, session *federationSession) error {
	monitorCertPEM := extractInstanceCertPEM()
	monitorKeyPEM  := extractInstanceKeyPEM()

	if monitorCertPEM == "" {
		return fmt.Errorf("extractInstanceCertPEM returned empty — certMat may not be initialized")
	}
	if monitorKeyPEM == "" {
		return fmt.Errorf("extractInstanceKeyPEM returned empty — private key marshal failed, check key type")
	}

	body, _ := json.Marshal(map[string]string{
		"application_id":   tag,
		"ac_endpoint":      acEndpoint,
		"session_cert":     session.SessionCertPEM,
		"session_cert_id":  session.SessionCertID,
		"monitor_cert_pem": monitorCertPEM,
		"monitor_key_pem":  monitorKeyPEM,
		"ca_cert":          session.CACertPEM,
	})

	req, err := http.NewRequest(http.MethodPost,
		signalAggregatorURL+"/federation/subscriptions",
		strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := upstreamClient.Do(req)
	if err != nil {
		return fmt.Errorf("notify signal-aggregator: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		log.Printf("[federation] signal-aggregator connected to %s stream", tag)
		return nil
	}

	raw, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("signal-aggregator returned %d: %s", resp.StatusCode, raw)
}
