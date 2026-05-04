package main

import (
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
// cert-forge client
//
// Every service calls obtainCerts() on startup instead of reading cert
// files from /certs/. The private key is delivered over the encrypted
// enrollment mTLS connection and held in memory only — never on disk,
// never on the shared volume.
//
// Flow:
//  1. GET /ca (unauthenticated) — get CA cert
//  2. POST /instance-cert on enrollment port (enrollment mTLS) — get cert+key
//  3. Hold both in memory, build TLS configs from them
//  4. All signing delegated to cert-forge /sign — algorithm is cert-forge's concern
// ---------------------------------------------------------------------------

type CertMaterial struct {
	CACertPEM    []byte
	InstanceCert tls.Certificate
	Fingerprint  string
	InstanceCN   string
	InstanceID   string
	ServiceName  string
	ValidUntil   time.Time
}

func obtainCerts(serviceName string) *CertMaterial {
	forgeURL   := cfEnvOr("CERT_FORGE_URL", "https://cert-forge:4014")
	enrollPort := cfEnvOr("ENROLLMENT_PORT", "4015")
	enrollCert := cfEnvOr("ENROLLMENT_CERT", "/certs/enrollment.crt")
	enrollKey  := cfEnvOr("ENROLLMENT_KEY", "/certs/enrollment.key")
	instanceID := os.Getenv("HOSTNAME")
	if instanceID == "" {
		instanceID = "local"
	}

	enrollURL := deriveEnrollmentURL(forgeURL, enrollPort)

	caCertPEM := fetchCACert(forgeURL)
	sans      := buildSANs(serviceName, instanceID)
	certPEM, keyPEM, fingerprint, instanceCN, validUntil :=
		requestInstanceCert(enrollURL, enrollCert, enrollKey, serviceName, instanceID, sans)

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		log.Fatalf("[%s] Failed to parse instance cert+key: %v", serviceName, err)
	}

	log.Printf("[%s] Instance cert obtained from cert-forge (CN=%s, valid until %s)",
		serviceName, instanceCN, validUntil.Format(time.RFC3339))

	return &CertMaterial{
		CACertPEM:    caCertPEM,
		InstanceCert: tlsCert,
		Fingerprint:  fingerprint,
		InstanceCN:   instanceCN,
		InstanceID:   instanceID,
		ServiceName:  serviceName,
		ValidUntil:   validUntil,
	}
}

// buildSANs constructs the SAN list for the gateway's instance cert.
// Because cert-forge's sans field overrides (not merges) the defaults,
// we must explicitly include the standard entries alongside any
// environment-supplied external hostname.
func buildSANs(serviceName, instanceID string) []string {
	sans := []string{
		serviceName,
		fmt.Sprintf("%s-%s", serviceName, instanceID),
		"localhost",
	}
	if h := os.Getenv("GATEWAY_EXTERNAL_HOSTNAME"); h != "" {
		sans = append(sans, h)
	}
	return sans
}

func deriveEnrollmentURL(forgeURL, enrollPort string) string {
	parts := strings.Split(forgeURL, ":")
	if len(parts) == 3 {
		return parts[0] + ":" + parts[1] + ":" + enrollPort
	}
	return strings.TrimRight(forgeURL, "/") + ":" + enrollPort
}

