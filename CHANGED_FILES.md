Date: 2026-04-14 UTC
Feature: Phase 1 — cert-forge K8s Secret integration + minimal Helm chart (cert-forge, Redis, seti-observability)
Files Modified: 2
  - cert-forge/main.go
  - cert-forge/enrollment.go
Files Added: 16
  - cert-forge/k8s.go
  - charts/seti/Chart.yaml
  - charts/seti/values.yaml
  - charts/seti/values/dev.yaml
  - charts/seti/templates/_helpers.tpl
  - charts/seti/templates/namespace.yaml
  - charts/seti/templates/cert-forge/serviceaccount.yaml
  - charts/seti/templates/cert-forge/role.yaml
  - charts/seti/templates/cert-forge/rolebinding.yaml
  - charts/seti/templates/cert-forge/deployment.yaml
  - charts/seti/templates/cert-forge/service.yaml
  - charts/seti/templates/redis/deployment.yaml
  - charts/seti/templates/seti-observability/deployment.yaml
  - charts/seti/templates/seti-observability/service.yaml
  - scripts/k3d-setup.sh
  - scripts/build-push.sh
