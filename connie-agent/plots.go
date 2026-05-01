package main

// ---------------------------------------------------------------------------
// Plot synchronisation
//
// Fetches plot JSON files from the constellation's repository and POSTs
// them to SETI's Plot Store. connie-agent owns all repository connectivity;
// Plot Store has no registry credentials and makes no outbound calls.
//
// Supported repository types:
//   github  — GitHub Contents API (api.github.com)
//   gitlab  — GitLab Repository Files API
//   generic — any endpoint returning a JSON array of {name, download_url}
//
// Plot files must be JSON with a plot_id field at minimum. connie-agent
// does not validate the full schema — Plot Store validates on receipt.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const plotsSubPath = "contracts/plots"

// repoFile represents one file entry from the repository listing API.
type repoFile struct {
	Name        string `json:"name"`
	Type        string `json:"type"`         // "file" (GitHub) or "blob" (GitLab)
	DownloadURL string `json:"download_url"` // GitHub
	RawURL      string `json:"raw_url"`      // GitLab
	ContentURL  string `json:"content_url"`  // generic
}

func (f repoFile) fetchURL() string {
	if f.DownloadURL != "" {
		return f.DownloadURL
	}
	if f.RawURL != "" {
		return f.RawURL
	}
	return f.ContentURL
}

func authHeader(app *RemoteApp) string {
	switch app.RegistryType {
	case "gitlab":
		return "PRIVATE-TOKEN " + app.RegistryToken
	default:
		return "Bearer " + app.RegistryToken
	}
}

// listPlotFiles returns all .json files in contracts/plots/ from the repository.
func listPlotFiles(app *RemoteApp) ([]repoFile, error) {
	url := strings.TrimRight(app.RegistryURL, "/") + "/" + plotsSubPath
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if app.RegistryToken != "" {
		req.Header.Set("Authorization", authHeader(app))
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list plots from %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No plots directory — not an error, just nothing to sync
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("registry returned HTTP %d for %s", resp.StatusCode, url)
	}

	var files []repoFile
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return nil, fmt.Errorf("parse file listing: %v", err)
	}

	// Filter to .json files only
	result := make([]repoFile, 0, len(files))
	for _, f := range files {
		if strings.HasSuffix(f.Name, ".json") &&
			(f.Type == "file" || f.Type == "blob" || f.Type == "") {
			result = append(result, f)
		}
	}
	return result, nil
}

// fetchFileContent downloads the raw content of a single plot file.
func fetchFileContent(app *RemoteApp, downloadURL string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	if app.RegistryToken != "" {
		req.Header.Set("Authorization", authHeader(app))
	}
	// Request raw content (GitHub sends base64-wrapped JSON otherwise)
	req.Header.Set("Accept", "application/vnd.github.raw+json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %v", downloadURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, downloadURL)
	}

	return io.ReadAll(resp.Body)
}

// postToPlotStore sends a single plot JSON payload to Plot Store's POST /plots.
func postToPlotStore(plotJSON []byte) error {
	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, plotStoreURL+"/plots",
		bytes.NewReader(plotJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := upstreamClient.Do(req)
	latency := time.Since(start).Milliseconds()
	reportEvent("plot-store", "POST", "/plots", 0, latency)
	if err != nil {
		return fmt.Errorf("post to plot-store: %v", err)
	}
	defer resp.Body.Close()
	reportEvent("plot-store", "POST", "/plots", resp.StatusCode, latency)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("plot-store returned %d: %s", resp.StatusCode, b)
	}
	return nil
}

// syncPlots fetches all plots from the constellation repo and posts them
// to Plot Store. Returns the count of successfully synced plots.
func syncPlots(tag string, app *RemoteApp) (int, error) {
	if app.RegistryURL == "" {
		return 0, nil // no registry configured — skip silently
	}

	files, err := listPlotFiles(app)
	if err != nil {
		return 0, fmt.Errorf("list plot files: %v", err)
	}
	if len(files) == 0 {
		return 0, nil
	}

	synced := 0
	for _, f := range files {
		fetchURL := f.fetchURL()
		if fetchURL == "" {
			log.Printf("[connie-agent] %s: no download URL for %s — skipping", tag, f.Name)
			continue
		}

		content, err := fetchFileContent(app, fetchURL)
		if err != nil {
			log.Printf("[connie-agent] %s: failed to fetch %s: %v", tag, f.Name, err)
			continue
		}

		// Quick validation — must be a JSON object with plot_id
		var check map[string]interface{}
		if err := json.Unmarshal(content, &check); err != nil {
			log.Printf("[connie-agent] %s: %s is not valid JSON — skipping", tag, f.Name)
			continue
		}

		if err := postToPlotStore(content); err != nil {
			log.Printf("[connie-agent] %s: failed to post %s to plot-store: %v", tag, f.Name, err)
			continue
		}

		log.Printf("[connie-agent] %s: synced plot %s", tag, f.Name)
		synced++
	}

	return synced, nil
}
