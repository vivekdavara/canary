{{/*
The name every object of the release is called by: the release's name, exactly. One name per release, so two releases
can't share a ClusterRole (a mapping that appended "-canary" to some names gave "foo" and "foo-canary" the same one).
Helm limits release names to 53 characters, inside every limit the names here have.
*/}}
{{- define "canary.fullname" -}}
{{- .Release.Name -}}
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
