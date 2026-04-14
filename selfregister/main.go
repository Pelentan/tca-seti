// selfregister — TCA service self-registration binary.
//
// A static Go binary that every TCA Job includes in its container image,
// regardless of the Job's primary language. Follows the same pattern as
// /healthcheck — one implementation, available everywhere.
//
// On execution:
//  1. Reads the service cert and key from /certs/{SERVICE_NAME}.crt/.key
//  2. Computes the cert SHA-256 fingerprint
//  3. Signs the registration payload with the service private key
//  4. POSTs to Augur Canis /services/register over mTLS
//  5. Exits 0 on success, 1 on failure
//
// Environment variables:
//   SERVICE_NAME      — this service's name (required)
//   NETWORK_ENDPOINT  — this service's mTLS endpoint (required)
//   AUGUR_CANIS_URL   — AC endpoint (default: https://augur-canis:4010)
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
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	serviceName := os.Getenv("SERVICE_NAME")
	endpoint := os.Getenv("NETWORK_ENDPOINT")
	acURL := os.Getenv("AUGUR_CANIS_URL")

	if serviceName == "" {
		log.Fatal("[selfregister] SERVICE_NAME not set")
	}
	if endpoint == "" {
		log.Fatalf("[selfregister] NETWORK_ENDPOINT not set for %s", serviceName)
	}
	if acURL == "" {
		acURL = "https://augur-canis:4010"
	}

	certPath := fmt.Sprintf("/certs/%s.crt", serviceName)
	keyPath := fmt.Sprintf("/certs/%s.key", serviceName)

	// Load cert
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		log.Fatalf("[selfregister] Cannot read cert for %s: %v", serviceName, err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		log.Fatalf("[selfregister] Cert for %s is not valid PEM", serviceName)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		log.Fatalf("[selfregister] Cannot parse cert for %s: %v", serviceName, err)
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(cert.Raw))

	// Load private key
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatalf("[selfregister] Cannot read key for %s: %v", serviceName, err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		log.Fatalf("[selfregister] Key for %s is not valid PEM", serviceName)
	}
	privateKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		log.Fatalf("[selfregister] Cannot parse key for %s: %v", serviceName, err)
	}

	// Build and sign payload
	timestamp := time.Now().UTC().Format(time.RFC3339)
	payload := serviceName + endpoint + fingerprint + timestamp
	hash := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hash[:])
	if err != nil {
		log.Fatalf("[selfregister] Signing failed for %s: %v", serviceName, err)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"service_name":     serviceName,
		"network_endpoint": endpoint,
		"cert_fingerprint": fingerprint,
		"timestamp":        timestamp,
		"signature":        base64.StdEncoding.EncodeToString(sig),
	})

	// Build mTLS client using this service's own cert
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		log.Fatalf("[selfregister] Cannot read CA cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)

	tlsCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		log.Fatalf("[selfregister] Cannot load keypair for %s: %v", serviceName, err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      pool,
				Certificates: []tls.Certificate{tlsCert},
				MinVersion:   tls.VersionTLS13,
			},
		},
		Timeout: 10 * time.Second,
	}

	url := strings.TrimRight(acURL, "/") + "/services/register"
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		log.Fatalf("[selfregister] Cannot build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("[selfregister] Registration request failed for %s: %v", serviceName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Fatalf("[selfregister] AC returned %d for %s", resp.StatusCode, serviceName)
	}

	var ack map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&ack)
	log.Printf("[selfregister] %s registered with AC (status=%s)", serviceName, ack["status"])
}
