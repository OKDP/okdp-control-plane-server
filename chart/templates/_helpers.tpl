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

{{/*
Centralized validation of engine enums and git mode requirements.
*/}}
{{- define "okdp-control-plane-server.validate" -}}
{{- $engine := .Values.engine | default dict -}}
{{- $engineType := dig "type" "" $engine | toString | trim | default "kubocd" -}}
{{- if ne $engineType "kubocd" -}}
{{- fail (printf "unsupported engine.type %q: supported values are kubocd" $engineType) -}}
{{- end -}}
{{- $backend := dig "kubocd" "backend" "" $engine | toString | trim | default "flux" -}}
{{- if ne $backend "flux" -}}
{{- fail (printf "unsupported engine.kubocd.backend %q: supported values are flux" $backend) -}}
{{- end -}}
{{- $mode := dig "kubocd" "persistence" "mode" "" $engine | toString | trim | default "kubernetes" -}}
{{- if not (has $mode (list "kubernetes" "git")) -}}
{{- fail (printf "unsupported engine.kubocd.persistence.mode %q: supported values are kubernetes, git" $mode) -}}
{{- end -}}
{{- $git := dig "kubocd" "persistence" "git" dict $engine -}}
{{- $sync := dig "sync" "" $git | toString | trim | default "flux" -}}
{{- if not (has $sync (list "flux" "none")) -}}
{{- fail (printf "unsupported engine.kubocd.persistence.git.sync %q: supported values are flux, none" $sync) -}}
{{- end -}}
{{- $url := include "okdp-control-plane-server.gitUrl" . -}}
{{- if and (eq $mode "git") (not $url) -}}
{{- fail "engine.kubocd.persistence.git.url is required in git mode" -}}
{{- end -}}
{{- $_ := include "okdp-control-plane-server.gitPath" . -}}
{{- end }}

{{/*
Normalized engine persistence mode ("kubernetes" or "git").
*/}}
{{- define "okdp-control-plane-server.persistenceMode" -}}
{{- $engine := .Values.engine | default dict -}}
{{- dig "kubocd" "persistence" "mode" "" $engine | toString | trim | default "kubernetes" -}}
{{- end }}

{{/*
Normalized git sync mode ("flux" or "none").
*/}}
{{- define "okdp-control-plane-server.gitSync" -}}
{{- $git := dig "kubocd" "persistence" "git" dict (.Values.engine | default dict) -}}
{{- dig "sync" "" $git | toString | trim | default "flux" -}}
{{- end }}

{{/*
Normalized git branch (defaults to "main" even when set to empty string).
*/}}
{{- define "okdp-control-plane-server.gitBranch" -}}
{{- $git := dig "kubocd" "persistence" "git" dict (.Values.engine | default dict) -}}
{{- dig "branch" "" $git | toString | trim | default "main" -}}
{{- end }}

{{/*
Normalized git path under the repository root, without leading "./" or "/" and
without "..", backslash, or ".git" segments.
*/}}
{{- define "okdp-control-plane-server.gitPath" -}}
{{- $git := dig "kubocd" "persistence" "git" dict (.Values.engine | default dict) -}}
{{- $rawPath := dig "path" "" $git | toString | trim -}}
{{- if contains "\\" $rawPath -}}
{{- fail "engine.kubocd.persistence.git.path must not contain backslashes" -}}
{{- end -}}
{{- $path := $rawPath | trimPrefix "./" | trimPrefix "/" | trimSuffix "/" -}}
{{- if regexMatch "(^|/)\\.\\.(/|$)" $path -}}
{{- fail "engine.kubocd.persistence.git.path must not contain '..' segments" -}}
{{- end -}}
{{- if regexMatch "(?i)(^|/)\\.git(/|$)" $path -}}
{{- fail "engine.kubocd.persistence.git.path must not contain '.git' segments" -}}
{{- end -}}
{{- $path -}}
{{- end }}

{{/*
Labels and annotations of the git mode sync objects: the label a server back
in kubernetes mode looks for, and the keep policy of keepSyncOnUninstall.
*/}}
{{- define "okdp-control-plane-server.syncMetadata" -}}
labels:
  {{- include "okdp-control-plane-server.labels" . | nindent 2 }}
  app.kubernetes.io/managed-by-control-plane: {{ include "okdp-control-plane-server.fullname" . }}
{{- if dig "kubocd" "persistence" "git" "keepSyncOnUninstall" true (.Values.engine | default dict) }}
annotations:
  helm.sh/resource-policy: keep
{{- end }}
{{- end }}

{{/*
Validated git repository URL: rejects query strings, fragments, unsupported
schemes (such as file:// or git://), and credentials embedded in the URL before
rendering a ConfigMap or GitRepository. Mirrors gitstore.parseURL rules across
http://, https://, ssh://, and SCP-style user@host:path.
*/}}
{{- define "okdp-control-plane-server.gitUrl" -}}
{{- $url := dig "kubocd" "persistence" "git" "url" "" (.Values.engine | default dict) | toString | trim -}}
{{- if $url -}}
{{- if or (contains "?" $url) (contains "#" $url) -}}
{{- fail "engine.kubocd.persistence.git.url must not contain '?' or '#': use engine.kubocd.persistence.git.secretName for credentials" -}}
{{- else if regexMatch "(?i)^ssh://" $url -}}
{{- $rest := regexReplaceAll "(?i)^ssh://" $url "" -}}
{{- if regexMatch "^[^@]*:.*@" $rest -}}
{{- fail "engine.kubocd.persistence.git.url must not embed a password: use engine.kubocd.persistence.git.secretName instead" -}}
{{- end -}}
{{- else if regexMatch "(?i)^https?://" $url -}}
{{- if contains "@" $url -}}
{{- fail "engine.kubocd.persistence.git.url must not embed credentials: use engine.kubocd.persistence.git.secretName instead" -}}
{{- end -}}
{{- else if contains "://" $url -}}
{{- if contains "@" $url -}}
{{- fail "engine.kubocd.persistence.git.url must not embed credentials: use engine.kubocd.persistence.git.secretName instead" -}}
{{- end -}}
{{- fail "engine.kubocd.persistence.git.url has an unsupported scheme: supported formats are http://, https://, ssh://, or SCP-style user@host:path" -}}
{{- else if hasPrefix "//" $url -}}
{{- if contains "@" $url -}}
{{- fail "engine.kubocd.persistence.git.url must not embed credentials: use engine.kubocd.persistence.git.secretName instead" -}}
{{- end -}}
{{- fail "engine.kubocd.persistence.git.url has an unsupported scheme: supported formats are http://, https://, ssh://, or SCP-style user@host:path" -}}
{{- else if regexMatch "^[^@]*:.*@" $url -}}
{{- fail "engine.kubocd.persistence.git.url must not embed a password: use engine.kubocd.persistence.git.secretName instead" -}}
{{- else if not (regexMatch "^[^/:@\\s]+@(\\[[^\\]]+\\]|[^/:@\\s]+):.+" $url) -}}
{{- fail "engine.kubocd.persistence.git.url has an unsupported format: supported formats are http://, https://, ssh://, or SCP-style user@host:path" -}}
{{- end -}}
{{- end -}}
{{- $url -}}
{{- end }}
