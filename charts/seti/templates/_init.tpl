{{/*
Wait for cert-forge /ca to respond — confirms CA is generated and
the seti-certs Secret has been written before the pod starts.
*/}}
{{- define "seti.waitForCertForge" -}}
- name: wait-for-cert-forge
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for cert-forge /ca..."
      until wget -qO- http://cert-forge:{{ .Values.certForge.publicPort }}/ca > /dev/null 2>&1; do
        echo "cert-forge not ready, retrying in 3s..."
        sleep 3
      done
      echo "cert-forge ready."
{{- end }}

{{- define "seti.waitForPostgres" -}}
- name: wait-for-postgres
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for postgres..."
      until nc -z postgres 5432; do
        echo "postgres not ready, retrying in 3s..."
        sleep 3
      done
      echo "postgres ready."
{{- end }}

{{- define "seti.waitForRedis" -}}
- name: wait-for-redis
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for redis..."
      until nc -z redis 6379; do
        echo "redis not ready, retrying in 3s..."
        sleep 3
      done
      echo "redis ready."
{{- end }}

{{- define "seti.waitForAugurCanis" -}}
- name: wait-for-augur-canis
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for augur-canis..."
      until nc -z augur-canis 4010; do
        echo "augur-canis not ready, retrying in 3s..."
        sleep 3
      done
      echo "augur-canis ready."
{{- end }}

{{- define "seti.waitForSetiObservability" -}}
- name: wait-for-seti-observability
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for seti-observability..."
      until nc -z seti-observability 4011; do
        echo "seti-observability not ready, retrying in 3s..."
        sleep 3
      done
      echo "seti-observability ready."
{{- end }}

{{- define "seti.waitForSignalClearance" -}}
- name: wait-for-signal-clearance
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for signal-clearance..."
      until nc -z signal-clearance 4001; do
        echo "signal-clearance not ready, retrying in 3s..."
        sleep 3
      done
      echo "signal-clearance ready."
{{- end }}

{{- define "seti.waitForUI" -}}
- name: wait-for-ui
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for ui..."
      until nc -z ui 4020; do
        echo "ui not ready, retrying in 3s..."
        sleep 3
      done
      echo "ui ready."
{{- end }}

{{- define "seti.waitForPlotStore" -}}
- name: wait-for-plot-store
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for plot-store..."
      until nc -z plot-store 4005; do
        echo "plot-store not ready, retrying in 3s..."
        sleep 3
      done
      echo "plot-store ready."
{{- end }}

{{- define "seti.waitForSignalAggregator" -}}
- name: wait-for-signal-aggregator
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for signal-aggregator..."
      until nc -z signal-aggregator 4006; do
        echo "signal-aggregator not ready, retrying in 3s..."
        sleep 3
      done
      echo "signal-aggregator ready."
{{- end }}

{{- define "seti.waitForResults" -}}
- name: wait-for-results
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for results..."
      until nc -z results 4008; do
        echo "results not ready, retrying in 3s..."
        sleep 3
      done
      echo "results ready."
{{- end }}

{{- define "seti.waitForInteractions" -}}
- name: wait-for-interactions
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for interactions..."
      until nc -z interactions 4009; do
        echo "interactions not ready, retrying in 3s..."
        sleep 3
      done
      echo "interactions ready."
{{- end }}
