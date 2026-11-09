{{/* Chart name, truncated to the 63-character label limit. */}}
{{- define "kflare.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified name used as the prefix of every resource. */}}
{{- define "kflare.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Common labels. */}}
{{- define "kflare.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "kflare.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kflare
{{- end }}

{{/* Labels that select the controller pods. */}}
{{- define "kflare.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kflare.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end }}

{{/* Name of the controller's ServiceAccount. */}}
{{- define "kflare.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kflare.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The leader election Lease's name: leaderElectionID, or the fullname's for an
installation scoped by watchNamespaces or watchSelector, so that those sharing a
namespace each have their own. Empty keeps the controller's default, which
earlier releases used, so that upgrading an unscoped installation never runs
two leaders at once.
*/}}
{{- define "kflare.leaderElectionID" -}}
{{- if .Values.leaderElectionID }}
{{- .Values.leaderElectionID }}
{{- else if or .Values.watchNamespaces .Values.watchSelector }}
{{- printf "%s.kflare.dev" (include "kflare.fullname" .) }}
{{- end }}
{{- end }}

{{/*
"true" when the controller's namespaced permissions go in a Role in each of
watchNamespaces rather than in its ClusterRole.
*/}}
{{- define "kflare.namespacedRBAC" -}}
{{- if and .Values.rbac.namespaced .Values.watchNamespaces }}true{{ end }}
{{- end }}

{{/* Controller image reference: by digest when one is set, otherwise by tag. */}}
{{- define "kflare.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}
