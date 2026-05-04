{{- define "vox.waitForCertForge" -}}
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

{{- define "vox.waitForRedis" -}}
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

{{- define "vox.waitForObservability" -}}
- name: wait-for-observability
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for observability-service..."
      until nc -z observability-service {{ .Values.observabilityService.port }}; do
        echo "observability-service not ready, retrying in 3s..."
        sleep 3
      done
      echo "observability-service ready."
{{- end }}

{{- define "vox.waitForAuthMongo" -}}
- name: wait-for-auth-mongo
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for auth-mongo..."
      until nc -z auth-mongo 27017; do
        echo "auth-mongo not ready, retrying in 3s..."
        sleep 3
      done
      echo "auth-mongo ready."
{{- end }}

{{- define "vox.waitForUserMongo" -}}
- name: wait-for-user-mongo
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for user-mongo..."
      until nc -z user-mongo 27017; do
        echo "user-mongo not ready, retrying in 3s..."
        sleep 3
      done
      echo "user-mongo ready."
{{- end }}

{{- define "vox.waitForPollMongo" -}}
- name: wait-for-poll-mongo
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for poll-mongo..."
      until nc -z poll-mongo 27017; do
        echo "poll-mongo not ready, retrying in 3s..."
        sleep 3
      done
      echo "poll-mongo ready."
{{- end }}

{{- define "vox.waitForPollStylerMongo" -}}
- name: wait-for-poll-styler-mongo
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for poll-styler-mongo..."
      until nc -z poll-styler-mongo 27017; do
        echo "poll-styler-mongo not ready, retrying in 3s..."
        sleep 3
      done
      echo "poll-styler-mongo ready."
{{- end }}

{{- define "vox.waitForPollWr4nglerMongo" -}}
- name: wait-for-poll-wr4ngler-mongo
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for poll-wr4ngler-mongo..."
      until nc -z poll-wr4ngler-mongo 27017; do
        echo "poll-wr4ngler-mongo not ready, retrying in 3s..."
        sleep 3
      done
      echo "poll-wr4ngler-mongo ready."
{{- end }}

{{- define "vox.waitForReportTemplaterDb" -}}
- name: wait-for-report-templater-db
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for report-templater-db..."
      until nc -z report-templater-db 5432; do
        echo "report-templater-db not ready, retrying in 3s..."
        sleep 3
      done
      echo "report-templater-db ready."
{{- end }}

{{- define "vox.waitForPaymentWr4ngler" -}}
- name: wait-for-payment-wr4ngler
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for payment-wr4ngler..."
      until nc -z payment-wr4ngler {{ .Values.paymentWr4ngler.port }}; do
        echo "payment-wr4ngler not ready, retrying in 3s..."
        sleep 3
      done
      echo "payment-wr4ngler ready."
{{- end }}

{{- define "vox.waitForUI" -}}
- name: wait-for-ui
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for ui..."
      until nc -z ui {{ .Values.ui.port }}; do
        echo "ui not ready, retrying in 3s..."
        sleep 3
      done
      echo "ui ready."
{{- end }}

{{- define "vox.waitForAugurCanis" -}}
- name: wait-for-augur-canis
  image: busybox:1.36
  command:
    - sh
    - -c
    - |
      echo "Waiting for augur-canis..."
      until nc -z augur-canis {{ .Values.augurCanis.port }}; do
        echo "augur-canis not ready, retrying in 3s..."
        sleep 3
      done
      echo "augur-canis ready."
{{- end }}
