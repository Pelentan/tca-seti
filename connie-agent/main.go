package main

// ---------------------------------------------------------------------------
// Connie Agent — Constellation Onboarding Agent
//
// Reads remote-apps.json and for each registered constellation:
//  1. Writes the SETI star-gazer public cert as seti-stargazer-cert Secret
//     in the constellation's Kubernetes namespace.
//  2. Fetches plot definitions from the constellation's repository and
//     POSTs them to Plot Store.
//
// Runs a sync on startup and on SYNC_INTERVAL_MINUTES schedule.
// Exposes /health, /status, /status/{tag}, /sync/{tag}.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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
	port                = envOr("PORT", "4014")
	observabilityURL    = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	plotStoreURL        = envOr("PLOT_STORE_URL", "https://plot-store:4005")
	signalAggregatorURL = envOr("SIGNAL_AGGREGATOR_URL", "https://signal-aggregator:4006")
	policyURL           = envOr("POLICY_URL", "https://policy:4003")
	redisURL            = envOr("REDIS_URL", "redis:6379")
	setiInstanceID      = envOr("SETI_INSTANCE_ID", "tca-seti-primary")
	remoteAppsPath      = envOr("REMOTE_APPS_PATH", "/etc/seti/remote-apps.json")
	starGazerCertPath   = envOr("STAR_GAZER_CERT", "/certs/star-gazer.crt")
	syncIntervalMin     = envOrInt("SYNC_INTERVAL_MINUTES", 60)
	silenceThresholdSec = envOrInt("SILENCE_THRESHOLD_SECONDS", 30)
	startTime           = time.Now()
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		fmt.Sscanf(v, "%d", &n)
		if n > 0 {
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// Remote app registry
// ---------------------------------------------------------------------------

type RemoteApp struct {
	Name                string `json:"name"`
	Description         string `json:"description"`
	Namespace           string `json:"namespace"`
	ACEndpoint          string `json:"ac_endpoint"`
	FederationEndpoint  string `json:"federation_endpoint"`
	CaURL               string `json:"ca_url"`
	RegistryURL         string `json:"registry_url"`
	RegistryPlotsPath   string `json:"registry_plots_path"`
	RegistryType        string `json:"registry_type"`
	RegistryToken       string `json:"registry_token"`
	RegistryTokenSecret string `json:"registry_token_secret"`
}

var (
	appsMu sync.RWMutex
	apps   map[string]*RemoteApp
)

// readK8sSecret reads a single key from a Kubernetes Secret using the
// in-cluster service account token and the K8s API server.
func readK8sSecret(namespace, secretName, key string) (string, error) {
	// Read in-cluster service account token
	tokenBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return "", fmt.Errorf("read service account token: %v", err)
	}
	saToken := strings.TrimSpace(string(tokenBytes))

	// K8s API server address from environment
	apiServer := os.Getenv("KUBERNETES_SERVICE_HOST")
	apiPort := os.Getenv("KUBERNETES_SERVICE_PORT")
	if apiServer == "" {
		apiServer = "kubernetes.default.svc"
		apiPort = "443"
	}
	url := fmt.Sprintf("https://%s:%s/api/v1/namespaces/%s/secrets/%s",
		apiServer, apiPort, namespace, secretName)

	// Read cluster CA for TLS verification
	caPool := x509.NewCertPool()
	caData, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return "", fmt.Errorf("read cluster CA: %v", err)
	}
	caPool.AppendCertsFromPEM(caData)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: caPool},
		},
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+saToken)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET secret: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("permission denied reading secret %s/%s — check RBAC", namespace, secretName)
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("secret %s/%s not found", namespace, secretName)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("K8s API returned %d for secret %s/%s", resp.StatusCode, namespace, secretName)
	}

	var secret struct {
		Data map[string][]byte `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&secret); err != nil {
		return "", fmt.Errorf("decode secret: %v", err)
	}

	// K8s stores secret values base64-encoded — Go's JSON decoder decodes them automatically
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %s/%s", key, namespace, secretName)
	}
	return strings.TrimSpace(string(val)), nil
}

func loadRemoteApps() error {
	data, err := os.ReadFile(remoteAppsPath)
	if err != nil {
		return fmt.Errorf("read %s: %v", remoteAppsPath, err)
	}
	raw := map[string]RemoteApp{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse remote-apps.json: %v", err)
	}
	loaded := make(map[string]*RemoteApp, len(raw))
	for tag, app := range raw {
		a := app
		// Default namespace to tag name if not specified
		if a.Namespace == "" {
			a.Namespace = tag
		}
		// Load registry token from K8s Secret if specified
		if a.RegistryTokenSecret != "" && a.RegistryToken == "" {
			token, err := readK8sSecret("seti", a.RegistryTokenSecret, "token")
			if err != nil {
				log.Printf("[connie-agent] %s: could not read registry token secret %q: %v",
					tag, a.RegistryTokenSecret, err)
			} else {
				a.RegistryToken = token
				log.Printf("[connie-agent] %s: registry token loaded from secret %q", tag, a.RegistryTokenSecret)
			}
		}
		loaded[tag] = &a
	}
	appsMu.Lock()
	apps = loaded
	appsMu.Unlock()
	log.Printf("[connie-agent] Loaded %d constellations from remote-apps.json", len(loaded))
	return nil
}

// ---------------------------------------------------------------------------
// Per-constellation sync state
// ---------------------------------------------------------------------------

type SyncState struct {
	mu             sync.Mutex
	Tag            string
	Namespace      string
	CertStatus     string    // ok | pending | failed | unknown
	CertWrittenAt  time.Time
	CertError      string
	PlotStatus     string    // ok | pending | failed | unknown
	PlotSyncedAt   time.Time
	PlotsCount     int
	PlotError      string
	SyncInProgress bool
}

var (
	statesMu sync.RWMutex
	states   map[string]*SyncState
)

func initStates() {
	appsMu.RLock()
	defer appsMu.RUnlock()
	statesMu.Lock()
	defer statesMu.Unlock()
	states = make(map[string]*SyncState, len(apps))
	for tag, app := range apps {
		states[tag] = &SyncState{
			Tag:        tag,
			Namespace:  app.Namespace,
			CertStatus: "unknown",
			PlotStatus: "unknown",
		}
	}
}

func getState(tag string) *SyncState {
	statesMu.RLock()
	defer statesMu.RUnlock()
	return states[tag]
}

// ---------------------------------------------------------------------------
// Sync loop
// ---------------------------------------------------------------------------

func runSyncLoop() {
	// Initial sync on startup
	syncAll()

	// No more periodic re-handshake — federation health is monitored
	// via the Redis channel silence detector in startFederationMonitor.
	// The sync loop now only handles non-federation tasks (cert writes, plot sync).
	ticker := time.NewTicker(time.Duration(syncIntervalMin) * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		syncNonFederation()
	}
}

// syncNonFederation runs everything except federation initiation.
// Federation is handled by startFederationMonitor per constellation.
func syncNonFederation() {
	appsMu.RLock()
	tags := make([]string, 0, len(apps))
	for tag := range apps {
		tags = append(tags, tag)
	}
	appsMu.RUnlock()

	for _, tag := range tags {
		appsMu.RLock()
		app, ok := apps[tag]
		appsMu.RUnlock()
		if !ok {
			continue
		}
		// Step 1: Reload star-gazer key material then write cert.
		// cert-forge may have rotated the star-gazer cert since startup —
		// key must be reloaded before writing so signing stays in sync with
		// whatever cert Vox receives.
		if err := loadStarGazerKey(); err != nil {
			log.Printf("[connie-agent] %s: star-gazer key reload failed: %v", tag, err)
		} else if err := writeStarGazerCert(app.Namespace); err != nil {
			log.Printf("[connie-agent] %s: cert write failed: %v", tag, err)
		}
		// Step 2: Sync plots
		syncPlots(tag, app)
	}
}

// startFederationMonitor starts a per-constellation goroutine that:
//  1. Checks if federation:tag:events has recent traffic
//  2. If not — runs handshake + enrollment
//  3. Subscribes to the channel and watches for silence
//  4. On 30s silence — re-runs handshake + enrollment + publishes warning
func startFederationMonitor(tag string, app *RemoteApp) {
	go func() {
		rdb := NewRedisClient(redisURL)
		channel := "federation:" + tag + ":events"

		for {
			// Always initiate federation on first run — ensures signal-aggregator
			// has the monitor cert regardless of whether the channel has traffic.
			// This handles SA restarts where fedApps state is lost.
			log.Printf("[connie-agent] %s: initiating federation", tag)
			if err := initiateFederation(tag, app); err != nil {
				log.Printf("[connie-agent] %s: federation initiation failed: %v", tag, err)
				time.Sleep(30 * time.Second)
				continue
			}
			activateInPolicy(tag, app)

			// Watch for silence — re-initiate if channel goes quiet
			watchForSilence(rdb, channel, tag, app)
		}
	}()
}

// watchForSilence subscribes to the federation channel and re-initiates
// federation if no message arrives within silenceThresholdSec seconds.
func watchForSilence(rdb *RedisClient, channel, tag string, app *RemoteApp) {
	ctx := context.Background()
	ch, err := rdb.Subscribe(ctx, channel)
	if err != nil {
		log.Printf("[connie-agent] %s: could not subscribe to %s: %v", tag, channel, err)
		return
	}

	timer := time.NewTimer(time.Duration(silenceThresholdSec) * time.Second)
	defer timer.Stop()

	for {
		select {
		case _, ok := <-ch:
			if !ok {
				// Channel closed
				return
			}
			// Reset silence timer on each event
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(time.Duration(silenceThresholdSec) * time.Second)

		case <-timer.C:
			log.Printf("[connie-agent] %s: silence detected on %s — re-initiating federation", tag, channel)
			publishSilenceWarning(tag)
			return // Return to outer loop which will re-initiate
		}
	}
}

func syncAll() {
	appsMu.RLock()
	tags := make([]string, 0, len(apps))
	for tag := range apps {
		tags = append(tags, tag)
	}
	appsMu.RUnlock()

	for _, tag := range tags {
		appsMu.RLock()
		app, ok := apps[tag]
		appsMu.RUnlock()
		if !ok {
			continue
		}

		// Reload star-gazer key then write cert — keeps signing key in sync with
		// whatever cert Vox receives after a cert-forge rotation.
		if err := loadStarGazerKey(); err != nil {
			log.Printf("[connie-agent] %s: star-gazer key reload failed: %v", tag, err)
		} else if err := writeStarGazerCert(app.Namespace); err != nil {
			log.Printf("[connie-agent] %s: cert write failed: %v", tag, err)
		} else {
			log.Printf("[connie-agent] %s: stargazer cert written to namespace %s", tag, app.Namespace)
		}

		// Sync plots
		if count, err := syncPlots(tag, app); err != nil {
			log.Printf("[connie-agent] %s: plot sync failed: %v", tag, err)
		} else {
			log.Printf("[connie-agent] %s: plot sync complete (%d plots)", tag, count)
		}

		// Start federation monitor — handles handshake and silence detection
		if app.ACEndpoint != "" && app.FederationEndpoint != "" {
			startFederationMonitor(tag, app)
		}
	}
}

func syncConstellation(tag string) {
	appsMu.RLock()
	app, ok := apps[tag]
	appsMu.RUnlock()
	if !ok {
		log.Printf("[connie-agent] %s: not found in registry", tag)
		return
	}

	state := getState(tag)
	if state == nil {
		return
	}

	state.mu.Lock()
	if state.SyncInProgress {
		state.mu.Unlock()
		log.Printf("[connie-agent] %s: sync already in progress — skipping", tag)
		return
	}
	state.SyncInProgress = true
	state.mu.Unlock()

	defer func() {
		state.mu.Lock()
		state.SyncInProgress = false
		state.mu.Unlock()
	}()

	log.Printf("[connie-agent] %s: starting sync", tag)

	// Step 1: Reload star-gazer key then write cert
	if err := loadStarGazerKey(); err != nil {
		log.Printf("[connie-agent] %s: star-gazer key reload failed: %v", tag, err)
	} else if err := writeStarGazerCert(app.Namespace); err != nil {
		log.Printf("[connie-agent] %s: cert write failed: %v", tag, err)
		state.mu.Lock()
		state.CertStatus = "failed"
		state.CertError = err.Error()
		state.mu.Unlock()
	} else {
		log.Printf("[connie-agent] %s: stargazer cert written to namespace %s", tag, app.Namespace)
		state.mu.Lock()
		state.CertStatus = "ok"
		state.CertWrittenAt = time.Now()
		state.CertError = ""
		state.mu.Unlock()
	}

	// Step 2: Sync plots
	count, err := syncPlots(tag, app)
	if err != nil {
		log.Printf("[connie-agent] %s: plot sync failed: %v", tag, err)
		state.mu.Lock()
		state.PlotStatus = "failed"
		state.PlotError = err.Error()
		state.mu.Unlock()
	} else {
		log.Printf("[connie-agent] %s: plot sync complete (%d plots)", tag, count)
		state.mu.Lock()
		state.PlotStatus = "ok"
		state.PlotSyncedAt = time.Now()
		state.PlotsCount = count
		state.PlotError = ""
		state.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	appsMu.RLock()
	count := len(apps)
	appsMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":                 "healthy",
		"constellations_loaded":  count,
		"uptime_seconds":         int(time.Since(startTime).Seconds()),
	})
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	statesMu.RLock()
	list := make([]map[string]interface{}, 0, len(states))
	for _, s := range states {
		list = append(list, stateToMap(s))
	}
	statesMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"constellations": list,
		"total":          len(list),
	})
}

func handleStatusConstellation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	tag := strings.TrimPrefix(r.URL.Path, "/status/")
	tag = strings.TrimSuffix(tag, "/")
	state := getState(tag)
	if state == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "NOT_FOUND",
			"message": fmt.Sprintf("constellation %q not found in remote-apps.json", tag),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stateToMap(state))
}

// handleConstellationProxy proxies requests to a remote constellation's AC
// via signal-aggregator which holds the trusted mTLS monitor cert connection.
// Routes:
//   POST /constellations/{tag}/run-contract-tests
//   GET  /constellations/{tag}/contract-suites
//   GET  /constellations/{tag}/contract-suites/{runId}
func handleConstellationProxy(w http.ResponseWriter, r *http.Request) {
	log.Printf("[connie-agent] constellation proxy received: %s %s", r.Method, r.URL.Path)
	// Parse /constellations/{tag}/{action...}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/constellations/"), "/", 2)
	if len(parts) < 2 {
		http.Error(w, `{"error":"missing action"}`, http.StatusBadRequest)
		return
	}
	tag, action := parts[0], parts[1]

	appsMu.RLock()
	app, ok := apps[tag]
	appsMu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"unknown constellation"}`, http.StatusNotFound)
		return
	}

	// Forward to signal-aggregator which holds the trusted mTLS connection to remote AC
	// Special case: request-token goes to remote AC's federation port via connie-agent directly
	if action == "request-token" {
		handleConstellationRequestToken(w, r, tag, app)
		return
	}

	target := signalAggregatorURL + "/federation/proxy/" + tag + "/" + action
	req, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		http.Error(w, `{"error":"could not build request"}`, http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := upstreamClient.Do(req)
	if err != nil {
		log.Printf("[connie-agent] constellation proxy error %s %s: %v", r.Method, target, err)
		http.Error(w, `{"error":"upstream request failed"}`, http.StatusBadGateway)
		return
	}
	log.Printf("[connie-agent] constellation proxy response: %d from %s", resp.StatusCode, target)
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func handleSyncConstellation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	tag := strings.TrimPrefix(r.URL.Path, "/sync/")
	tag = strings.TrimSuffix(tag, "/")

	state := getState(tag)
	if state == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "NOT_FOUND",
			"message": fmt.Sprintf("constellation %q not found", tag),
		})
		return
	}

	state.mu.Lock()
	if state.SyncInProgress {
		state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "SYNC_IN_PROGRESS",
			"message": fmt.Sprintf("sync already running for %q", tag),
		})
		return
	}
	state.mu.Unlock()

	go syncConstellation(tag)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tag":     tag,
		"status":  "accepted",
		"message": "sync started asynchronously — check /status/" + tag,
	})
}

