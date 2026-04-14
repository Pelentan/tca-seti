package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
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

// selfRegister publishes a signed registration request to Augur Canis on startup.
// AC verifies the signature against the constellation CA before accepting.
// Idempotent — safe to call on every restart.
func selfRegister(serviceName, networkEndpoint string) {
	acURL := envOr("AUGUR_CANIS_URL", "https://augur-canis:4010")

	certPath := fmt.Sprintf("/certs/%s.crt", serviceName)
	keyPath := fmt.Sprintf("/certs/%s.key", serviceName)

	// Load service cert for fingerprint
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		log.Printf("[%s] selfRegister: could not read cert: %v", serviceName, err)
		return
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		log.Printf("[%s] selfRegister: cert is not valid PEM", serviceName)
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		log.Printf("[%s] selfRegister: could not parse cert: %v", serviceName, err)
		return
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(cert.Raw))

	// Load private key for signing
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		log.Printf("[%s] selfRegister: could not read key: %v", serviceName, err)
		return
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		log.Printf("[%s] selfRegister: key is not valid PEM", serviceName)
		return
	}
	privateKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		log.Printf("[%s] selfRegister: could not parse key: %v", serviceName, err)
		return
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)

	// Signed payload: service_name + network_endpoint + cert_fingerprint + timestamp
	payload := serviceName + networkEndpoint + fingerprint + timestamp
	hash := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hash[:])
	if err != nil {
		log.Printf("[%s] selfRegister: signing failed: %v", serviceName, err)
		return
	}

	body, _ := json.Marshal(map[string]interface{}{
		"service_name":     serviceName,
		"network_endpoint": networkEndpoint,
		"cert_fingerprint": fingerprint,
		"timestamp":        timestamp,
		"signature":        base64.StdEncoding.EncodeToString(sig),
	})

	url := strings.TrimRight(acURL, "/") + "/services/register"
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		log.Printf("[%s] selfRegister: build request failed: %v", serviceName, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := upstreamClient.Do(req)
	if err != nil {
		log.Printf("[%s] selfRegister: request failed: %v — AC may not be ready yet", serviceName, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var ack map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&ack)
		status, _ := ack["status"].(string)
		log.Printf("[%s] selfRegister: registered with AC (status=%s)", serviceName, status)
	} else {
		log.Printf("[%s] selfRegister: AC returned %d", serviceName, resp.StatusCode)
	}
}
