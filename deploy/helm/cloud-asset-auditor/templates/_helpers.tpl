{{/*
Expand the name of the chart.
*/}}
{{- define "cloud-asset-auditor.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name. Truncated to 63 chars per the
Kubernetes spec for DNS-1123 names.
*/}}
{{- define "cloud-asset-auditor.fullname" -}}
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
Chart name and version, joined the way Helm conventions specify.
*/}}
{{- define "cloud-asset-auditor.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Standard labels emitted on every resource.
*/}}
{{- define "cloud-asset-auditor.labels" -}}
helm.sh/chart: {{ include "cloud-asset-auditor.chart" . }}
{{ include "cloud-asset-auditor.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels (subset of the full set that's stable across upgrades).
*/}}
{{- define "cloud-asset-auditor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cloud-asset-auditor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name (uses the override when set; otherwise the fullname).
*/}}
{{- define "cloud-asset-auditor.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cloud-asset-auditor.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference: repository + tag (defaults to .Chart.AppVersion).
*/}}
{{- define "cloud-asset-auditor.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Standard env-var block injected into the auditor container. Sourced from
the credentials Secret (when set) plus extraEnv.
*/}}
{{- define "cloud-asset-auditor.envFrom" -}}
{{- if .Values.credentials.existingSecret }}
- secretRef:
    name: {{ .Values.credentials.existingSecret }}
{{- end }}
{{- end }}

{{/*
Args for `auditor audit ...`. Used by the CronJob and tests.
*/}}
{{- define "cloud-asset-auditor.auditArgs" -}}
{{- $args := list "audit" -}}
{{- if .Values.providers -}}
{{- $args = append $args "--provider" -}}
{{- $args = append $args (join "," .Values.providers) -}}
{{- end -}}
{{- $args = append $args "-o" -}}
{{- $args = append $args .Values.audit.outputFormat -}}
{{- if .Values.audit.timeout -}}
{{- $args = append $args "--timeout" -}}
{{- $args = append $args (.Values.audit.timeout | toString) -}}
{{- end -}}
{{- if .Values.audit.maxConcurrency -}}
{{- $args = append $args "--max-concurrency" -}}
{{- $args = append $args (.Values.audit.maxConcurrency | toString) -}}
{{- end -}}
{{- if .Values.audit.includeRaw -}}
{{- $args = append $args "--include-raw" -}}
{{- end -}}
{{- if and .Values.cronjob.pvc.enabled (eq .Values.mode "cronjob") -}}
{{- $args = append $args "--output-file" -}}
{{- $args = append $args (printf "%s/assets-$(POD_NAME).%s" .Values.cronjob.pvc.mountPath .Values.audit.outputFormat) -}}
{{- end -}}
{{- range .Values.audit.extraArgs -}}
{{- $args = append $args . -}}
{{- end -}}
{{- toYaml $args -}}
{{- end }}
