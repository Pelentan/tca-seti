{{/*
Common labels applied to all SETI resources.
*/}}
{{- define "seti.labels" -}}
app.kubernetes.io/part-of: seti
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
Selector labels — used in Deployment.spec.selector.matchLabels and
Service.spec.selector. Must be stable across upgrades.
*/}}
{{- define "seti.selectorLabels" -}}
app: {{ .name }}
app.kubernetes.io/part-of: seti
{{- end }}