func fetchCACert(forgeURL string) []byte {
	// /ca is on plain HTTP — no TLS, no client cert needed
	publicPort := cfEnvOr("PUBLIC_PORT", "4016")
	publicURL := deriveEnrollmentURL(forgeURL, publicPort)
	publicURL = strings.Replace(publicURL, "https://", "http://", 1)
	url := strings.TrimRight(publicURL, "/") + "/ca"
	client := &http.Client{Timeout: 5 * time.Second}

	for attempt := 1; attempt <= 30; attempt++ {
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("[certforge] Waiting for cert-forge /ca (attempt %d/30): %v", attempt, err)
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result map[string]string
		if err := json.Unmarshal(body, &result); err != nil || result["ca_cert"] == "" {
			log.Printf("[certforge] Invalid /ca response (attempt %d/30)", attempt)
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("[certforge] CA cert obtained")
		return []byte(result["ca_cert"])
	}
	log.Fatalf("[certforge] Could not obtain CA cert after 30 attempts")
	return nil
}

func requestInstanceCert(enrollURL, enrollCertPath, enrollKeyPath, serviceName, instanceID string, sans []string) ([]byte, []byte, string, string, time.Time) {
	enrollTLSCert, err := tls.LoadX509KeyPair(enrollCertPath, enrollKeyPath)
	if err != nil {
		log.Fatalf("[certforge] Cannot load enrollment cert %s: %v", enrollCertPath, err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates:       []tls.Certificate{enrollTLSCert},
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS13,
			},
		},
		Timeout: 10 * time.Second,
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"service_name": serviceName,
		"instance_id":  instanceID,
		"sans":         sans,
	})
	if err != nil {
		log.Fatalf("[certforge] Failed to marshal instance-cert request: %v", err)
	}

	url := strings.TrimRight(enrollURL, "/") + "/instance-cert"

	for attempt := 1; attempt <= 30; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(string(reqBody)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[certforge] Waiting for /instance-cert (attempt %d/30): %v", attempt, err)
			time.Sleep(2 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			log.Printf("[certforge] /instance-cert returned %d (attempt %d/30)", resp.StatusCode, attempt)
			time.Sleep(2 * time.Second)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var result map[string]interface{}
		if err := json.Unmarshal(respBody, &result); err != nil {
			log.Printf("[certforge] Invalid /instance-cert response: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		certPEM  := []byte(result["cert"].(string))
		keyPEM   := []byte(result["key"].(string))
		fp       := result["fingerprint"].(string)
		cn       := result["instance_cn"].(string)
		vuStr, _ := result["valid_until"].(string)
		vu, _    := time.Parse(time.RFC3339, vuStr)

		return certPEM, keyPEM, fp, cn, vu
	}
	log.Fatalf("[certforge] Could not obtain instance cert after 30 attempts")
	return nil, nil, "", "", time.Time{}
}

func buildServerTLS(mat *CertMaterial) *tls.Config {
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(mat.CACertPEM)
	return &tls.Config{
		Certificates: []tls.Certificate{mat.InstanceCert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

func buildClientTLS(mat *CertMaterial) *tls.Config {
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(mat.CACertPEM)
	return &tls.Config{
		Certificates: []tls.Certificate{mat.InstanceCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}
}

// signPayload delegates signing to cert-forge /sign.
// The calling service never handles the private key for signing —
// cert-forge owns the algorithm and the key material for this purpose.
func signPayload(mat *CertMaterial, payload []byte) (string, error) {
	forgeURL := cfEnvOr("CERT_FORGE_URL", "https://cert-forge:4014")

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: buildClientTLS(mat),
		},
		Timeout: 10 * time.Second,
	}

	body := fmt.Sprintf(`{"service_name":%q,"instance_id":%q,"payload":%q}`,
		mat.ServiceName, mat.InstanceID, base64.StdEncoding.EncodeToString(payload))

	req, _ := http.NewRequest(http.MethodPost,
		strings.TrimRight(forgeURL, "/")+"/sign",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sign request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cert-forge /sign returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]string
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("sign response invalid: %v", err)
	}
	sig := result["signature"]
	if sig == "" {
		return "", fmt.Errorf("cert-forge returned empty signature")
	}
	return sig, nil
}

// selfRegisterWithAC handles the full self-registration flow:
// sign the payload via cert-forge, POST to Augur Canis.
// Retries with backoff — AC may not be ready when this is first called.
func selfRegisterWithAC(mat *CertMaterial, networkEndpoint string) {
	acURL := cfEnvOr("AUGUR_CANIS_URL", "https://augur-canis:4010")

	for attempt := 1; ; attempt++ {
		timestamp := time.Now().UTC().Format(time.RFC3339)
		payload   := mat.ServiceName + networkEndpoint + mat.Fingerprint + timestamp

		sig, err := signPayload(mat, []byte(payload))
		if err != nil {
			log.Printf("[%s] selfRegister: signing failed (attempt %d): %v", mat.ServiceName, attempt, err)
			time.Sleep(3 * time.Second)
			continue
		}

		var certPEM string
		if len(mat.InstanceCert.Certificate) > 0 {
			certPEM = string(pem.EncodeToMemory(&pem.Block{
				Type:  "CERTIFICATE",
				Bytes: mat.InstanceCert.Certificate[0],
			}))
		}

		body, _ := json.Marshal(map[string]interface{}{
			"service_name":     mat.ServiceName,
			"network_endpoint": networkEndpoint,
			"cert_fingerprint": mat.Fingerprint,
			"cert_pem":         certPEM,
			"timestamp":        timestamp,
			"signature":        sig,
		})

		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: buildClientTLS(mat),
			},
			Timeout: 10 * time.Second,
		}

		req, _ := http.NewRequest(http.MethodPost,
			strings.TrimRight(acURL, "/")+"/services/register",
			strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[%s] selfRegister: AC unreachable (attempt %d): %v", mat.ServiceName, attempt, err)
			time.Sleep(3 * time.Second)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var ack map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&ack)
			log.Printf("[%s] selfRegister: registered with AC (status=%s)", mat.ServiceName, ack["status"])
			return
		}
		log.Printf("[%s] selfRegister: AC returned %d (attempt %d)", mat.ServiceName, resp.StatusCode, attempt)
		time.Sleep(3 * time.Second)
	}
}

func cfEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
