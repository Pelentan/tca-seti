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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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

// WriteTLSSecret writes a kubernetes.io/tls typed Secret with the standard
// tls.crt and tls.key keys. Traefik requires this type for TLS termination —
// it will not accept Opaque secrets for its own certificate material.
func WriteTLSSecret(secretName string, certPEM, keyPEM []byte) error {
	namespace := k8sNamespace()
	client, err := k8sHTTPClient()
	if err != nil {
		return err
	}
	token, err := k8sToken()
	if err != nil {
		return err
	}

	secret := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      secretName,
			"namespace": namespace,
		},
		"type": "kubernetes.io/tls",
		"data": map[string]string{
			"tls.crt": base64.StdEncoding.EncodeToString(certPEM),
			"tls.key": base64.StdEncoding.EncodeToString(keyPEM),
		},
	}

	body, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("marshal TLS secret: %v", err)
	}

	authHeader := "Bearer " + token
	baseURL := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets", k8sAPIBase, namespace)

	getReq, _ := http.NewRequest(http.MethodGet, baseURL+"/"+secretName, nil)
	getReq.Header.Set("Authorization", authHeader)
	getResp, err := client.Do(getReq)
	if err != nil {
		return fmt.Errorf("check TLS secret existence: %v", err)
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
		return fmt.Errorf("write TLS secret (%s): %v", method, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("write TLS secret status %d: %s", resp.StatusCode, b)
	}

	log.Printf("[cert-forge] K8s TLS Secret %s/%s written", namespace, secretName)
	return nil
}

// PatchDeploymentRestart patches the kubectl.kubernetes.io/restartedAt
// annotation on a Deployment, triggering a rolling restart. This is the
// same mechanism kubectl rollout restart uses — no separate controller needed.
func PatchDeploymentRestart(name, namespace string) error {
	client, err := k8sHTTPClient()
	if err != nil {
		return err
	}
	token, err := k8sToken()
	if err != nil {
		return err
	}

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]string{
						"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format(time.RFC3339),
					},
				},
			},
		},
	}

	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %v", err)
	}

	url := fmt.Sprintf("%s/apis/apps/v1/namespaces/%s/deployments/%s",
		k8sAPIBase, namespace, name)

	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build patch request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("patch deployment: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("patch deployment status %d: %s", resp.StatusCode, b)
	}

	log.Printf("[cert-forge] Deployment %s/%s restart triggered", namespace, name)
	return nil
}

// ---------------------------------------------------------------------------
// Symmetric secrets — postgres password and JWT signing secret.
//
// Generated once on first startup and persisted in the K8s Secret or on
// the certs volume. Loaded from storage on every subsequent startup so
// they survive cert-forge restarts without changing value.
//
// Both are 32 random bytes encoded as lowercase hex (64 characters).
// ---------------------------------------------------------------------------

// loadOrGenerateSymmetricSecrets returns the postgres password and JWT secret,
// loading from storage if they already exist or generating fresh values if not.
func loadOrGenerateSymmetricSecrets(inK8s bool, secretName string) (postgresPassword, jwtSecret []byte, err error) {
	if inK8s {
		// Try to load existing values from the Secret
		existing, err := ReadK8sSecret(secretName)
		if err != nil {
			log.Printf("[cert-forge] Could not read existing Secret for symmetric secrets: %v — generating fresh", err)
		}
		if existing != nil {
			if v, ok := existing["postgres-password"]; ok && len(v) > 0 {
				postgresPassword = v
				log.Printf("[cert-forge] Loaded existing postgres-password from Secret")
			}
			if v, ok := existing["jwt-secret"]; ok && len(v) > 0 {
				jwtSecret = v
				log.Printf("[cert-forge] Loaded existing jwt-secret from Secret")
			}
		}
	} else {
		// Docker Compose: try to load from files on the certs volume
		pgPath := filepath.Join(outputDir, "postgres-password")
		jwtPath := filepath.Join(outputDir, "jwt-secret")

		if data, err := os.ReadFile(pgPath); err == nil && len(data) > 0 {
			postgresPassword = data
			log.Printf("[cert-forge] Loaded existing postgres-password from volume")
		}
		if data, err := os.ReadFile(jwtPath); err == nil && len(data) > 0 {
			jwtSecret = data
			log.Printf("[cert-forge] Loaded existing jwt-secret from volume")
		}
	}

	// Generate any missing values
	if len(postgresPassword) == 0 {
		postgresPassword, err = generateSymmetricSecret()
		if err != nil {
			return nil, nil, fmt.Errorf("generate postgres-password: %w", err)
		}
		log.Printf("[cert-forge] Generated new postgres-password")
	}
	if len(jwtSecret) == 0 {
		jwtSecret, err = generateSymmetricSecret()
		if err != nil {
			return nil, nil, fmt.Errorf("generate jwt-secret: %w", err)
		}
		log.Printf("[cert-forge] Generated new jwt-secret")
	}

	return postgresPassword, jwtSecret, nil
}

// generateSymmetricSecret returns 32 cryptographically random bytes as
// a lowercase hex string (64 characters). Suitable for passwords and
// HMAC signing secrets.
func generateSymmetricSecret() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("rand: %w", err)
	}
	return []byte(hex.EncodeToString(raw)), nil
}