func stateToMap(s *SyncState) map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]interface{}{
		"tag":              s.Tag,
		"namespace":        s.Namespace,
		"cert_status":      s.CertStatus,
		"plot_status":      s.PlotStatus,
		"sync_in_progress": s.SyncInProgress,
	}
	if !s.CertWrittenAt.IsZero() {
		m["cert_last_written_at"] = s.CertWrittenAt.UTC().Format(time.RFC3339)
	}
	if s.CertError != "" {
		m["cert_error"] = s.CertError
	}
	if !s.PlotSyncedAt.IsZero() {
		m["plot_last_synced_at"] = s.PlotSyncedAt.UTC().Format(time.RFC3339)
	}
	if s.PlotsCount > 0 {
		m["plots_synced"] = s.PlotsCount
	}
	if s.PlotError != "" {
		m["plot_error"] = s.PlotError
	}
	return m
}

// initiateFederation calls signal-aggregator POST /federation/subscriptions
// to connect SETI to the constellation's AC SSE stream.
// Idempotent — signal-aggregator returns 200 if already federated.
// cleanupStaleFederations removes federation subscriptions from signal-aggregator
// for any constellations no longer in remote-apps.json.
func cleanupStaleFederations() {
	req, err := http.NewRequest(http.MethodGet,
		signalAggregatorURL+"/federation/subscriptions", nil)
	if err != nil {
		return
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var result struct {
		Subscriptions []struct {
			ApplicationID string `json:"application_id"`
		} `json:"subscriptions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}

	appsMu.RLock()
	currentApps := apps
	appsMu.RUnlock()

	for _, sub := range result.Subscriptions {
		if _, exists := currentApps[sub.ApplicationID]; !exists {
			log.Printf("[connie-agent] Removing stale federation subscription for %s", sub.ApplicationID)
			delReq, err := http.NewRequest(http.MethodDelete,
				signalAggregatorURL+"/federation/subscriptions?application_id="+sub.ApplicationID, nil)
			if err != nil {
				continue
			}
			delResp, err := upstreamClient.Do(delReq)
			if err != nil {
				log.Printf("[connie-agent] Failed to remove federation for %s: %v", sub.ApplicationID, err)
				continue
			}
			delResp.Body.Close()
			log.Printf("[connie-agent] Federation subscription removed for %s", sub.ApplicationID)
		}
	}
}

// publishSilenceWarning publishes a warning to seti:alerts when federation
// silence is detected on a constellation channel.
// handleConstellationRequestToken requests a short-lived token from the remote AC
// using the Star-Gazer signed request over the federation port.
func handleConstellationRequestToken(w http.ResponseWriter, r *http.Request, tag string, app *RemoteApp) {
	caPool, _, err := fetchConstellationCA(app.CaURL)
	if err != nil {
		http.Error(w, `{"error":"could not fetch constellation CA"}`, http.StatusServiceUnavailable)
		return
	}

	// Use the SETI instance cert — registered as monitor cert with remote AC during handshake
	monitorCert := certMat.InstanceCert
	tlsCfg := &tls.Config{
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &monitorCert, nil
		},
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS13,
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
		Timeout:   15 * time.Second,
	}

	tokenURL := app.ACEndpoint + "/auth/session-token"
	req, err := http.NewRequest(http.MethodPost, tokenURL, r.Body)
	if err != nil {
		http.Error(w, `{"error":"could not build token request"}`, http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[connie-agent] %s: token request failed: %v", tag, err)
		http.Error(w, `{"error":"token request failed"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func publishSilenceWarning(tag string) {
	rdb := NewRedisClient(redisURL)
	payload, _ := json.Marshal(map[string]interface{}{
		"type":           "federation_silence",
		"constellation":  tag,
		"message":        "No events received from " + tag + " — re-initiating federation",
		"timestamp":      time.Now().UTC().Format(time.RFC3339),
		"seti_instance":  setiInstanceID,
	})
	if err := rdb.Publish(context.Background(), "seti:alerts", string(payload)); err != nil {
		log.Printf("[connie-agent] %s: failed to publish silence warning: %v", tag, err)
	} else {
		log.Printf("[connie-agent] %s: silence warning published to seti:alerts", tag)
	}
}

func initiateFederation(tag string, app *RemoteApp) error {
	session, err := performHandshake(app.FederationEndpoint, app.CaURL)
	if err != nil {
		return fmt.Errorf("handshake failed: %v", err)
	}
	if err := notifySignalAggregator(tag, app.ACEndpoint, session); err != nil {
		return fmt.Errorf("notify signal-aggregator: %v", err)
	}
	// Federation succeeded — activate the constellation in Policy so it
	// appears in the UI's constellation tab list.
	activateInPolicy(tag, app)
	return nil
}

func activateInPolicy(tag string, app *RemoteApp) {
	body, _ := json.Marshal(map[string]string{
		"name":                app.Name,
		"description":         app.Description,
		"namespace":           app.Namespace,
		"ac_endpoint":         app.ACEndpoint,
		"federation_endpoint": app.FederationEndpoint,
		"ca_url":              app.CaURL,
		"registry_url":        app.RegistryURL,
		"registry_type":       app.RegistryType,
	})
	req, err := http.NewRequest(http.MethodPost,
		policyURL+"/available-applications/"+tag+"/register",
		strings.NewReader(string(body)))
	if err != nil {
		log.Printf("[connie-agent] %s: could not build policy activation request: %v", tag, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := upstreamClient.Do(req)
	if err != nil {
		log.Printf("[connie-agent] %s: policy activation failed: %v", tag, err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		log.Printf("[connie-agent] %s: activated in Policy", tag)
	} else {
		log.Printf("[connie-agent] %s: policy activation returned %d", tag, resp.StatusCode)
	}
}

func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller": "connie-agent", "callee": callee,
			"method": method, "path": path,
			"status_code": status, "latency_ms": latencyMs, "protocol": "mtls",
		})
		req, err := http.NewRequest(http.MethodPost, observabilityURL+"/event",
			strings.NewReader(string(body)))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := upstreamClient.Do(req)
		if err != nil {
			return
		}
		resp.Body.Close()
	}()
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

var certMat *CertMaterial

var upstreamClient *http.Client

func buildUpstreamClient() {
	upstreamClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: buildClientTLS(certMat),
		},
	}
}

func main() {
	certMat = obtainCerts("connie-agent")
	buildUpstreamClient()

	if err := loadStarGazerKey(); err != nil {
		log.Printf("[connie-agent] WARNING: %v — federation will be unavailable", err)
	}

	if err := loadRemoteApps(); err != nil {
		log.Printf("[connie-agent] WARNING: %v — starting with empty registry", err)
		apps = map[string]*RemoteApp{}
	}

	initStates()
	cleanupStaleFederations()
	go selfRegisterWithAC(certMat, "https://connie-agent:4014")
	go runSyncLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/status", handleStatus)
	mux.HandleFunc("/status/", handleStatusConstellation)
	mux.HandleFunc("/sync/", handleSyncConstellation)
	mux.HandleFunc("/constellations/", handleConstellationProxy)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: buildServerTLS(certMat),
	}

	log.Printf("[connie-agent] Listening on :%s (mTLS)", port)
	log.Printf("[connie-agent] Sync interval: %d minutes", syncIntervalMin)

	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[connie-agent] Server error: %v", err)
		}
	}()
	awaitShutdown(server)
}
