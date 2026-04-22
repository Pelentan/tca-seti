package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Plot Store — owns the Plot database.
//
// A Plot is an AI-generated test scenario: a sequence of steps each defining
// an HTTP action to execute, expected call chains to verify via Signal
// Aggregator, and expected response shape. Plot Test loads Plots from here.
//
// Phase 3: in-memory storage.
// Swap point: replace with PostgreSQL in Phase 4.
// ---------------------------------------------------------------------------

var (
	port             = envOr("PORT", "4005")
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
	plotsPath        = envOr("PLOTS_PATH", "/contracts/plots")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Plot model
// ---------------------------------------------------------------------------

// PlotCall mirrors the contract PlotCall schema — the HTTP action for a step.
type PlotCall struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    interface{}       `json:"body,omitempty"`
}

// PlotAssertion defines a semantic assertion on a step's response body.
type PlotAssertion struct {
	Field    string      `json:"field"`
	Operator string      `json:"operator"` // equals, not_equals, contains, exists, not_exists, greater_than, less_than
	Value    interface{} `json:"value,omitempty"`
}

// CaptureDefinition extracts a value from the response for use in later steps.
type CaptureDefinition struct {
	Name string `json:"name"` // referenced as {name} in subsequent steps
	Path string `json:"path"` // dot-notation into the response body
}

// ExpectedCall defines an inter-service call expected in the observability stream.
type ExpectedCall struct {
	Caller         string `json:"caller"`
	Callee         string `json:"callee"`
	Method         string `json:"method,omitempty"`
	Path           string `json:"path,omitempty"`
	MinOccurrences int    `json:"min_occurrences,omitempty"`
}

