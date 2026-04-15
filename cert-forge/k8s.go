package main

// ---------------------------------------------------------------------------
// K8s Secret writer — stdlib only.
//
// cert-forge detects whether it is running in Kubernetes by checking for
// the projected ServiceAccount token. If present, it writes all cert
// material to a K8s Secret instead of to a volume. Services then mount
// the Secret to obtain the enrollment cert.
//
// CA persistence: on startup cert-forge checks whether the Secret already
// contains a valid CA cert and key. If so, it loads them rather than
// generating a new CA. This means cert-forge can restart without
// invalidating every service's instance cert — the CA is stable across
// cert-forge pod restarts. Only the enrollment material and static certs
// are regenerated on restart (they are short-lived credentials, not the
// trust anchor).
//
// No external dependencies. No client-go. The K8s API is HTTP/JSON.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"crypto/rsa"
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
	"time"
)

const (
	saTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCACertPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	k8sAPIBase      = "https://kubernetes.default.svc"

	// Minimum remaining validity before we treat a CA as expired and regenerate.
	caMinRemainingDays = 30
)

// InKubernetes returns true when the projected SA token is present.
// Reliable: K8s always mounts it; Docker Compose never does.
func InKubernetes() bool {
	_, err := os.Stat(saTokenPath)
	return err == nil
}

func k8sNamespace() string {
	if ns := os.Getenv("K8S_NAMESPACE"); ns != "" {
		return ns
	}
	b, err := os.ReadFile(saNamespacePath)
	if err == nil && len(b) > 0 {
		return string(b)
	}
	return "seti"
}

func k8sHTTPClient() (*http.Client, error) {
	caCert, err := os.ReadFile(saCACertPath)
	if err != nil {
		return nil, fmt.Errorf("read k8s CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("parse k8s CA cert")
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}, nil
}

func k8sToken() (string, error) {
	b, err := os.ReadFile(saTokenPath)
	if err != nil {
		return "", fmt.Errorf("read SA token: %v", err)
	}
	return string(b), nil
}

// ReadK8sSecret retrieves the named Secret and returns its data map
// (keys → raw bytes, already base64-decoded).
// Returns nil, nil if the Secret does not exist.
func ReadK8sSecret(secretName string) (map[string][]byte, error) {
	namespace := k8sNamespace()
	client, err := k8sHTTPClient()
	if err != nil {
		return nil, err
	}
	token, err := k8sToken()
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s", k8sAPIBase, namespace, secretName)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read secret: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("read secret status %d: %s", resp.StatusCode, b)
	}

	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&secret); err != nil {
		return nil, fmt.Errorf("decode secret: %v", err)
	}

	result := make(map[string][]byte, len(secret.Data))
	for k, v := range secret.Data {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("decode secret key %q: %v", k, err)
		}
		result[k] = decoded
	}
	return result, nil
}

// LoadExistingCA attempts to load a previously persisted CA cert and key
// from the K8s Secret. Returns nil, nil if no valid CA is found —
// the caller should then generate a fresh CA.
//
// A CA is considered invalid if:
//   - The Secret doesn't exist or doesn't contain ca.crt / ca.key
//   - The cert or key cannot be parsed
//   - The cert expires within caMinRemainingDays
func LoadExistingCA(secretName string) (*CA, error) {
	data, err := ReadK8sSecret(secretName)
	if err != nil {
		return nil, err
	}
	if data == nil {
		log.Printf("[cert-forge] No existing Secret found — will generate fresh CA")
		return nil, nil
	}

	certPEM, hasCert := data["ca.crt"]
	keyPEM, hasKey := data["ca.key"]
	if !hasCert || !hasKey {
		log.Printf("[cert-forge] Secret exists but missing ca.crt or ca.key — will generate fresh CA")
		return nil, nil
	}

	// Parse cert
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		log.Printf("[cert-forge] ca.crt in Secret is not valid PEM — will generate fresh CA")
		return nil, nil
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		log.Printf("[cert-forge] ca.crt parse error: %v — will generate fresh CA", err)
		return nil, nil
	}

	// Check validity window
	remaining := time.Until(cert.NotAfter)
	if remaining < time.Duration(caMinRemainingDays)*24*time.Hour {
		log.Printf("[cert-forge] Existing CA expires in %.0f days (threshold %d) — will generate fresh CA",
			remaining.Hours()/24, caMinRemainingDays)
		return nil, nil
	}

	// Parse key
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		log.Printf("[cert-forge] ca.key in Secret is not valid PEM — will generate fresh CA")
		return nil, nil
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		log.Printf("[cert-forge] ca.key parse error: %v — will generate fresh CA", err)
		return nil, nil
	}

	log.Printf("[cert-forge] Loaded existing CA from Secret (CN=%s, expires %s, %.0f days remaining)",
		cert.Subject.CommonName,
		cert.NotAfter.Format("2006-01-02"),
		remaining.Hours()/24,
	)
	return &CA{
		cert:    cert,
		certDER: certBlock.Bytes,
		certPEM: certPEM,
		key:     key,
	}, nil
}

// caKeyPEM returns the PEM-encoded CA private key for Secret storage.
func caKeyPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// WriteK8sSecret creates or replaces the named Secret in the pod namespace.
// data values are raw bytes — base64 encoding is handled here per the
// K8s Secret data field requirement.
func WriteK8sSecret(secretName string, data map[string][]byte) error {
	namespace := k8sNamespace()
	client, err := k8sHTTPClient()
	if err != nil {
		return err
	}
	token, err := k8sToken()
	if err != nil {
		return err
	}

	encodedData := make(map[string]string, len(data))
	for k, v := range data {
		encodedData[k] = base64.StdEncoding.EncodeToString(v)
	}

	secret := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      secretName,
			"namespace": namespace,
		},
		"type": "Opaque",
		"data": encodedData,
	}

	body, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("marshal secret: %v", err)
	}

	authHeader := "Bearer " + token
	baseURL := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets", k8sAPIBase, namespace)

	// GET — determine whether to POST (create) or PUT (replace).
	getReq, _ := http.NewRequest(http.MethodGet, baseURL+"/"+secretName, nil)
	getReq.Header.Set("Authorization", authHeader)
	getResp, err := client.Do(getReq)
	if err != nil {
		return fmt.Errorf("check secret existence: %v", err)
	}
	io.Copy(io.Discard, getResp.Body)
	getResp.Body.Close()

	var method, url string
	if getResp.StatusCode == http.StatusNotFound {
		method, url = http.MethodPost, baseURL
	} else {
		method, url = http.MethodPut, baseURL+"/"+secretName
	}

	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("write secret (%s): %v", method, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("write secret status %d: %s", resp.StatusCode, b)
	}

	log.Printf("[cert-forge] K8s Secret %s/%s written (%d keys)", namespace, secretName, len(data))
	return nil
}
