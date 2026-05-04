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

func selfRegister(serviceName, networkEndpoint string) {
	acURL := os.Getenv("AUGUR_CANIS_URL")
	if acURL == "" {
		acURL = "https://augur-canis:4010"
	}

	certPath := fmt.Sprintf("/certs/%s.crt", serviceName)
	keyPath := fmt.Sprintf("/certs/%s.key", serviceName)

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		log.Printf("[%s] selfRegister: could not read cert: %v", serviceName, err)
		return
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(cert.Raw))

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		log.Printf("[%s] selfRegister: could not read key: %v", serviceName, err)
		return
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return
	}
	privateKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return
	}

	timestamp := time.Now().UTC().Format(time.RFC3339)
	payload := serviceName + networkEndpoint + fingerprint + timestamp
	hash := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, hash[:])
	if err != nil {
		return
	}

	body, _ := json.Marshal(map[string]interface{}{
		"service_name":     serviceName,
		"network_endpoint": networkEndpoint,
		"cert_fingerprint": fingerprint,
		"cert_pem":         string(certPEM),
		"timestamp":        timestamp,
		"signature":        base64.StdEncoding.EncodeToString(sig),
	})

	// Build a minimal mTLS client inline — observability has no shared upstreamClient
	caCert, err := os.ReadFile("/certs/ca.crt")
	if err != nil {
		return
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caCert)
	tlsCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return
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
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[%s] selfRegister: %v — AC may not be ready yet", serviceName, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		var ack map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&ack)
		log.Printf("[%s] selfRegister: registered with AC (status=%s)", serviceName, ack["status"])
	} else {
		log.Printf("[%s] selfRegister: AC returned %d", serviceName, resp.StatusCode)
	}
}
