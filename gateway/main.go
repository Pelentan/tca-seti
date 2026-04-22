package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

var (
	jwtSecret         = mustReadSecretFile("JWT_SECRET_FILE")
	signalClearanceURL = envOr("SIGNAL_CLEARANCE_URL", "https://signal-clearance:4001")
	observabilityURL  = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	redisURL          = envOr("REDIS_URL", "redis:6379")
	externalPort      = envOr("EXTERNAL_PORT", "4000")
	authMode          = envOr("AUTH_MODE", "dev")
	uiURL             = envOr("UI_URL", "https://ui:4020")
	contractTestURL   = envOr("CONTRACT_TEST_URL", "https://contract-test:4003")
	resultsURL        = envOr("RESULTS_URL", "https://results:4008")
	plotStoreURL      = envOr("PLOT_STORE_URL",  "https://plot-store:4005")
	plotTestURL       = envOr("PLOT_TEST_URL",   "https://plot-test:4004")
	signalAggURL      = envOr("SIGNAL_AGG_URL",  "https://signal-aggregator:4006")
	integrationURL    = envOr("INTEGRATION_URL", "https://integration:4013")
	feedWranglerURL   = envOr("FEED_WRANGLER_URL", "https://feed-wrangler:4007")
	policyURL         = envOr("POLICY_URL",       "https://policy:4002")
	augurCanisURL     = envOr("AUGUR_CANIS_URL",  "https://augur-canis:4010")
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("[gateway] Required env var %s is not set", key)
	}
	return v
}

// mustReadSecretFile reads a secret value from the file path given by the
// named environment variable. Used for secrets written by cert-forge to
// the /certs volume rather than passed as plain env vars.
func mustReadSecretFile(envKey string) []byte {
	path := os.Getenv(envKey)
	if path == "" {
		log.Fatalf("[gateway] Required env var %s is not set", envKey)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("[gateway] Failed to read secret file %s: %v", path, err)
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		log.Fatalf("[gateway] Secret file %s is empty", path)
	}
	return data
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// mTLS client for upstream calls
// ---------------------------------------------------------------------------

var upstreamClient *http.Client

func buildUpstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: buildClientTLS(certMat),
		},
	}
}


