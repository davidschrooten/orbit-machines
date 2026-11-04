{{/*
Expand the name of the chart.
*/}}
{{- define "orbit-machines.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "orbit-machines.fullname" -}}
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
{{- define "orbit-machines.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "orbit-machines.labels" -}}
helm.sh/chart: {{ include "orbit-machines.chart" . }}
{{ include "orbit-machines.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels for orbit-dispatcher
*/}}
{{- define "orbit-machines.selectorLabels" -}}
app.kubernetes.io/name: {{ include "orbit-machines.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: dispatcher
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "orbit-machines.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "orbit-machines.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the secret to use
*/}}
{{- define "orbit-machines.secretName" -}}
{{- if .Values.orbit.existingSecret }}
{{- .Values.orbit.existingSecret }}
{{- else }}
{{- printf "%s-secret" (include "orbit-machines.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Resolve the S3 endpoint URL
*/}}
{{- define "orbit-machines.s3Endpoint" -}}
{{- if .Values.s3.endpoint }}
{{- .Values.s3.endpoint }}
{{- else if .Values.s3.minio.enabled }}
{{- printf "http://%s-minio:9000" (include "orbit-machines.fullname" .) }}
{{- else }}
{{- "https://s3.amazonaws.com" }}
{{- end }}
{{- end }}
