package main

// ---------------------------------------------------------------------------
// cert-forge — Self-Registration with Augur Canis
//
// cert-forge differs from every other Job: it never writes its own instance
// cert to disk (the K8s Secret holds constellation material, not cert-forge's
// own identity). The standard selfregister pattern (read cert from /certs/)
// therefore cannot be used here.
//
// Instead, selfRegisterWithAC:
//   - Uses selfIK (held in memory) for fingerprint and signing
//   - Builds a one-shot mTLS client from selfIK and the constellation CA pool
//   - Fires in a goroutine with a 5s delay to let the sign server bind
//   - Retries up to 10 times at 5s intervals (AC may not be up yet)
//
// The network_endpoint registered is the sign server (constellation mTLS port)
// because that is the port AC uses for its contract test.
// ---------------------------------------------------------------------------

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// selfRegisterWithAC registers cert-forge with Augur Canis using the in-memory
// selfIK.  Call this in a goroutine after the sign server starts.
func selfRegisterWithAC(selfIK *InstanceKey, caPool *x509.CertPool) {
	acURL := cfEnvOr("AUGUR_CANIS_URL", "https://augur-canis:4010")
	signPort := cfEnvOr("PORT", "3020")
	networkEndpoint := fmt.Sprintf("https://cert-forge:%s", signPort)

	// Build a one-shot mTLS client authenticated with our own instance cert.
	tlsCert := tls.Certificate{
		Certificate: [][]byte{selfIK.cert.Raw},
		PrivateKey:  selfIK.key,
		Leaf:        selfIK.cert,
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{tlsCert},
				RootCAs:      caPool,
				MinVersion:   tls.VersionTLS13,
			},
		},
	}

	fingerprint := selfIK.fingerprint
	timestamp := time.Now().UTC().Format(time.RFC3339)
	payload := "cert-forge" + networkEndpoint + fingerprint + timestamp
	hash := sha256.Sum256([]byte(payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, selfIK.key, crypto.SHA256, hash[:])
	if err != nil {
		log.Printf("[cert-forge] selfRegister: signing failed: %v", err)
		return
	}

	body, _ := json.Marshal(map[string]interface{}{
		"service_name":     "cert-forge",
		"network_endpoint": networkEndpoint,
		"cert_fingerprint": fingerprint,
		"cert_pem":         string(selfIK.certPEM),
		"timestamp":        timestamp,
		"signature":        base64.StdEncoding.EncodeToString(sig),
	})

	url := strings.TrimRight(acURL, "/") + "/services/register"

	for attempt := 1; attempt <= 10; attempt++ {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
		if err != nil {
			log.Printf("[cert-forge] selfRegister: build request failed: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[cert-forge] selfRegister: attempt %d failed: %v — retrying in 5s", attempt, err)
			time.Sleep(5 * time.Second)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var ack map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&ack)
			status, _ := ack["status"].(string)
			log.Printf("[cert-forge] selfRegister: registered with AC (status=%s)", status)
			return
		}
		log.Printf("[cert-forge] selfRegister: attempt %d — AC returned %d, retrying in 5s", attempt, resp.StatusCode)
		time.Sleep(5 * time.Second)
	}

	log.Printf("[cert-forge] selfRegister: failed after 10 attempts — AC unreachable or rejecting registration")
}
