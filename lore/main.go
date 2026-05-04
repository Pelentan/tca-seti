package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Lore — SETI's institutional memory.
//
// Three-tier storage model:
//   Tier 1 (Redis, run-scoped):    Transient capture. Not Lore's concern.
//   Tier 2 (Redis, sliding window): Bad-whiff buffer. Not Lore's concern.
//   Tier 3 (PostgreSQL, persistent): Everything Lore owns.
//
// Lore does not reason. It remembers. AI-lien reasons against what Lore holds.
//
// Write paths:
//   - ai-lien:  trend points, baselines, incidents, patterns (primary)
//   - results:  trend points and incidents on the direct fast path
//   - wr4ngler: stubbed — returns 501
//
// Read paths:
//   - ai-lien:  baselines, incidents, trend points, patterns
//   - plot-test: baselines
//   - integration: trend points, patterns (read-only)
// ---------------------------------------------------------------------------

var (
	port             = envOr("PORT", "4110")
	databaseURL      = buildDatabaseURL()
	observabilityURL = envOr("OBSERVABILITY_URL", "https://seti-observability:4011")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("[lore] Required env var %s not set", key)
	}
	return v
}

// buildDatabaseURL constructs the postgres connection URL using X.509
// client certificate authentication. No password — lore authenticates
// via its cert-forge instance cert (CN=lore-db maps to the lore pg role).
// Falls back to DATABASE_URL env var for Docker Compose compatibility.
func buildDatabaseURL() string {
	// Direct URL takes precedence (Docker Compose dev mode)
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}

	// K8s: cert-based auth — no password in the URL
	cert := envOr("DATABASE_CERT", "/certs/lore-db.crt")
	key := envOr("DATABASE_KEY", "/certs/lore-db.key")
	ca := envOr("DATABASE_CA", "/certs/ca.crt")

	// Verify the cert files exist before building the URL
	for _, f := range []string{cert, key, ca} {
		if _, err := os.Stat(f); err != nil {
			log.Fatalf("[lore] Database cert file not found %s: %v", f, err)
		}
	}

	return fmt.Sprintf(
		"postgres://lore@postgres:5432/lore?sslmode=verify-full&sslcert=%s&sslkey=%s&sslrootcert=%s",
		cert, key, ca,
	)
}

// ---------------------------------------------------------------------------
// Database
// ---------------------------------------------------------------------------

var db *sql.DB