// PlotStep supports both the contract schema (Call wrapper) and the legacy flat format.
// Normalize() must be called before execution to resolve whichever format is present.
type PlotStep struct {
	StepNumber  int    `json:"step_number"`
	Description string `json:"description"`

	// Contract schema — preferred
	Call             *PlotCall           `json:"call,omitempty"`
	ExpectStatus     int                 `json:"expect_status,omitempty"`
	Assertions       []PlotAssertion     `json:"assertions,omitempty"`
	Capture          []CaptureDefinition `json:"capture,omitempty"`
	ExpectCallChain  []ExpectedCall      `json:"expect_call_chain,omitempty"`

	// Legacy flat format — still accepted for backward compatibility
	Method         string            `json:"method,omitempty"`
	Path           string            `json:"path,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Body           interface{}       `json:"body,omitempty"`
	ExpectedStatus int               `json:"expected_status,omitempty"`
	ExpectedChain  []ExpectedCall    `json:"expected_chain,omitempty"`
	VerifyWithin   int               `json:"verify_within_seconds,omitempty"`
}

// Normalize resolves the dual-format PlotStep into canonical fields.
// After calling this, Method/Path/Headers/Body/ExpectedStatus/ExpectedChain
// are always populated regardless of which format the JSON used.
func (s *PlotStep) Normalize(captures map[string]string) {
	// Resolve call wrapper → flat fields
	if s.Call != nil {
		s.Method = s.Call.Method
		s.Path = s.Call.Path
		if s.Call.Headers != nil {
			s.Headers = s.Call.Headers
		}
		if s.Call.Body != nil {
			s.Body = s.Call.Body
		}
	}
	// Resolve expect_status → expected_status
	if s.ExpectStatus != 0 && s.ExpectedStatus == 0 {
		s.ExpectedStatus = s.ExpectStatus
	}
	// Resolve expect_call_chain → expected_chain
	if len(s.ExpectCallChain) > 0 && len(s.ExpectedChain) == 0 {
		s.ExpectedChain = s.ExpectCallChain
	}
	// Apply capture substitutions to path and body
	if len(captures) > 0 {
		s.Path = applyCaptures(s.Path, captures)
		if bodyStr, ok := s.Body.(string); ok {
			s.Body = applyCaptures(bodyStr, captures)
		}
	}
}

// applyCaptures replaces {name} tokens in a string with captured values.
func applyCaptures(s string, captures map[string]string) string {
	for k, v := range captures {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

type Plot struct {
	PlotID        string     `json:"plot_id"`
	ApplicationID string     `json:"application_id"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	Version       string     `json:"version"`
	Author        string     `json:"author"` // "ai-lien" or wrangler ID
	Steps         []PlotStep `json:"steps"`
	Tags          []string   `json:"tags,omitempty"`
	Flagged       bool       `json:"flagged"`
	FlagReason    string     `json:"flag_reason,omitempty"`
	CreatedAt     string     `json:"created_at"`
	UpdatedAt     string     `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// In-memory store — Phase 3
// Swap point: replace with PostgreSQL in Phase 4
// ---------------------------------------------------------------------------

var (
	mu     sync.RWMutex
	plots  = map[string]*Plot{}
	nextID atomic.Int64
)

func generateID() string {
	return fmt.Sprintf("plot-%d", time.Now().UnixNano())
}

// ---------------------------------------------------------------------------
// mTLS
// ---------------------------------------------------------------------------

var upstreamClient *http.Client

func buildUpstreamClient() {
	upstreamClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: buildClientTLS(certMat),
		},
	}
}


func reportEvent(callee, method, path string, status int, latencyMs int64) {
	go func() {
		body, _ := json.Marshal(map[string]interface{}{
			"caller": "plot-store", "callee": callee,
			"method": method, "path": path,
			"status_code": status, "latency_ms": latencyMs, "protocol": "mtls",
		})
		req, _ := http.NewRequest(http.MethodPost, observabilityURL+"/event", strings.NewReader(string(body)))
		if req == nil {
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
// Handlers
// ---------------------------------------------------------------------------

func handlePlots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()

	switch r.Method {
	case http.MethodGet:
		appFilter := r.URL.Query().Get("application_id")
		mu.RLock()
		result := make([]*Plot, 0, len(plots))
		for _, p := range plots {
			if appFilter != "" && p.ApplicationID != appFilter {
				continue
			}
			result = append(result, p)
		}
		mu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"plots": result, "total": len(result)})
		reportEvent("self", "GET", "/plots", 200, time.Since(start).Milliseconds())

	case http.MethodPost:
		var req Plot
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": err.Error()})
			return
		}
		if req.ApplicationID == "" || req.Name == "" || len(req.Steps) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": "application_id, name, and at least one step required"})
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		req.PlotID = generateID()
		req.CreatedAt = now
		req.UpdatedAt = now

		mu.Lock()
		plots[req.PlotID] = &req
		mu.Unlock()

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(req)
		log.Printf("[plot-store] Plot created: %s (%s) for %s — %d steps", req.PlotID, req.Name, req.ApplicationID, len(req.Steps))
		reportEvent("self", "POST", "/plots", 201, time.Since(start).Milliseconds())

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handlePlot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/plots/"), "/")
	plotID := parts[0]

	if len(parts) > 1 && parts[1] == "flag" && r.Method == http.MethodPost {
		var req struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		p, exists := plots[plotID]
		if exists {
			p.Flagged = true
			p.FlagReason = req.Reason
			p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		mu.Unlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("plot %s not found", plotID)})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "plot flagged"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		mu.RLock()
		p, exists := plots[plotID]
		mu.RUnlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("plot %s not found", plotID)})
			return
		}
		json.NewEncoder(w).Encode(p)
		reportEvent("self", "GET", "/plots/"+plotID, 200, time.Since(start).Milliseconds())

	case http.MethodPut:
		var req Plot
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": err.Error()})
			return
		}
		mu.Lock()
		existing, exists := plots[plotID]
		if !exists {
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("plot %s not found", plotID)})
			return
		}
		req.PlotID = plotID
		req.CreatedAt = existing.CreatedAt
		req.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		plots[plotID] = &req
		mu.Unlock()
		json.NewEncoder(w).Encode(req)

	case http.MethodDelete:
		mu.Lock()
		_, exists := plots[plotID]
		if exists {
			delete(plots, plotID)
		}
		mu.Unlock()
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": fmt.Sprintf("plot %s not found", plotID)})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": fmt.Sprintf("plot %s deleted", plotID)})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleAppPlots(w http.ResponseWriter, r *http.Request) {
	appID := strings.TrimPrefix(r.URL.Path, "/applications/")
	appID = strings.TrimSuffix(appID, "/plots")

	mu.RLock()
	result := make([]*Plot, 0)
	for _, p := range plots {
		if p.ApplicationID == appID {
			result = append(result, p)
		}
	}
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"plots": result, "total": len(result), "application_id": appID})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	count := len(plots)
	mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "healthy", "plots_stored": count,
		"storage": "in-memory (Phase 3 — swap PostgreSQL in Phase 4)",
		"uptime_seconds": int(time.Since(startTime).Seconds()),
	})
}

var startTime = time.Now()

func loadServerTLS() *tls.Config {
	return buildServerTLS(certMat)
}

func loadPlotsFromDisk() {
	appDir := filepath.Join(plotsPath)
	entries, err := os.ReadDir(appDir)
	if err != nil {
		log.Printf("[plot-store] No plots directory found at %s — starting empty", plotsPath)
		return
	}

	loaded := 0
	for _, appEntry := range entries {
		if !appEntry.IsDir() {
			continue
		}
		appID := appEntry.Name()
		plotFiles, err := os.ReadDir(filepath.Join(appDir, appID))
		if err != nil {
			continue
		}

		for _, pf := range plotFiles {
			if !strings.HasSuffix(pf.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(appDir, appID, pf.Name()))
			if err != nil {
				log.Printf("[plot-store] Failed to read %s: %v", pf.Name(), err)
				continue
			}
			var p Plot
			if err := json.Unmarshal(data, &p); err != nil {
				log.Printf("[plot-store] Failed to parse %s: %v", pf.Name(), err)
				continue
			}
			if p.ApplicationID == "" {
				p.ApplicationID = appID
			}
			now := time.Now().UTC().Format(time.RFC3339)
			p.PlotID = generateID()
			p.CreatedAt = now
			p.UpdatedAt = now
			mu.Lock()
			plots[p.PlotID] = &p
			mu.Unlock()
			loaded++
			log.Printf("[plot-store] Loaded plot: %s (%s)", p.Name, pf.Name())
		}
	}
	log.Printf("[plot-store] Loaded %d plots from %s", loaded, plotsPath)
}

var certMat *CertMaterial

// ---------------------------------------------------------------------------
// Registry ingest — called by Policy on application registration
// ---------------------------------------------------------------------------

type IngestRequest struct {
	RegistryURL   string `json:"registry_url"`
	RegistryType  string `json:"registry_type"`
	RegistryToken string `json:"registry_token"`
}

// PlotSubmission mirrors the plot-store.yaml PlotSubmission schema —
// the JSON format written by the AI partner into contracts/plots/.
type PlotSubmission struct {
	ApplicationID   string       `json:"application_id"`
	JobName         string       `json:"job_name"`
	PlotName        string       `json:"plot_name"`
	ContractVersion string       `json:"contract_version"`
	Description     string       `json:"description"`
	GeneratedBy     string       `json:"generated_by"`
	Steps           []PlotStep   `json:"steps"`
}

func registryFetch(req IngestRequest, path string) ([]byte, error) {
	url := strings.TrimRight(req.RegistryURL, "/") + "/" + strings.TrimLeft(path, "/")
	httpReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	switch req.RegistryType {
	case "gitlab":
		httpReq.Header.Set("PRIVATE-TOKEN", req.RegistryToken)
	default: // github, generic
		httpReq.Header.Set("Authorization", "Bearer "+req.RegistryToken)
	}
	httpReq.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("registry returned HTTP %d for %s", resp.StatusCode, path)
	}
	buf := make([]byte, 0, 32768)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

// registryListPlots returns a list of {name, download_url} for files
// in contracts/plots/. GitHub and GitLab both return a JSON array of
// file objects; generic endpoints are expected to do the same.
type registryFile struct {
	Name        string `json:"name"`
	Type        string `json:"type"`        // "file" (GitHub) or "blob" (GitLab)
	DownloadURL string `json:"download_url"` // GitHub
	RawURL      string `json:"raw_url"`      // GitLab fallback
	ContentURL  string `json:"content_url"`  // generic
}

func (f registryFile) fetchURL() string {
	if f.DownloadURL != "" {
		return f.DownloadURL
	}
	if f.RawURL != "" {
		return f.RawURL
	}
	return f.ContentURL
}

func fetchFileContent(req IngestRequest, downloadURL string) ([]byte, error) {
	httpReq, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	switch req.RegistryType {
	case "gitlab":
		httpReq.Header.Set("PRIVATE-TOKEN", req.RegistryToken)
	default:
		httpReq.Header.Set("Authorization", "Bearer "+req.RegistryToken)
	}
	// Request raw content, not JSON-wrapped base64
	httpReq.Header.Set("Accept", "application/vnd.github.raw+json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d fetching file", resp.StatusCode)
	}
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	return buf, nil
}

func handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"code":"METHOD_NOT_ALLOWED"}`, http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()

	tag := strings.TrimPrefix(r.URL.Path, "/ingest/")
	tag = strings.TrimSuffix(tag, "/")
	if tag == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_REQUEST", "message": "tag required in path"})
		return
	}

	var req IngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.RegistryURL == "" || req.RegistryToken == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "INVALID_REQUEST",
			"message": "registry_url, registry_type, and registry_token required",
		})
		return
	}
	if req.RegistryType == "" {
		req.RegistryType = "generic"
	}

	// List contracts/plots/ directory
	listing, err := registryFetch(req, "contracts/plots")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "REGISTRY_UNREACHABLE",
			"message": err.Error(),
		})
		reportEvent("registry:"+tag, "GET", "contracts/plots", 502, time.Since(start).Milliseconds())
		return
	}
	reportEvent("registry:"+tag, "GET", "contracts/plots", 200, time.Since(start).Milliseconds())

	var files []registryFile
	if err := json.Unmarshal(listing, &files); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{
			"code":    "REGISTRY_PARSE_ERROR",
			"message": "could not parse registry directory listing",
		})
		return
	}

	ingested, updated, skipped := 0, 0, 0
	var skipReasons []string
	now := time.Now().UTC().Format(time.RFC3339)

	for _, f := range files {
		if f.Type != "file" && f.Type != "blob" {
			continue
		}
		if !strings.HasSuffix(f.Name, ".json") {
			continue // plots are JSON files in contracts/plots/
		}

		dlURL := f.fetchURL()
		if dlURL == "" {
			skipped++
			skipReasons = append(skipReasons, fmt.Sprintf("%s: no download URL", f.Name))
			continue
		}

		content, err := fetchFileContent(req, dlURL)
		if err != nil {
			skipped++
			skipReasons = append(skipReasons, fmt.Sprintf("%s: fetch failed: %v", f.Name, err))
			continue
		}

		var sub PlotSubmission
		if err := json.Unmarshal(content, &sub); err != nil {
			skipped++
			skipReasons = append(skipReasons, fmt.Sprintf("%s: invalid JSON: %v", f.Name, err))
			continue
		}
		if sub.JobName == "" || sub.PlotName == "" || len(sub.Steps) == 0 {
			skipped++
			skipReasons = append(skipReasons, fmt.Sprintf("%s: missing job_name, plot_name, or steps", f.Name))
			continue
		}

		// Use the tag as application_id if the submission doesn't specify one
		appID := sub.ApplicationID
		if appID == "" {
			appID = "app-" + tag
		}
		plotName := sub.JobName + "/" + sub.PlotName

		mu.Lock()
		// Check for existing plot with same app+name — version it if found
		var existingID string
		for id, p := range plots {
			if p.ApplicationID == appID && p.Name == plotName {
				existingID = id
				break
			}
		}
		if existingID != "" {
			plots[existingID].Flagged = true
			plots[existingID].FlagReason = "superseded by ingest version"
			plots[existingID].UpdatedAt = now
			updated++
		} else {
			ingested++
		}

		newPlot := &Plot{
			PlotID:        generateID(),
			ApplicationID: appID,
			Name:          plotName,
			Description:   sub.Description,
			Version:       sub.ContractVersion,
			Author:        sub.GeneratedBy,
			Steps:         sub.Steps,
			Flagged:       false,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		plots[newPlot.PlotID] = newPlot
		mu.Unlock()

		log.Printf("[plot-store] Ingested plot %s/%s for %s", sub.JobName, sub.PlotName, appID)
	}

	result := map[string]interface{}{
		"tag":          tag,
		"ingested":     ingested,
		"updated":      updated,
		"skipped":      skipped,
		"ingested_at":  now,
	}
	if len(skipReasons) > 0 {
		result["skip_reasons"] = skipReasons
	}

	log.Printf("[plot-store] Ingest complete for %s: %d new, %d updated, %d skipped",
		tag, ingested, updated, skipped)
	reportEvent("registry:"+tag, "POST", "/ingest/"+tag, 200, time.Since(start).Milliseconds())
	json.NewEncoder(w).Encode(result)
}

func main() {
	certMat = obtainCerts("plot-store")
	go selfRegisterWithAC(certMat, "https://plot-store:4005")
	buildUpstreamClient()
	loadPlotsFromDisk()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/plots", handlePlots)
	mux.HandleFunc("/plots/", handlePlot)
	mux.HandleFunc("/applications/", handleAppPlots)
	mux.HandleFunc("/ingest/", handleIngest)

	server := &http.Server{Addr: ":" + port, Handler: mux, TLSConfig: loadServerTLS()}
	log.Printf("[plot-store] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[plot-store] Storage: in-memory (Phase 3 — swap PostgreSQL in Phase 4)")

	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[plot-store] Server error: %v", err)
		}
	}()
	awaitShutdown(server)
}
