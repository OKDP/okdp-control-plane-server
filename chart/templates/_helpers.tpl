{{/*
Expand the name of the chart.
*/}}
{{- define "okdp-control-plane-server.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name, truncated at 63 chars
because of the DNS naming spec.
*/}}
{{- define "okdp-control-plane-server.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "okdp-control-plane-server.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{ include "okdp-control-plane-server.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "okdp-control-plane-server.selectorLabels" -}}
app.kubernetes.io/name: {{ include "okdp-control-plane-server.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
