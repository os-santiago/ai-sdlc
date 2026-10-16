{{/*
Expand the name of the chart.
*/}}
{{- define "ai-sdlc-k8s-runtime.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "ai-sdlc-k8s-runtime.fullname" -}}
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

{{/*
Namespace to render every namespaced resource into.
*/}}
{{- define "ai-sdlc-k8s-runtime.namespace" -}}
{{- .Values.namespace.name | default "ai-sdlc-runtime" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "ai-sdlc-k8s-runtime.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "ai-sdlc-k8s-runtime.labels" -}}
helm.sh/chart: {{ include "ai-sdlc-k8s-runtime.chart" . }}
{{ include "ai-sdlc-k8s-runtime.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ai-sdlc
{{- end }}

{{/*
Selector labels
*/}}
{{- define "ai-sdlc-k8s-runtime.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ai-sdlc-k8s-runtime.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "ai-sdlc-k8s-runtime.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "ai-sdlc-k8s-runtime.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
ADR-0005: name of the Secret every component's DATABASE_URL env var reads
from. When postgres.cloudNativePG.enabled is true this is the Secret
CloudNativePG itself generates for the Cluster (<clusterName>-app);
otherwise it is databaseURLSecret.name, pointing at an existing instance.
*/}}
{{- define "ai-sdlc-k8s-runtime.databaseSecretName" -}}
{{- if .Values.postgres.cloudNativePG.enabled }}
{{- printf "%s-app" .Values.postgres.cloudNativePG.clusterName }}
{{- else }}
{{- .Values.postgres.databaseURLSecret.name }}
{{- end }}
{{- end }}

{{/*
ADR-0005: key within the database Secret holding a ready-to-use
postgresql:// connection string. CloudNativePG's generated Secret calls
this key "uri"; databaseURLSecret.key overrides it for an existing
instance whose Secret uses a different key name.
*/}}
{{- define "ai-sdlc-k8s-runtime.databaseSecretKey" -}}
{{- if .Values.postgres.cloudNativePG.enabled }}
{{- "uri" }}
{{- else }}
{{- .Values.postgres.databaseURLSecret.key }}
{{- end }}
{{- end }}
