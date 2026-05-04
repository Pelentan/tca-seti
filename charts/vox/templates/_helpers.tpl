{{- define "vox.labels" -}}
app.kubernetes.io/part-of: vox
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}
