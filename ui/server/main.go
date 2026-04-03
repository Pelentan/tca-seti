package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	gatewayURL = envOr("GATEWAY_URL", "https://seti.cluster.internal")
	port       = envOr("PORT", "4020")
	startTime  = time.Now()
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadServerTLS() *tls.Config {
	// Retry cert loading — cert-init may not have flushed to the volume yet
	var caCert []byte
	var err error
	for i := 0; i < 10; i++ {
		caCert, err = os.ReadFile("/certs/ca.crt")
		if err == nil {
			break
		}
		log.Printf("[ui] Waiting for certs (%d/10): %v", i+1, err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("[ui] CA cert not found after retries: %v", err)
	}

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)

	cert, err := tls.LoadX509KeyPair("/certs/ui.crt", "/certs/ui.key")
	if err != nil {
		log.Fatalf("[ui] Service cert not found: %v", err)
	}

	return &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
}

func main() {
	mux := http.NewServeMux()

	// Health endpoint — mTLS protected like all other endpoints
	mux.HandleFunc("/health", handleHealth)

	// Static assets — served from /app/dist
	fs := http.FileServer(http.Dir("/app/dist"))

	// All routes: assets served directly, SPA routes serve index.html
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ".") {
			fs.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, "/app/dist/index.html")
	})

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: loadServerTLS(),
	}

	log.Printf("[ui] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[ui] Gateway URL: %s", gatewayURL)
	log.Printf("[ui] Zero open ports — all connections require client certificate")

	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("[ui] Server error: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":         "healthy",
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}
