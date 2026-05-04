package main

// ---------------------------------------------------------------------------
// K8s Secret writer — cross-namespace
//
// Writes the SETI star-gazer public certificate as a Secret named
// seti-stargazer-cert in the target constellation namespace.
//
// Uses the in-cluster ServiceAccount token and the K8s API server.
// Requires a ClusterRole or Role in the target namespace with:
//   - secrets: get, create, update
//
// Stdlib only — no external K8s client library.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

const (
	k8sAPIBase      = "https://kubernetes.default.svc"
	saTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	starGazerSecret = "seti-stargazer-cert"
)

// k8sHTTPClient returns an HTTP client that trusts the cluster CA.
func k8sHTTPClient() (*http.Client, error) {
	caCert, err := os.ReadFile(saCAPath)
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("parse cluster CA cert")
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
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

// writeStarGazerCert reads the star-gazer public cert from disk and writes
// it as seti-stargazer-cert Secret in the target namespace. Idempotent —
// creates on first write, replaces on subsequent writes.
func writeStarGazerCert(targetNamespace string) error {
	certPEM, err := os.ReadFile(starGazerCertPath)
	if err != nil {
		return fmt.Errorf("read star-gazer cert from %s: %v", starGazerCertPath, err)
	}

	client, err := k8sHTTPClient()
	if err != nil {
		return fmt.Errorf("k8s client: %v", err)
	}
	token, err := k8sToken()
	if err != nil {
		return fmt.Errorf("k8s token: %v", err)
	}

	secret := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      starGazerSecret,
			"namespace": targetNamespace,
			"labels": map[string]string{
				"managed-by": "connie-agent",
				"seti-cert":  "stargazer",
			},
		},
		"type": "kubernetes.io/tls",
		"data": map[string]string{
			"tls.crt": base64.StdEncoding.EncodeToString(certPEM),
			// tls.key is intentionally absent — we only distribute the public cert.
			// K8s requires tls.key for kubernetes.io/tls type so use Opaque instead.
		},
	}

	// Use Opaque type since we only have the public cert, not the key.
	secret["type"] = "Opaque"

	body, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("marshal secret: %v", err)
	}

	authHeader := "Bearer " + token
	baseURL := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets", k8sAPIBase, targetNamespace)

	// Check if Secret already exists
	getReq, _ := http.NewRequest(http.MethodGet, baseURL+"/"+starGazerSecret, nil)
	getReq.Header.Set("Authorization", authHeader)
	getResp, err := client.Do(getReq)
	if err != nil {
		return fmt.Errorf("check secret existence in %s: %v", targetNamespace, err)
	}
	io.Copy(io.Discard, getResp.Body)
	getResp.Body.Close()

	var method, url string
	if getResp.StatusCode == http.StatusNotFound {
		method, url = http.MethodPost, baseURL
	} else {
		method, url = http.MethodPut, baseURL+"/"+starGazerSecret
	}

	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("write secret to %s (%s): %v", targetNamespace, method, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("write secret to %s: status %d: %s", targetNamespace, resp.StatusCode, b)
	}

	log.Printf("[connie-agent] seti-stargazer-cert written to namespace %s", targetNamespace)
	return nil
}
