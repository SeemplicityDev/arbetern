{{/*
Expand the name of the chart.
*/}}
{{- define "arbetern.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "arbetern.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "arbetern.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "arbetern.labels" -}}
helm.sh/chart: {{ include "arbetern.chart" . }}
{{ include "arbetern.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "arbetern.selectorLabels" -}}
app.kubernetes.io/name: {{ include "arbetern.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "arbetern.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "arbetern.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Session app name. It must differ from arbetern.name so arbetern's Service and PDB never select session pods.
*/}}
{{- define "arbetern.projects.name" -}}
{{- printf "%s-session" (include "arbetern.name" . | trunc 56 | trimSuffix "-") }}
{{- end }}

{{- define "arbetern.projects.namespace" -}}
{{- .Values.projects.sessions.namespace.name | default .Release.Namespace }}
{{- end }}

{{- define "arbetern.projects.selectorLabels" -}}
app.kubernetes.io/name: {{ include "arbetern.projects.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: session
{{- end }}

{{- define "arbetern.projects.labels" -}}
helm.sh/chart: {{ include "arbetern.chart" . }}
{{ include "arbetern.projects.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "arbetern.projects.sessionName" -}}
{{- printf "%s-session" (include "arbetern.fullname" .) }}
{{- end }}

{{- define "arbetern.projects.hostConfigName" -}}
{{- printf "%s-session-host-config" (include "arbetern.fullname" .) }}
{{- end }}

{{- define "arbetern.projects.jobTemplateName" -}}
{{- printf "%s-session-job" (include "arbetern.fullname" .) }}
{{- end }}

{{- define "arbetern.projects.gatewayName" -}}
{{- printf "%s-projects-gateway" (include "arbetern.fullname" . | trunc 46 | trimSuffix "-") }}
{{- end }}

{{- define "arbetern.projects.gatewayHost" -}}
{{- printf "%s.%s.svc.cluster.local" (include "arbetern.projects.gatewayName" .) .Release.Namespace }}
{{- end }}

{{- define "arbetern.projects.gatewayURL" -}}
{{- printf "http://%s:%d" (include "arbetern.projects.gatewayHost" .) (int .Values.projects.gateway.port) }}
{{- end }}

{{- define "arbetern.projects.imageRepository" -}}
{{- .Values.projects.sessions.image.repository | default .Values.image.repository }}
{{- end }}

{{- define "arbetern.projects.image" -}}
{{- printf "%s:%s" (include "arbetern.projects.imageRepository" .) (.Values.projects.sessions.image.tag | default "runner") }}
{{- end }}

{{- define "arbetern.projects.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 10001
runAsGroup: 10001
fsGroup: 10001
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{- define "arbetern.projects.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop:
    - ALL
{{- end }}

{{/*
Fails the render on projects values that arbetern would reject at startup.
*/}}
{{- define "arbetern.projects.validate" -}}
{{- $p := .Values.projects }}
{{- $port := int $p.gateway.port }}
{{- if or (eq $port (int .Values.containerPort)) (eq $port (int (index (.Values.env | default dict) "PORT"))) (and .Values.headroom.enabled (eq $port (int .Values.headroom.port))) (and .Values.modelRouter.enabled (eq $port (int .Values.modelRouter.port))) }}
{{- fail "projects.gateway.port must differ from containerPort, env.PORT, headroom.port and modelRouter.port" }}
{{- end }}
{{- if and (eq (include "arbetern.projects.namespace" .) .Release.Namespace) (not (and $p.sessions.admissionPolicy.enabled (.Capabilities.APIVersions.Has "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy"))) }}
{{- fail "sessions in the release namespace need projects.sessions.admissionPolicy.enabled and a cluster serving admissionregistration.k8s.io/v1 ValidatingAdmissionPolicy (helm template: pass --api-versions admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy)" }}
{{- end }}
{{- if not (regexMatch "^(acceptEdits|auto|bypassPermissions|dontAsk|manual)$" (toString $p.sessions.permissionMode)) }}
{{- fail "projects.sessions.permissionMode must be bypassPermissions, acceptEdits, dontAsk, auto or manual" }}
{{- end }}
{{- if not (regexMatch "^(sonnet|opus|haiku|fable|claude-[A-Za-z0-9.-]+)?$" (toString $p.sessions.model)) }}
{{- fail "projects.sessions.model must be a label (sonnet, opus, haiku, fable) or an Anthropic model ID such as claude-sonnet-5-5" }}
{{- end }}
{{- end }}

{{/*
Fails the render on model router values that would collide with another container in the pod.
*/}}
{{- define "arbetern.modelRouter.validate" -}}
{{- $port := int .Values.modelRouter.port }}
{{- if or (eq $port (int .Values.containerPort)) (eq $port (int (index (.Values.env | default dict) "PORT"))) (and .Values.headroom.enabled (eq $port (int .Values.headroom.port))) }}
{{- fail "modelRouter.port must differ from containerPort, env.PORT and headroom.port" }}
{{- end }}
{{- with .Values.modelRouter.model.docker }}
{{- if and .digest (not (regexMatch "^sha256:[a-f0-9]{64}$" (toString .digest))) }}
{{- fail "modelRouter.model.docker.digest must be sha256:<64 hex characters>" }}
{{- end }}
{{- if and .digest (not (regexMatch "^[a-z0-9]+([._-][a-z0-9]+)*/[a-z0-9]+([._-][a-z0-9]+)*$" (toString .repository))) }}
{{- fail "modelRouter.model.docker.repository must be a Docker Hub <namespace>/<name>, e.g. ai/qwen3" }}
{{- end }}
{{- end }}
{{- end }}
