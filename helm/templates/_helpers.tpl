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
Runner app name. It must differ from arbetern.name so arbetern's Service and PDB never select runner pods.
*/}}
{{- define "arbetern.projects.name" -}}
{{- printf "%s-runner" (include "arbetern.name" . | trunc 56 | trimSuffix "-") }}
{{- end }}

{{- define "arbetern.projects.namespace" -}}
{{- .Values.projects.runners.namespace.name | default .Release.Namespace }}
{{- end }}

{{- define "arbetern.projects.selectorLabels" -}}
app.kubernetes.io/name: {{ include "arbetern.projects.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: claude-code-self-hosted-runner
{{- end }}

{{- define "arbetern.projects.labels" -}}
helm.sh/chart: {{ include "arbetern.chart" . }}
{{ include "arbetern.projects.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "arbetern.projects.orchestratorSelectorLabels" -}}
{{ include "arbetern.projects.selectorLabels" . }}
app.kubernetes.io/component: orchestrator
{{- end }}

{{- define "arbetern.projects.sessionRunnerSelectorLabels" -}}
{{ include "arbetern.projects.selectorLabels" . }}
app.kubernetes.io/component: session-runner
{{- end }}

{{- define "arbetern.projects.orchestratorName" -}}
{{- printf "%s-orchestrator" (include "arbetern.fullname" .) }}
{{- end }}

{{- define "arbetern.projects.sessionRunnerName" -}}
{{- printf "%s-session-runner" (include "arbetern.fullname" .) }}
{{- end }}

{{- define "arbetern.projects.hostConfigName" -}}
{{- printf "%s-runner-host-config" (include "arbetern.fullname" .) }}
{{- end }}

{{- define "arbetern.projects.jobTemplateName" -}}
{{- printf "%s-runner-job" (include "arbetern.fullname" .) }}
{{- end }}

{{- define "arbetern.projects.environmentSecretName" -}}
{{- .Values.projects.runners.existingEnvironmentSecret | default (printf "%s-runner-environment" (include "arbetern.fullname" .)) }}
{{- end }}

{{- define "arbetern.projects.gatewayName" -}}
{{- printf "%s-projects-gateway" (include "arbetern.fullname" . | trunc 46 | trimSuffix "-") }}
{{- end }}

{{- define "arbetern.projects.gatewayURL" -}}
{{- printf "http://%s.%s.svc.cluster.local:%d" (include "arbetern.projects.gatewayName" .) .Release.Namespace (int .Values.projects.gateway.port) }}
{{- end }}

{{- define "arbetern.projects.imageRepository" -}}
{{- .Values.projects.runners.image.repository | default .Values.image.repository }}
{{- end }}

{{- define "arbetern.projects.image" -}}
{{- printf "%s:%s" (include "arbetern.projects.imageRepository" .) (.Values.projects.runners.image.tag | default "runner") }}
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
Fails the render on projects values that arbetern or the runners would reject at startup.
*/}}
{{- define "arbetern.projects.validate" -}}
{{- $p := .Values.projects }}
{{- $id := required "projects.environmentId is required when projects.enabled is true (the ccpool_… id of the self-hosted environment)" $p.environmentId }}
{{- if not (regexMatch "^ccpool_[A-Za-z0-9]{1,64}$" (toString $id)) }}
{{- fail "projects.environmentId must match ^ccpool_[A-Za-z0-9]{1,64}$" }}
{{- end }}
{{- with $p.runnerAccountId }}
{{- if not (regexMatch "^user_[A-Za-z0-9]{1,64}$" (toString .)) }}
{{- fail "projects.runnerAccountId must match ^user_[A-Za-z0-9]{1,64}$" }}
{{- end }}
{{- end }}
{{- if not (or $p.runners.existingEnvironmentSecret (and .Values.createSecret $p.runners.environmentKey)) }}
{{- fail "projects.runners.environmentKey (with createSecret: true) or projects.runners.existingEnvironmentSecret is required when projects.enabled is true" }}
{{- end }}
{{- $port := int $p.gateway.port }}
{{- if or (eq $port (int .Values.containerPort)) (eq $port (int (index (.Values.env | default dict) "PORT"))) (and .Values.headroom.enabled (eq $port (int .Values.headroom.port))) (and .Values.modelRouter.enabled (eq $port (int .Values.modelRouter.port))) }}
{{- fail "projects.gateway.port must differ from containerPort, env.PORT, headroom.port and modelRouter.port" }}
{{- end }}
{{- if and (eq (include "arbetern.projects.namespace" .) .Release.Namespace) (not (and $p.runners.admissionPolicy.enabled (.Capabilities.APIVersions.Has "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy"))) }}
{{- fail "runners in the release namespace need projects.runners.admissionPolicy.enabled and a cluster serving admissionregistration.k8s.io/v1 ValidatingAdmissionPolicy (helm template: pass --api-versions admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy)" }}
{{- end }}
{{- range $p.triggers }}
{{- if not (regexMatch "^[a-z0-9][a-z0-9-]{0,31}$" (toString .)) }}
{{- fail (printf "projects.triggers entry %q must match ^[a-z0-9][a-z0-9-]{0,31}$" (toString .)) }}
{{- end }}
{{- if and $.Values.createSecret (not (index ($.Values.secretValues | default dict) (printf "project-trigger-%s" .))) }}
{{- fail (printf "secretValues.project-trigger-%s is required for projects.triggers entry %q when createSecret is true" . .) }}
{{- end }}
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