func connectDB() {
	var err error
	db, err = sql.Open("postgres", databaseURL)
	if err != nil {
		log.Fatalf("[lore] Failed to open database: %v", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("[lore] Database ping failed: %v", err)
	}
	log.Printf("[lore] Database connected")
}

func initSchema() {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS trend_points (
			trend_point_id  TEXT PRIMARY KEY,
			application_id  TEXT NOT NULL,
			job_name        TEXT NOT NULL,
			source_job      TEXT NOT NULL,
			signal_type     TEXT NOT NULL,
			description     TEXT NOT NULL,
			evidence        JSONB NOT NULL DEFAULT '{}',
			bad_whiff_window_start TIMESTAMPTZ,
			related_incident_id    TEXT,
			run_id                 TEXT,
			recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_trend_points_app    ON trend_points(application_id);
		CREATE INDEX IF NOT EXISTS idx_trend_points_job    ON trend_points(application_id, job_name);
		CREATE INDEX IF NOT EXISTS idx_trend_points_source ON trend_points(source_job);
		CREATE INDEX IF NOT EXISTS idx_trend_points_time   ON trend_points(recorded_at DESC);

		CREATE TABLE IF NOT EXISTS baselines (
			application_id          TEXT NOT NULL,
			job_name                TEXT NOT NULL,
			established_by          TEXT NOT NULL DEFAULT 'ai-lien',
			latency_p50_ms          INTEGER NOT NULL,
			latency_p95_ms          INTEGER NOT NULL,
			latency_p99_ms          INTEGER NOT NULL,
			latency_alert_multiplier NUMERIC(4,2) NOT NULL DEFAULT 2.0,
			contract_test_pass_rate NUMERIC(5,4) NOT NULL,
			plot_test_pass_rate     NUMERIC(5,4),
			sample_size             INTEGER NOT NULL,
			observation_window_hours INTEGER NOT NULL,
			notes                   TEXT,
			established_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (application_id, job_name)
		);

		CREATE TABLE IF NOT EXISTS incidents (
			incident_id             TEXT PRIMARY KEY,
			application_id          TEXT NOT NULL,
			job_name                TEXT NOT NULL,
			source_job              TEXT NOT NULL,
			trigger_type            TEXT NOT NULL,
			description             TEXT NOT NULL,
			ai_assessment           TEXT NOT NULL,
			ai_recommended_action   TEXT NOT NULL,
			run_id                  TEXT NOT NULL,
			related_trend_point_ids JSONB NOT NULL DEFAULT '[]',
			status                  TEXT NOT NULL DEFAULT 'open',
			recorded_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			resolved_at             TIMESTAMPTZ,
			resolution_summary      TEXT,
			ai_assessment_correct   BOOLEAN
		);
		CREATE INDEX IF NOT EXISTS idx_incidents_app    ON incidents(application_id);
		CREATE INDEX IF NOT EXISTS idx_incidents_status ON incidents(status);
		CREATE INDEX IF NOT EXISTS idx_incidents_time   ON incidents(recorded_at DESC);

		CREATE TABLE IF NOT EXISTS patterns (
			pattern_id          TEXT PRIMARY KEY,
			application_id      TEXT NOT NULL,
			job_name            TEXT,
			name                TEXT NOT NULL,
			description         TEXT NOT NULL,
			sequence            JSONB NOT NULL DEFAULT '[]',
			observed_count      INTEGER NOT NULL DEFAULT 1,
			associated_outcome  TEXT NOT NULL,
			source_incident_ids JSONB NOT NULL DEFAULT '[]',
			identified_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_observed_at    TIMESTAMPTZ
		);
		CREATE INDEX IF NOT EXISTS idx_patterns_app ON patterns(application_id);

		CREATE TABLE IF NOT EXISTS corrections (
			correction_id   TEXT PRIMARY KEY,
			incident_id     TEXT NOT NULL,
			wrangler_id     TEXT NOT NULL,
			correction      TEXT NOT NULL,
			ai_was_correct  BOOLEAN,
			recorded_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`)
	if err != nil {
		log.Fatalf("[lore] Schema init failed: %v", err)
	}
	log.Printf("[lore] Schema ready")
}

// ---------------------------------------------------------------------------
// ID generation
// ---------------------------------------------------------------------------

var startTime = time.Now()

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// ---------------------------------------------------------------------------
// Observability
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
			"caller":      "lore",
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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

// ---------------------------------------------------------------------------
// Handlers — Trend Points
// ---------------------------------------------------------------------------

func handleTrendPoints(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	switch r.Method {
	case http.MethodPost:
		var req struct {
			ApplicationID        string                 `json:"application_id"`
			JobName              string                 `json:"job_name"`
			SourceJob            string                 `json:"source_job"`
			SignalType           string                 `json:"signal_type"`
			Description          string                 `json:"description"`
			Evidence             map[string]interface{} `json:"evidence"`
			BadWhiffWindowStart  *string                `json:"bad_whiff_window_start,omitempty"`
			RelatedIncidentID    *string                `json:"related_incident_id,omitempty"`
			RunID                *string                `json:"run_id,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if req.ApplicationID == "" || req.JobName == "" || req.SourceJob == "" ||
			req.SignalType == "" || req.Description == "" {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST",
				"application_id, job_name, source_job, signal_type, and description required")
			return
		}

		validSources := map[string]bool{"ai-lien": true, "results": true, "wr4ngler": true}
		if !validSources[req.SourceJob] {
			errJSON(w, http.StatusBadRequest, "INVALID_SOURCE",
				"source_job must be ai-lien, results, or wr4ngler")
			return
		}

		evidence, _ := json.Marshal(req.Evidence)
		id := newID("lore-tp")
		now := time.Now().UTC()

		_, err := db.Exec(`
			INSERT INTO trend_points
				(trend_point_id, application_id, job_name, source_job, signal_type,
				 description, evidence, bad_whiff_window_start, related_incident_id,
				 run_id, recorded_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			id, req.ApplicationID, req.JobName, req.SourceJob, req.SignalType,
			req.Description, string(evidence), req.BadWhiffWindowStart,
			req.RelatedIncidentID, req.RunID, now,
		)
		if err != nil {
			log.Printf("[lore] trend_points insert failed: %v", err)
			errJSON(w, http.StatusInternalServerError, "DB_ERROR", "failed to store trend point")
			return
		}

		result := map[string]interface{}{
			"trend_point_id": id, "application_id": req.ApplicationID,
			"job_name": req.JobName, "source_job": req.SourceJob,
			"signal_type": req.SignalType, "description": req.Description,
			"evidence": req.Evidence, "recorded_at": now.Format(time.RFC3339),
		}
		if req.BadWhiffWindowStart != nil {
			result["bad_whiff_window_start"] = *req.BadWhiffWindowStart
		}
		if req.RelatedIncidentID != nil {
			result["related_incident_id"] = *req.RelatedIncidentID
		}
		if req.RunID != nil {
			result["run_id"] = *req.RunID
		}

		log.Printf("[lore] Trend point stored: %s (%s/%s %s)", id, req.ApplicationID, req.JobName, req.SignalType)
		writeJSON(w, http.StatusCreated, result)
		reportEvent("db", "POST", "/trend-points", 201, time.Since(start).Milliseconds())

	case http.MethodGet:
		q := r.URL.Query()
		appID := q.Get("application_id")
		jobName := q.Get("job_name")
		sourceJob := q.Get("source_job")
		since := q.Get("since")
		limit := 100

		if l := q.Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
			if limit < 1 {
				limit = 1
			}
			if limit > 500 {
				limit = 500
			}
		}

		query := `SELECT trend_point_id, application_id, job_name, source_job,
			signal_type, description, evidence, bad_whiff_window_start,
			related_incident_id, run_id, recorded_at
			FROM trend_points WHERE 1=1`
		args := []interface{}{}
		n := 1

		if appID != "" {
			query += fmt.Sprintf(" AND application_id=$%d", n)
			args = append(args, appID)
			n++
		}
		if jobName != "" {
			query += fmt.Sprintf(" AND job_name=$%d", n)
			args = append(args, jobName)
			n++
		}
		if sourceJob != "" {
			query += fmt.Sprintf(" AND source_job=$%d", n)
			args = append(args, sourceJob)
			n++
		}
		if since != "" {
			query += fmt.Sprintf(" AND recorded_at>$%d", n)
			args = append(args, since)
			n++
		}
		query += fmt.Sprintf(" ORDER BY recorded_at DESC LIMIT $%d", n)
		args = append(args, limit)

		rows, err := db.Query(query, args...)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "DB_ERROR", "query failed")
			return
		}
		defer rows.Close()

		results := []map[string]interface{}{}
		for rows.Next() {
			var (
				tpID, appIDv, jn, src, sig, desc string
				evidenceRaw                       []byte
				whiffStart, relInc, runIDv        sql.NullString
				recordedAt                        time.Time
			)
			if err := rows.Scan(&tpID, &appIDv, &jn, &src, &sig, &desc,
				&evidenceRaw, &whiffStart, &relInc, &runIDv, &recordedAt); err != nil {
				continue
			}
			var evidence interface{}
			json.Unmarshal(evidenceRaw, &evidence)
			row := map[string]interface{}{
				"trend_point_id": tpID, "application_id": appIDv,
				"job_name": jn, "source_job": src, "signal_type": sig,
				"description": desc, "evidence": evidence,
				"recorded_at": recordedAt.UTC().Format(time.RFC3339),
			}
			if whiffStart.Valid {
				row["bad_whiff_window_start"] = whiffStart.String
			}
			if relInc.Valid {
				row["related_incident_id"] = relInc.String
			}
			if runIDv.Valid {
				row["run_id"] = runIDv.String
			}
			results = append(results, row)
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"trend_points": results, "total": len(results),
		})
		reportEvent("db", "GET", "/trend-points", 200, time.Since(start).Milliseconds())

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleTrendPoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/trend-points/")
	id = strings.TrimSuffix(id, "/")

	var (
		tpID, appID, jn, src, sig, desc string
		evidenceRaw                      []byte
		whiffStart, relInc, runIDv       sql.NullString
		recordedAt                       time.Time
	)
	err := db.QueryRow(`
		SELECT trend_point_id, application_id, job_name, source_job,
		       signal_type, description, evidence, bad_whiff_window_start,
		       related_incident_id, run_id, recorded_at
		FROM trend_points WHERE trend_point_id=$1`, id).
		Scan(&tpID, &appID, &jn, &src, &sig, &desc,
			&evidenceRaw, &whiffStart, &relInc, &runIDv, &recordedAt)
	if err != nil {
		errJSON(w, http.StatusNotFound, "NOT_FOUND",
			fmt.Sprintf("trend point %s not found", id))
		return
	}
	var evidence interface{}
	json.Unmarshal(evidenceRaw, &evidence)
	row := map[string]interface{}{
		"trend_point_id": tpID, "application_id": appID,
		"job_name": jn, "source_job": src, "signal_type": sig,
		"description": desc, "evidence": evidence,
		"recorded_at": recordedAt.UTC().Format(time.RFC3339),
	}
	if whiffStart.Valid {
		row["bad_whiff_window_start"] = whiffStart.String
	}
	if relInc.Valid {
		row["related_incident_id"] = relInc.String
	}
	if runIDv.Valid {
		row["run_id"] = runIDv.String
	}
	writeJSON(w, http.StatusOK, row)
}

// ---------------------------------------------------------------------------
// Handlers — Baselines
// ---------------------------------------------------------------------------

func handleBaseline(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// Path: /baselines/{application_id}/{job_name}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/baselines/"), "/")
	if len(parts) < 2 {
		errJSON(w, http.StatusBadRequest, "INVALID_PATH", "path must be /baselines/{application_id}/{job_name}")
		return
	}
	appID, jobName := parts[0], parts[1]

	switch r.Method {
	case http.MethodGet:
		var (
			estBy                             string
			p50, p95, p99, sampleSize, winH  int
			alertMult, ctPass                 float64
			ptPass                            sql.NullFloat64
			notes                             sql.NullString
			estAt, updAt                      time.Time
		)
		err := db.QueryRow(`
			SELECT established_by, latency_p50_ms, latency_p95_ms, latency_p99_ms,
			       latency_alert_multiplier, contract_test_pass_rate, plot_test_pass_rate,
			       sample_size, observation_window_hours, notes, established_at, updated_at
			FROM baselines WHERE application_id=$1 AND job_name=$2`,
			appID, jobName).
			Scan(&estBy, &p50, &p95, &p99, &alertMult, &ctPass, &ptPass,
				&sampleSize, &winH, &notes, &estAt, &updAt)
		if err != nil {
			errJSON(w, http.StatusNotFound, "BASELINE_NOT_FOUND",
				fmt.Sprintf("No baseline established for job %q in application %q", jobName, appID))
			return
		}
		result := map[string]interface{}{
			"application_id": appID, "job_name": jobName,
			"established_by": estBy, "latency_p50_ms": p50,
			"latency_p95_ms": p95, "latency_p99_ms": p99,
			"latency_alert_multiplier": alertMult,
			"contract_test_pass_rate":  ctPass,
			"sample_size": sampleSize, "observation_window_hours": winH,
			"established_at": estAt.UTC().Format(time.RFC3339),
			"updated_at":     updAt.UTC().Format(time.RFC3339),
		}
		if ptPass.Valid {
			result["plot_test_pass_rate"] = ptPass.Float64
		}
		if notes.Valid {
			result["notes"] = notes.String
		}
		writeJSON(w, http.StatusOK, result)
		reportEvent("db", "GET", "/baselines", 200, time.Since(start).Milliseconds())

	case http.MethodPut:
		var req struct {
			EstablishedBy           string   `json:"established_by"`
			LatencyP50Ms            int      `json:"latency_p50_ms"`
			LatencyP95Ms            int      `json:"latency_p95_ms"`
			LatencyP99Ms            int      `json:"latency_p99_ms"`
			LatencyAlertMultiplier  *float64 `json:"latency_alert_multiplier,omitempty"`
			ContractTestPassRate    float64  `json:"contract_test_pass_rate"`
			PlotTestPassRate        *float64 `json:"plot_test_pass_rate,omitempty"`
			SampleSize              int      `json:"sample_size"`
			ObservationWindowHours  int      `json:"observation_window_hours"`
			Notes                   *string  `json:"notes,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if req.LatencyP50Ms == 0 || req.SampleSize == 0 {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST",
				"latency_p50_ms, latency_p95_ms, latency_p99_ms, contract_test_pass_rate, sample_size, observation_window_hours required")
			return
		}

		mult := 2.0
		if req.LatencyAlertMultiplier != nil {
			mult = *req.LatencyAlertMultiplier
		}
		now := time.Now().UTC()

		_, err := db.Exec(`
			INSERT INTO baselines
				(application_id, job_name, established_by, latency_p50_ms,
				 latency_p95_ms, latency_p99_ms, latency_alert_multiplier,
				 contract_test_pass_rate, plot_test_pass_rate, sample_size,
				 observation_window_hours, notes, established_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (application_id, job_name) DO UPDATE SET
				established_by=$3, latency_p50_ms=$4, latency_p95_ms=$5,
				latency_p99_ms=$6, latency_alert_multiplier=$7,
				contract_test_pass_rate=$8, plot_test_pass_rate=$9,
				sample_size=$10, observation_window_hours=$11,
				notes=$12, updated_at=$14`,
			appID, jobName, "ai-lien", req.LatencyP50Ms, req.LatencyP95Ms,
			req.LatencyP99Ms, mult, req.ContractTestPassRate, req.PlotTestPassRate,
			req.SampleSize, req.ObservationWindowHours, req.Notes, now, now,
		)
		if err != nil {
			log.Printf("[lore] baseline upsert failed: %v", err)
			errJSON(w, http.StatusInternalServerError, "DB_ERROR", "failed to upsert baseline")
			return
		}

		result := map[string]interface{}{
			"application_id": appID, "job_name": jobName,
			"established_by": "ai-lien",
			"latency_p50_ms": req.LatencyP50Ms, "latency_p95_ms": req.LatencyP95Ms,
			"latency_p99_ms": req.LatencyP99Ms, "latency_alert_multiplier": mult,
			"contract_test_pass_rate": req.ContractTestPassRate,
			"sample_size": req.SampleSize, "observation_window_hours": req.ObservationWindowHours,
			"established_at": now.Format(time.RFC3339), "updated_at": now.Format(time.RFC3339),
		}
		if req.PlotTestPassRate != nil {
			result["plot_test_pass_rate"] = *req.PlotTestPassRate
		}
		if req.Notes != nil {
			result["notes"] = *req.Notes
		}

		log.Printf("[lore] Baseline upserted: %s/%s", appID, jobName)
		writeJSON(w, http.StatusOK, result)
		reportEvent("db", "PUT", "/baselines", 200, time.Since(start).Milliseconds())

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Handlers — Incidents
// ---------------------------------------------------------------------------

func handleIncidents(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	switch r.Method {
	case http.MethodPost:
		var req struct {
			ApplicationID        string   `json:"application_id"`
			JobName              string   `json:"job_name"`
			SourceJob            string   `json:"source_job"`
			TriggerType          string   `json:"trigger_type"`
			Description          string   `json:"description"`
			AIAssessment         string   `json:"ai_assessment"`
			AIRecommendedAction  string   `json:"ai_recommended_action"`
			RunID                string   `json:"run_id"`
			RelatedTrendPointIDs []string `json:"related_trend_point_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if req.ApplicationID == "" || req.JobName == "" || req.SourceJob == "" ||
			req.TriggerType == "" || req.Description == "" ||
			req.AIAssessment == "" || req.RunID == "" {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST", "required fields missing")
			return
		}

		if req.RelatedTrendPointIDs == nil {
			req.RelatedTrendPointIDs = []string{}
		}
		relatedJSON, _ := json.Marshal(req.RelatedTrendPointIDs)

		id := newID("lore-inc")
		now := time.Now().UTC()

		_, err := db.Exec(`
			INSERT INTO incidents
				(incident_id, application_id, job_name, source_job, trigger_type,
				 description, ai_assessment, ai_recommended_action, run_id,
				 related_trend_point_ids, status, recorded_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'open',$11)`,
			id, req.ApplicationID, req.JobName, req.SourceJob, req.TriggerType,
			req.Description, req.AIAssessment, req.AIRecommendedAction, req.RunID,
			string(relatedJSON), now,
		)
		if err != nil {
			log.Printf("[lore] incident insert failed: %v", err)
			errJSON(w, http.StatusInternalServerError, "DB_ERROR", "failed to store incident")
			return
		}

		log.Printf("[lore] Incident recorded: %s (%s/%s %s)", id, req.ApplicationID, req.JobName, req.TriggerType)
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"incident_id": id, "application_id": req.ApplicationID,
			"job_name": req.JobName, "source_job": req.SourceJob,
			"trigger_type": req.TriggerType, "description": req.Description,
			"ai_assessment": req.AIAssessment, "ai_recommended_action": req.AIRecommendedAction,
			"run_id": req.RunID, "related_trend_point_ids": req.RelatedTrendPointIDs,
			"status": "open", "recorded_at": now.Format(time.RFC3339),
		})
		reportEvent("db", "POST", "/incidents", 201, time.Since(start).Milliseconds())

	case http.MethodGet:
		q := r.URL.Query()
		appID := q.Get("application_id")
		jobName := q.Get("job_name")
		status := q.Get("status")
		since := q.Get("since")
		limit := 50

		if l := q.Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
			if limit < 1 {
				limit = 1
			}
			if limit > 200 {
				limit = 200
			}
		}

		query := `SELECT incident_id, application_id, job_name, source_job,
			trigger_type, description, ai_assessment, ai_recommended_action,
			run_id, related_trend_point_ids, status, recorded_at,
			resolved_at, resolution_summary, ai_assessment_correct
			FROM incidents WHERE 1=1`
		args := []interface{}{}
		n := 1

		if appID != "" {
			query += fmt.Sprintf(" AND application_id=$%d", n)
			args = append(args, appID)
			n++
		}
		if jobName != "" {
			query += fmt.Sprintf(" AND job_name=$%d", n)
			args = append(args, jobName)
			n++
		}
		if status != "" {
			query += fmt.Sprintf(" AND status=$%d", n)
			args = append(args, status)
			n++
		}
		if since != "" {
			query += fmt.Sprintf(" AND recorded_at>$%d", n)
			args = append(args, since)
			n++
		}
		query += fmt.Sprintf(" ORDER BY recorded_at DESC LIMIT $%d", n)
		args = append(args, limit)

		rows, err := db.Query(query, args...)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "DB_ERROR", "query failed")
			return
		}
		defer rows.Close()

		results := []map[string]interface{}{}
		openCount, resolvedCount := 0, 0
		for rows.Next() {
			var (
				incID, appIDv, jn, src, trig, desc, assess, action, runID string
				relatedRaw                                                  []byte
				stat                                                        string
				recAt                                                       time.Time
				resolvedAt                                                  sql.NullTime
				resSummary                                                  sql.NullString
				correct                                                     sql.NullBool
			)
			if err := rows.Scan(&incID, &appIDv, &jn, &src, &trig, &desc,
				&assess, &action, &runID, &relatedRaw, &stat, &recAt,
				&resolvedAt, &resSummary, &correct); err != nil {
				continue
			}
			var related []string
			json.Unmarshal(relatedRaw, &related)
			row := map[string]interface{}{
				"incident_id": incID, "application_id": appIDv,
				"job_name": jn, "source_job": src, "trigger_type": trig,
				"description": desc, "ai_assessment": assess,
				"ai_recommended_action": action, "run_id": runID,
				"related_trend_point_ids": related,
				"status": stat, "recorded_at": recAt.UTC().Format(time.RFC3339),
			}
			if resolvedAt.Valid {
				row["resolved_at"] = resolvedAt.Time.UTC().Format(time.RFC3339)
			}
			if resSummary.Valid {
				row["resolution_summary"] = resSummary.String
			}
			if correct.Valid {
				row["ai_assessment_correct"] = correct.Bool
			}
			results = append(results, row)
			if stat == "open" {
				openCount++
			} else {
				resolvedCount++
			}
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"incidents":      results,
			"total":          len(results),
			"open_count":     openCount,
			"resolved_count": resolvedCount,
		})
		reportEvent("db", "GET", "/incidents", 200, time.Since(start).Milliseconds())

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func handleIncident(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/incidents/"), "/")
	incID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	if r.Method == http.MethodGet && action == "" {
		var (
			appID, jn, src, trig, desc, assess, action2, runID string
			relatedRaw                                           []byte
			stat                                                 string
			recAt                                                time.Time
			resolvedAt                                           sql.NullTime
			resSummary                                           sql.NullString
			correct                                              sql.NullBool
		)
		err := db.QueryRow(`
			SELECT application_id, job_name, source_job, trigger_type, description,
			       ai_assessment, ai_recommended_action, run_id, related_trend_point_ids,
			       status, recorded_at, resolved_at, resolution_summary, ai_assessment_correct
			FROM incidents WHERE incident_id=$1`, incID).
			Scan(&appID, &jn, &src, &trig, &desc, &assess, &action2, &runID,
				&relatedRaw, &stat, &recAt, &resolvedAt, &resSummary, &correct)
		if err != nil {
			errJSON(w, http.StatusNotFound, "NOT_FOUND",
				fmt.Sprintf("incident %s not found", incID))
			return
		}
		var related []string
		json.Unmarshal(relatedRaw, &related)
		row := map[string]interface{}{
			"incident_id": incID, "application_id": appID,
			"job_name": jn, "source_job": src, "trigger_type": trig,
			"description": desc, "ai_assessment": assess,
			"ai_recommended_action": action2, "run_id": runID,
			"related_trend_point_ids": related,
			"status": stat, "recorded_at": recAt.UTC().Format(time.RFC3339),
		}
		if resolvedAt.Valid {
			row["resolved_at"] = resolvedAt.Time.UTC().Format(time.RFC3339)
		}
		if resSummary.Valid {
			row["resolution_summary"] = resSummary.String
		}
		if correct.Valid {
			row["ai_assessment_correct"] = correct.Bool
		}
		writeJSON(w, http.StatusOK, row)
		return
	}

	if r.Method == http.MethodPost && action == "resolve" {
		var req struct {
			ResolvedBy          string  `json:"resolved_by"`
			ResolutionSummary   string  `json:"resolution_summary"`
			AIAssessmentCorrect *bool   `json:"ai_assessment_correct,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if req.ResolvedBy == "" || req.ResolutionSummary == "" {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST",
				"resolved_by and resolution_summary required")
			return
		}

		now := time.Now().UTC()
		result, err := db.Exec(`
			UPDATE incidents SET status='resolved', resolved_at=$1,
			resolution_summary=$2, ai_assessment_correct=$3
			WHERE incident_id=$4 AND status='open'`,
			now, req.ResolutionSummary, req.AIAssessmentCorrect, incID)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "DB_ERROR", "update failed")
			return
		}
		rows, _ := result.RowsAffected()
		if rows == 0 {
			errJSON(w, http.StatusConflict, "ALREADY_RESOLVED",
				fmt.Sprintf("incident %s is not open", incID))
			return
		}
		log.Printf("[lore] Incident resolved: %s by %s", incID, req.ResolvedBy)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"incident_id": incID, "status": "resolved",
			"resolved_at":        now.Format(time.RFC3339),
			"resolution_summary": req.ResolutionSummary,
		})
		return
	}

	w.WriteHeader(http.StatusMethodNotAllowed)
}

