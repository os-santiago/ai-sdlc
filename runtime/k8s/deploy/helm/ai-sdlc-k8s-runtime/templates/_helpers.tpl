{{/* A paused installation requires explicit frozen authority. PostgreSQL and
the schema migrator remain available; every delivery Deployment uses this. */}}
{{- define "ai-sdlc-k8s-runtime.replicas" -}}
{{- if .Values.runtime.paused -}}
{{- $inner := .Values.authority.policyJSON | fromJson -}}
{{- if ne (toJson $inner.freeze) "true" -}}
{{- fail "paused installation requires explicit frozen inner authority" -}}
{{- end -}}
{{- if .Values.build.enabled -}}
{{- $outer := .Values.build.outerPolicyJSON | fromJson -}}
{{- if ne (toJson $outer.freeze) "true" -}}
{{- fail "paused installation requires explicit frozen outer authority" -}}
{{- end -}}
{{- end -}}
0
{{- else -}}
1
{{- end -}}
{{- end -}}

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
{{- .Values.namespace.name | default .Release.Namespace }}
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

{{/* The immutable project manifest ConfigMap is named by its content digest,
so installing a different manifest creates a new object instead of failing
on immutable data. */}}
{{- define "ai-sdlc-k8s-runtime.projectConfigName" -}}
{{- printf "%s-project-%s" (include "ai-sdlc-k8s-runtime.fullname" .) (.Values.project.manifestJSON | sha256sum | trunc 12) -}}
{{- end }}

{{/*
OpenShift DNS egress: the cluster DNS pods run in openshift-dns and listen on
5353 behind the dns-default Service's port 53. Network policy matches the
pod port, so kube-system:53 never applies there.
*/}}
{{- define "ai-sdlc-k8s-runtime.openshiftDNSEgress" -}}
- to:
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: openshift-dns}}
      podSelector: {matchLabels: {dns.operator.openshift.io/daemonset-dns: default}}
  ports: [{protocol: UDP, port: 5353}, {protocol: TCP, port: 5353}]
{{- end }}

{{/*
Sandbox pod overhead headroom. RuntimeClass admission adds the class's
overhead.podFixed to every pod, and ResourceQuota counts it. When
sandbox.podOverhead is set, each sandbox namespace quota grows by that overhead
for every pod it admits; the worker and verifier refuse a class whose overhead
exceeds it. Input: dict "root" $ "pods" N "quota" (dict of the four fields).
Without an overhead the caller renders its original quota unchanged.
*/}}
{{- define "ai-sdlc-k8s-runtime.sandboxQuota" -}}
{{- $o := .root.Values.sandbox.podOverhead | default dict -}}
{{- $cpu := mul .pods (trimSuffix "m" ($o.cpu | default "0m") | atoi) -}}
{{- $mem := mul .pods (trimSuffix "Mi" ($o.memory | default "0Mi") | atoi) -}}
{{- $out := list -}}
{{- range $key := list "requests.cpu" "requests.memory" "limits.cpu" "limits.memory" -}}
{{- $q := toString (index $.quota $key) -}}
{{- if hasSuffix ".cpu" $key -}}
{{- $m := 0 -}}
{{- if regexMatch "^[0-9]+m$" $q -}}{{- $m = trimSuffix "m" $q | atoi -}}
{{- else if regexMatch "^[0-9]+$" $q -}}{{- $m = mul (atoi $q) 1000 -}}
{{- else -}}{{- fail (printf "quota %s %s must be whole cores or millicores to add sandbox.podOverhead" $key $q) -}}{{- end -}}
{{- $out = append $out (printf "%s: %q" $key (printf "%dm" (add $m $cpu))) -}}
{{- else -}}
{{- $mi := 0 -}}
{{- if regexMatch "^[0-9]+Mi$" $q -}}{{- $mi = trimSuffix "Mi" $q | atoi -}}
{{- else if regexMatch "^[0-9]+Gi$" $q -}}{{- $mi = mul (trimSuffix "Gi" $q | atoi) 1024 -}}
{{- else -}}{{- fail (printf "quota %s %s must be whole Mi or Gi to add sandbox.podOverhead" $key $q) -}}{{- end -}}
{{- $out = append $out (printf "%s: %q" $key (printf "%dMi" (add $mi $mem))) -}}
{{- end -}}
{{- end -}}
{{- join ", " $out -}}
{{- end -}}