// ---------------------------------------------------------------------------
// Observability reporting
// ---------------------------------------------------------------------------

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller":      "gateway",
			"callee":      callee,
			"method":      method,
			"path":        path,
			"status_code": status,
			"latency_ms":  latencyMs,
			"protocol":    "mtls",
		})
		req, err := http.NewRequest(http.MethodPost, observabilityURL+"/event",
			strings.NewReader(string(body)))
		if err != nil {
			log.Printf("[gateway] observability report error: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := upstreamClient.Do(req)
		if err != nil {
			log.Printf("[gateway] observability report error: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

// ---------------------------------------------------------------------------
// JWT claims
// ---------------------------------------------------------------------------

func validateJWT(tokenStr string) (*JWTClaims, error) {
	return VerifyJWT(tokenStr, jwtSecret)
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func servesSPA(w http.ResponseWriter, r *http.Request) {
	// Serve index.html so React Router handles the path client-side
	req, _ := http.NewRequest(http.MethodGet, uiURL+"/", nil)
	resp, err := upstreamClient.Do(req)
	if err != nil {
		http.Error(w, `{"code":"UI_UNAVAILABLE"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.Copy(w, resp.Body)
}

// spaOrAPI wraps a route that conflicts with a React SPA path.
// No Authorization header = browser navigation → serve SPA.
// Authorization header present = API call → apply normal auth.
func spaOrAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			servesSPA(w, r)
			return
		}
		next(w, r)
	}
}

func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			http.Error(w, `{"code":"UNAUTHORIZED","message":"missing bearer token"}`,
				http.StatusUnauthorized)
			return
		}
		claims, err := validateJWT(token)
		if err != nil {
			http.Error(w, `{"code":"UNAUTHORIZED","message":"invalid token"}`,
				http.StatusUnauthorized)
			return
		}
		r.Header.Set("X-Wrangler-ID", claims.WranglerID)
		r.Header.Set("X-Wrangler-Clearance", claims.ClearanceLevel)
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// Proxy helper
// ---------------------------------------------------------------------------

func proxyTo(upstream, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		target := upstream + path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"code":"BAD_REQUEST","message":"failed to read body"}`,
				http.StatusBadRequest)
			return
		}

		req, err := http.NewRequest(r.Method, target, strings.NewReader(string(body)))
		if err != nil {
			http.Error(w, `{"code":"INTERNAL_ERROR","message":"failed to build request"}`,
				http.StatusInternalServerError)
			return
		}
		req.Header = r.Header.Clone()
		req.Header.Set("Content-Type", "application/json")

		resp, err := upstreamClient.Do(req)
		latencyMs := time.Since(start).Milliseconds()

		callee := strings.Split(strings.TrimPrefix(upstream, "https://"), ":")[0]

		if err != nil {
			reportEvent(callee, r.Method, path, 0, latencyMs)
			http.Error(w, `{"code":"UPSTREAM_ERROR","message":"upstream unavailable"}`,
				http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		reportEvent(callee, r.Method, path, resp.StatusCode, latencyMs)

		respBody, _ := io.ReadAll(resp.Body)
		// Copy upstream response headers to the client — critically includes Set-Cookie
		for key, values := range resp.Header {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		// Ensure JSON content type is set (may be overridden by upstream header copy)
		if resp.Header.Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
	}
}

// ---------------------------------------------------------------------------
// SSE — observability event stream from Redis
// ---------------------------------------------------------------------------

type sseClient struct {
	ch chan string
}

var (
	sseMu      sync.RWMutex
	sseClients = map[*sseClient]struct{}{}
)

func addSSEClient(c *sseClient) {
	sseMu.Lock()
	sseClients[c] = struct{}{}
	sseMu.Unlock()
}

func removeSSEClient(c *sseClient) {
	sseMu.Lock()
	delete(sseClients, c)
	sseMu.Unlock()
}

func broadcastSSE(msg string) {
	sseMu.RLock()
	defer sseMu.RUnlock()
	for c := range sseClients {
		select {
		case c.ch <- msg:
		default:
			// Client too slow — drop event rather than block
		}
	}
}

var redisShutdown context.CancelFunc

func startRedisSubscriber() {
	outerCtx, outerCancel := context.WithCancel(context.Background())
	redisShutdown = outerCancel
	go func() {
		rdb := NewRedisClient(redisURL)
		for {
			select {
			case <-outerCtx.Done():
				return
			default:
			}
			ctx, cancel := context.WithCancel(outerCtx)
			ch, err := rdb.Subscribe(ctx, "seti:events")
			if err != nil {
				cancel()
				log.Printf("[gateway] Redis subscribe failed — retrying in 2s: %v", err)
				time.Sleep(2 * time.Second)
				continue
			}
			log.Printf("[gateway] Subscribed to seti:events")
			for msg := range ch {
				broadcastSSE(msg)
			}
			cancel()
			log.Printf("[gateway] seti:events subscription dropped — reconnecting in 2s")
			time.Sleep(2 * time.Second)
		}
	}()
}

func handleSSE(w http.ResponseWriter, r *http.Request) {
	// EventSource cannot set headers — accept JWT via query param
	tokenStr := r.URL.Query().Get("token")
	if tokenStr == "" {
		tokenStr = bearerToken(r)
	}
	if tokenStr == "" {
		http.Error(w, `{"code":"UNAUTHORIZED","message":"missing token"}`, http.StatusUnauthorized)
		return
	}
	if _, err := validateJWT(tokenStr); err != nil {
		http.Error(w, `{"code":"UNAUTHORIZED","message":"invalid token"}`, http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	client := &sseClient{ch: make(chan string, 64)}
	addSSEClient(client)
	defer removeSSEClient(client)

	// Send connected event
	fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"connected\"}\n\n")
	flusher.Flush()

	for {
		select {
		case msg := <-client.ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			// Keepalive ping
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// OIDC redirect and callback — delegates to Signal Clearance
// ---------------------------------------------------------------------------

func handleLoginRedirect(w http.ResponseWriter, r *http.Request) {
	// Proxy the login initiation to Signal Clearance
	// Signal Clearance knows the IdP configuration and builds the redirect URL
	start := time.Now()
	resp, err := upstreamClient.Get(signalClearanceURL + "/auth/oidc/authorize")
	latencyMs := time.Since(start).Milliseconds()
	reportEvent("signal-clearance", "GET", "/auth/oidc/authorize", func() int {
		if err != nil {
			return 0
		}
		return resp.StatusCode
	}(), latencyMs)

	if err != nil {
		log.Printf("[gateway] Failed to get OIDC authorize URL: %v", err)
		http.Error(w, `{"code":"IDP_UNAVAILABLE","message":"IdP unavailable"}`,
			http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		http.Error(w, `{"code":"INTERNAL_ERROR","message":"failed to decode authorize response"}`,
			http.StatusInternalServerError)
		return
	}

	redirectURL, ok := result["redirect_url"]
	if !ok {
		http.Error(w, `{"code":"INTERNAL_ERROR","message":"no redirect_url in response"}`,
			http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, redirectURL, http.StatusFound)
}

func handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	errParam := r.URL.Query().Get("error")

	if errParam != "" {
		log.Printf("[gateway] OIDC callback error: %s", errParam)
		http.Redirect(w, r, "/login?error="+errParam, http.StatusFound)
		return
	}

	// Exchange code for tokens via Signal Clearance
	start := time.Now()
	payload, _ := json.Marshal(map[string]string{"code": code, "state": state})
	req, _ := http.NewRequest(http.MethodPost, signalClearanceURL+"/auth/oidc/callback",
		strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := upstreamClient.Do(req)
	latencyMs := time.Since(start).Milliseconds()
	reportEvent("signal-clearance", "POST", "/auth/oidc/callback", func() int {
		if err != nil {
			return 0
		}
		return resp.StatusCode
	}(), latencyMs)

	if err != nil {
		log.Printf("[gateway] OIDC callback exchange failed: %v", err)
		http.Redirect(w, r, "/login?error=exchange_failed", http.StatusFound)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Redirect(w, r, "/login?error=auth_failed", http.StatusFound)
		return
	}

	var authResult map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&authResult); err != nil {
		http.Redirect(w, r, "/login?error=decode_failed", http.StatusFound)
		return
	}

	jwt, _ := authResult["jwt"].(string)
	refreshToken, _ := authResult["refresh_token"].(string)

	// Store refresh token in httpOnly cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "seti_refresh",
		Value:    refreshToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   7 * 24 * 3600,
	})

	// Redirect to dashboard with JWT as fragment (never hits server)
	http.Redirect(w, r, fmt.Sprintf("/#jwt=%s", jwt), http.StatusFound)
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	sseMu.RLock()
	activeFeeds := len(sseClients)
	sseMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       "healthy",
		"service":      "gateway",
		"active_feeds": activeFeeds,
	})
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

var certMat *CertMaterial

func main() {
	certMat = obtainCerts("gateway")
	upstreamClient = buildUpstreamClient()
	go selfRegisterWithAC(certMat, "https://gateway:4000")
	startRedisSubscriber()

	mux := http.NewServeMux()

	// Health — no auth
	mux.HandleFunc("/health", handleHealth)

	// Auth flow — no JWT required
	mux.HandleFunc("/login", handleLoginRedirect)
	mux.HandleFunc("/auth/callback", handleOIDCCallback)

	// Auth proxy to Signal Clearance — no JWT for login/verify
	mux.HandleFunc("POST /auth/federated", proxyTo(signalClearanceURL, "/auth/federated"))
	mux.HandleFunc("POST /auth/refresh", proxyTo(signalClearanceURL, "/auth/refresh"))
	mux.HandleFunc("POST /auth/logout", requireAuth(proxyTo(signalClearanceURL, "/auth/logout")))

	// Observability SSE feed — auth required
	mux.HandleFunc("/events", handleSSE)

	// Admin — SPA route, no API backend. spaOrAPI serves index.html for browser navigation.
	mux.HandleFunc("/admin", spaOrAPI(requireAuth(func(w http.ResponseWriter, r *http.Request) {
		// /admin is a React SPA route — if somehow reached with auth, serve SPA
		servesSPA(w, r)
	})))
	mux.HandleFunc("/admin/", spaOrAPI(requireAuth(func(w http.ResponseWriter, r *http.Request) {
		servesSPA(w, r)
	})))

	// Dev auth routes — only active when AUTH_MODE=dev
	if authMode == "dev" {
		mux.HandleFunc("GET /auth/dev/login-form", proxyTo(signalClearanceURL, "/auth/dev/login-form"))
		mux.HandleFunc("POST /auth/dev/login", proxyTo(signalClearanceURL, "/auth/dev/login"))
		log.Printf("[gateway] STUB: dev auth endpoints mounted — real OIDC not enforced")
	}

	// Contract test routes — auth required
	mux.HandleFunc("POST /run-contract-test", requireAuth(proxyTo(contractTestURL, "/run")))
	mux.HandleFunc("GET /plot-results", requireAuth(proxyTo(resultsURL, "/plot-results")))
	mux.HandleFunc("POST /plot-results", requireAuth(proxyTo(resultsURL, "/plot-results")))
	mux.HandleFunc("GET /contract-results", requireAuth(proxyTo(resultsURL, "/contract-results")))
	mux.HandleFunc("/contract-results/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		runID := strings.TrimPrefix(r.URL.Path, "/contract-results/")
		if runID == "" {
			proxyTo(resultsURL, "/contract-results")(w, r)
		} else {
			proxyTo(resultsURL, "/contract-results/"+runID)(w, r)
		}
	}))

	// Phase 3 routes — auth required
	mux.HandleFunc("GET /plots", spaOrAPI(requireAuth(proxyTo(plotStoreURL, "/plots"))))
	mux.HandleFunc("POST /plots", requireAuth(proxyTo(plotStoreURL, "/plots")))
	mux.HandleFunc("/plots/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		proxyTo(plotStoreURL, r.URL.Path)(w, r)
	}))
	mux.HandleFunc("POST /run-plot-test", requireAuth(proxyTo(plotTestURL, "/run")))
	mux.HandleFunc("/plot-runs/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		runID := strings.TrimPrefix(r.URL.Path, "/plot-runs/")
		proxyTo(plotTestURL, "/run/"+runID)(w, r)
	}))
	mux.HandleFunc("GET /signal-events", requireAuth(proxyTo(signalAggURL, "/events")))
	mux.HandleFunc("GET /subscriptions", requireAuth(proxyTo(signalAggURL, "/subscriptions")))

	// Feed Wr4ngler routes — auth required
	mux.HandleFunc("/feeds", spaOrAPI(requireAuth(proxyTo(feedWranglerURL, "/feeds"))))
	mux.HandleFunc("/feeds/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		proxyTo(feedWranglerURL, r.URL.Path)(w, r)
	}))

	// Observability rules — auth required
	mux.HandleFunc("/observability/rules", requireAuth(proxyTo(observabilityURL, "/rules")))

	// Augur Canis admin — health, job list, check results (auth required)
	// Exposed for admin UI and Plot Test access to AC state.
	mux.HandleFunc("/augur-canis/health", requireAuth(proxyTo(augurCanisURL, "/health")))
	mux.HandleFunc("/augur-canis/jobs", requireAuth(proxyTo(augurCanisURL, "/jobs")))
	mux.HandleFunc("/augur-canis/checks/recent", requireAuth(proxyTo(augurCanisURL, "/checks/recent")))
	mux.HandleFunc("/augur-canis/configuration", requireAuth(proxyTo(augurCanisURL, "/configuration")))
	mux.HandleFunc("/augur-canis/alerts", requireAuth(proxyTo(augurCanisURL, "/alerts/active")))
	mux.HandleFunc("/augur-canis/run-contract-tests", requireAuth(proxyTo(augurCanisURL, "/run-contract-tests")))
	mux.HandleFunc("/augur-canis/ring/run", requireAuth(proxyTo(augurCanisURL, "/ring/run")))
	mux.HandleFunc("/augur-canis/contract-suites", requireAuth(proxyTo(augurCanisURL, "/contract-suites")))
	mux.HandleFunc("/augur-canis/contract-suites/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		upstream := strings.TrimPrefix(r.URL.Path, "/augur-canis")
		proxyTo(augurCanisURL, upstream)(w, r)
	}))
	mux.HandleFunc("/augur-canis/baselines", requireAuth(proxyTo(augurCanisURL, "/baselines")))
	mux.HandleFunc("/augur-canis/baselines/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		upstream := strings.TrimPrefix(r.URL.Path, "/augur-canis")
		proxyTo(augurCanisURL, upstream)(w, r)
	}))
	mux.HandleFunc("/augur-canis/metrics", requireAuth(proxyTo(augurCanisURL, "/metrics")))
	mux.HandleFunc("/augur-canis/checks/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		upstream := strings.TrimPrefix(r.URL.Path, "/augur-canis")
		proxyTo(augurCanisURL, upstream)(w, r)
	}))

	// Phase 4 routes — auth required
	mux.HandleFunc("/v1/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		proxyTo(integrationURL, r.URL.Path)(w, r)
	}))
	mux.HandleFunc("/ai-providers", requireAuth(proxyTo(policyURL, "/ai-providers")))
	mux.HandleFunc("/applications", requireAuth(proxyTo(policyURL, "/applications")))
	mux.HandleFunc("/applications/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		proxyTo(policyURL, r.URL.Path)(w, r)
	}))
	mux.HandleFunc("/ai-providers/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		proxyTo(policyURL, r.URL.Path)(w, r)
	}))
	// Available applications — all Wr4nglers can read; write actions (register/deregister) are
	// sec-wr4ngler only but enforcement lives in Policy, not here.
	mux.HandleFunc("/available-applications", requireAuth(proxyTo(policyURL, "/available-applications")))
	mux.HandleFunc("/available-applications/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		proxyTo(policyURL, r.URL.Path)(w, r)
	}))

	// All other routes — proxy to UI service (serves the React SPA)
	// The UI handles client-side routing for /dev-login, /, /dashboard, etc.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		target := uiURL + r.URL.RequestURI()
		req, _ := http.NewRequest(r.Method, target, r.Body)
		if req == nil {
			http.Error(w, `{"code":"INTERNAL_ERROR"}`, http.StatusInternalServerError)
			return
		}
		resp, err := upstreamClient.Do(req)
		latencyMs := time.Since(start).Milliseconds()

		if err != nil {
			reportEvent("ui", r.Method, r.URL.Path, 0, latencyMs)
			http.Error(w, `{"code":"UI_UNAVAILABLE","message":"UI service unavailable"}`,
				http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		reportEvent("ui", r.Method, r.URL.Path, resp.StatusCode, latencyMs)

		// Copy headers
		for k, v := range resp.Header {
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}
		w.WriteHeader(resp.StatusCode)

		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	})

	// External TLS server (browser-facing)
	// External TLS — browsers don't present client certs, use instance cert for server identity
	server := &http.Server{
		Addr:    ":" + externalPort,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{certMat.InstanceCert},
			MinVersion:   tls.VersionTLS12, // Browsers need 1.2 compatibility
		},
	}

	log.Printf("[gateway] Listening on :%s (TLS external, mTLS upstream)", externalPort)
	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[gateway] Server error: %v", err)
		}
	}()
	awaitShutdown(server)
}