// ---------------------------------------------------------------------------
// Handlers — Patterns
// ---------------------------------------------------------------------------

func handlePatterns(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	switch r.Method {
	case http.MethodPost:
		var req struct {
			ApplicationID     string                   `json:"application_id"`
			JobName           *string                  `json:"job_name,omitempty"`
			Name              string                   `json:"name"`
			Description       string                   `json:"description"`
			Sequence          []map[string]interface{} `json:"sequence"`
			ObservedCount     int                      `json:"observed_count"`
			AssociatedOutcome string                   `json:"associated_outcome"`
			SourceIncidentIDs []string                 `json:"source_incident_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
			return
		}
		if req.ApplicationID == "" || req.Name == "" || req.Description == "" ||
			len(req.Sequence) == 0 || req.AssociatedOutcome == "" {
			errJSON(w, http.StatusBadRequest, "INVALID_REQUEST", "required fields missing")
			return
		}

		if req.SourceIncidentIDs == nil {
			req.SourceIncidentIDs = []string{}
		}
		seqJSON, _ := json.Marshal(req.Sequence)
		srcJSON, _ := json.Marshal(req.SourceIncidentIDs)

		id := newID("lore-pat")
		now := time.Now().UTC()

		_, err := db.Exec(`
			INSERT INTO patterns
				(pattern_id, application_id, job_name, name, description,
				 sequence, observed_count, associated_outcome, source_incident_ids,
				 identified_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)`,
			id, req.ApplicationID, req.JobName, req.Name, req.Description,
			string(seqJSON), req.ObservedCount, req.AssociatedOutcome,
			string(srcJSON), now,
		)
		if err != nil {
			log.Printf("[lore] pattern insert failed: %v", err)
			errJSON(w, http.StatusInternalServerError, "DB_ERROR", "failed to store pattern")
			return
		}

		log.Printf("[lore] Pattern recorded: %s (%s)", id, req.Name)
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"pattern_id": id, "application_id": req.ApplicationID,
			"job_name": req.JobName, "name": req.Name,
			"description": req.Description, "sequence": req.Sequence,
			"observed_count": req.ObservedCount,
			"associated_outcome": req.AssociatedOutcome,
			"source_incident_ids": req.SourceIncidentIDs,
			"identified_at": now.Format(time.RFC3339),
			"updated_at":    now.Format(time.RFC3339),
		})
		reportEvent("db", "POST", "/patterns", 201, time.Since(start).Milliseconds())

	case http.MethodGet:
		q := r.URL.Query()
		appID := q.Get("application_id")
		jobName := q.Get("job_name")
		limit := 50
		if l := q.Get("limit"); l != "" {
			fmt.Sscanf(l, "%d", &limit)
			if limit < 1 {
				limit = 1
			}
			if limit > 200 {
				limit = 200
			}
		}

		query := `SELECT pattern_id, application_id, job_name, name, description,
			sequence, observed_count, associated_outcome, source_incident_ids,
			identified_at, updated_at, last_observed_at
			FROM patterns WHERE 1=1`
		args := []interface{}{}
		n := 1
		if appID != "" {
			query += fmt.Sprintf(" AND application_id=$%d", n)
			args = append(args, appID)
			n++
		}
		if jobName != "" {
			query += fmt.Sprintf(" AND job_name=$%d", n)
			args = append(args, jobName)
			n++
		}
		query += fmt.Sprintf(" ORDER BY updated_at DESC LIMIT $%d", n)
		args = append(args, limit)

		rows, err := db.Query(query, args...)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, "DB_ERROR", "query failed")
			return
		}
		defer rows.Close()

		results := []map[string]interface{}{}
		for rows.Next() {
			var (
				patID, appIDv, name, desc, assoc string
				jn                               sql.NullString
				seqRaw, srcRaw                   []byte
				count                            int
				identAt, updAt                   time.Time
				lastObs                          sql.NullTime
			)
			if err := rows.Scan(&patID, &appIDv, &jn, &name, &desc,
				&seqRaw, &count, &assoc, &srcRaw,
				&identAt, &updAt, &lastObs); err != nil {
				continue
			}
			var seq, src interface{}
			json.Unmarshal(seqRaw, &seq)
			json.Unmarshal(srcRaw, &src)
			row := map[string]interface{}{
				"pattern_id": patID, "application_id": appIDv,
				"name": name, "description": desc,
				"sequence": seq, "observed_count": count,
				"associated_outcome": assoc, "source_incident_ids": src,
				"identified_at": identAt.UTC().Format(time.RFC3339),
				"updated_at":    updAt.UTC().Format(time.RFC3339),
			}
			if jn.Valid {
				row["job_name"] = jn.String
			}
			if lastObs.Valid {
				row["last_observed_at"] = lastObs.Time.UTC().Format(time.RFC3339)
			}
			results = append(results, row)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"patterns": results, "total": len(results),
		})
		reportEvent("db", "GET", "/patterns", 200, time.Since(start).Milliseconds())

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Handlers — Corrections (stub)
// ---------------------------------------------------------------------------

func handleCorrections(w http.ResponseWriter, r *http.Request) {
	errJSON(w, http.StatusNotImplemented, "NOT_IMPLEMENTED",
		"Wr4ngler corrections are not yet implemented. Stub endpoint — wire the UI when ready.")
}

// ---------------------------------------------------------------------------
// Handlers — Health
// ---------------------------------------------------------------------------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	var tpCount, baselineCount, incOpen, incTotal, patCount int
	db.QueryRow(`SELECT COUNT(*) FROM trend_points`).Scan(&tpCount)
	db.QueryRow(`SELECT COUNT(*) FROM baselines`).Scan(&baselineCount)
	db.QueryRow(`SELECT COUNT(*) FROM incidents WHERE status='open'`).Scan(&incOpen)
	db.QueryRow(`SELECT COUNT(*) FROM incidents`).Scan(&incTotal)
	db.QueryRow(`SELECT COUNT(*) FROM patterns`).Scan(&patCount)

	status := "healthy"
	if err := db.Ping(); err != nil {
		status = "degraded"
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":                status,
		"trend_points_stored":   tpCount,
		"baselines_established": baselineCount,
		"incidents_open":        incOpen,
		"incidents_total":       incTotal,
		"patterns_learned":      patCount,
		"uptime_seconds":        int(time.Since(startTime).Seconds()),
	})
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

var certMat *CertMaterial

func main() {
	certMat = obtainCerts("lore")
	buildUpstreamClient()
	connectDB()
	initSchema()
	go selfRegisterWithAC(certMat, "https://lore:4110")

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/trend-points", handleTrendPoints)
	mux.HandleFunc("/trend-points/", handleTrendPoint)
	mux.HandleFunc("/baselines/", handleBaseline)
	mux.HandleFunc("/incidents", handleIncidents)
	mux.HandleFunc("/incidents/", handleIncident)
	mux.HandleFunc("/patterns", handlePatterns)
	mux.HandleFunc("/corrections", handleCorrections)

	server := &http.Server{
		Addr:      ":" + port,
		Handler:   mux,
		TLSConfig: buildServerTLS(certMat),
	}

	log.Printf("[lore] Listening on :%s (mTLS, TLS 1.3)", port)
	log.Printf("[lore] Storage: PostgreSQL (persistent)")
	log.Printf("[lore] Institutional memory: trend points, baselines, incidents, patterns")

	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[lore] Server error: %v", err)
		}
	}()
	awaitShutdown(server)
}
