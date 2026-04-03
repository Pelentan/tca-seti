Date: 2026-04-02 00:00 UTC
Feature: Phase 4 — Integration, AI-lien, Policy AI provider management, Admin UI
Files Added: 8
  - integration/main.go
  - integration/go.mod
  - integration/go.sum
  - integration/Dockerfile
  - ai-lien/main.py
  - ai-lien/requirements.txt
  - ai-lien/Dockerfile
  - ui/src/pages/Admin.tsx
Files Modified: 9
  - policy/main.go (AI provider CRUD, Ollama model discovery, active model selection)
  - interactions/main.go (real AI-lien caller replacing stub)
  - gateway/main.go (Phase 4 routes: /v1/*, /ai-providers/*)
  - cert-init/generate-certs.sh (integration, ai-lien certs)
  - seti-observability/main.go (integration, ai-lien in allowlist)
  - augur-canis/main.go (integration, ai-lien in bootstrap registry)
  - docker-compose.yml (integration, ai-lien services; AI_LIEN_URL for interactions)
  - ui/src/App.tsx (Admin route)
  - ui/src/pages/Dashboard.tsx (Admin link in header for sec-wrangler)