{{- define "ai-sdlc-k8s-runtime.sandboxOverheadSet" -}}
{{- $o := .Values.sandbox.podOverhead | default dict -}}
{{- if or $o.cpu $o.memory }}true{{ end -}}
{{- end -}}

{{/*
The sandbox RuntimeClass a configuration document selects, matching the field
name case-insensitively as the runtime's JSON decoding does. Absent means the
default; more than one spelling fails. Input: dict "doc" map "field" label.
*/}}
{{- define "ai-sdlc-k8s-runtime.sandboxClassField" -}}
{{- $found := list -}}
{{- range $key, $value := .doc -}}
{{- if eq (lower $key) "runtimeclass" -}}{{- $found = append $found $value -}}{{- end -}}
{{- end -}}
{{- if gt (len $found) 1 -}}{{- fail (printf "the %s field appears more than once with different capitalization" .field) -}}{{- end -}}
{{- if $found -}}{{- first $found | default "ai-sdlc-offline" -}}{{- else -}}ai-sdlc-offline{{- end -}}
{{- end -}}

{{/*
The execution configuration's placement object as JSON, matching the field
name case-insensitively as the runtime does. Absent means an empty object;
more than one spelling fails. Input: the decoded configuration document.
*/}}
{{- define "ai-sdlc-k8s-runtime.placementField" -}}
{{- $found := list -}}
{{- range $key, $value := . -}}
{{- if eq (lower $key) "placement" -}}{{- $found = append $found $value -}}{{- end -}}
{{- end -}}
{{- if gt (len $found) 1 -}}{{- fail "the execution configuration placement field appears more than once with different capitalization" -}}{{- end -}}
{{- $placement := dict -}}
{{- if $found -}}{{- if not (kindIs "invalid" (first $found)) -}}{{- $placement = first $found -}}{{- end -}}{{- end -}}
{{- if not (kindIs "map" $placement) -}}{{- fail "the execution configuration placement must be an object" -}}{{- end -}}
{{- toJson $placement -}}
{{- end -}}

{{/*
generationToolchain renders "true" when project.manifestJSON declares a
generation.toolchain object, else nothing. The executor quota uses it.
*/}}
{{- define "ai-sdlc-k8s-runtime.generationToolchain" -}}
{{- with .Values.project.manifestJSON -}}
{{- $m := fromJson . -}}
{{- /* fromJson reports malformed or non-object JSON as an "Error" key, which a
       strict manifest can never contain: refuse rather than size 4Gi. */ -}}
{{- if or (hasKey $m "Error") (not (hasKey $m "version")) -}}
{{- fail "project.manifestJSON must be the installer's canonical manifest JSON object" -}}
{{- end -}}
{{- if kindIs "map" (get $m "generation") -}}
{{- if kindIs "map" (get (get $m "generation") "toolchain") -}}true{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
dependencyCache renders "true" when project.manifestJSON declares a
verification.dependencies object, else nothing. The verification controller
then may read (only read) the sealed dependency cache claims.
*/}}
{{- define "ai-sdlc-k8s-runtime.dependencyCache" -}}
{{- with .Values.project.manifestJSON -}}
{{- $m := fromJson . -}}
{{- if or (hasKey $m "Error") (not (hasKey $m "version")) -}}
{{- fail "project.manifestJSON must be the installer's canonical manifest JSON object" -}}
{{- end -}}
{{- if kindIs "map" (get $m "verification") -}}
{{- if kindIs "map" (get (get $m "verification") "dependencies") -}}true{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
