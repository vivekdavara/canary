{{/*
The name every object of the release is called by: the release's name, or "<release>-canary" when the release's name
doesn't say canary. Cut to 52 characters, so "<name>-leader-election" still fits a label value's 63.
*/}}
{{- define "canary.fullname" -}}
{{- if contains "canary" .Release.Name -}}
{{- .Release.Name | trunc 52 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-canary" .Release.Name | trunc 52 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* The pods' selector. Never changes across upgrades: a Deployment's selector can't be changed. */}}
{{- define "canary.selectorLabels" -}}
app.kubernetes.io/name: canary
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "canary.labels" -}}
{{ include "canary.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
