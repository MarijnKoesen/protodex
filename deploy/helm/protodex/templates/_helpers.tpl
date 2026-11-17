{{/* SPDX-License-Identifier: Apache-2.0 */}}

{{- define "protodex.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "protodex.fullname" -}}
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

{{- define "protodex.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Common labels (without component). */}}
{{- define "protodex.labels" -}}
helm.sh/chart: {{ include "protodex.chart" . }}
app.kubernetes.io/name: {{ include "protodex.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: protodex
{{- end }}

{{- define "protodex.selectorLabels" -}}
app.kubernetes.io/name: {{ include "protodex.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end }}

{{- define "protodex.seaweedfs.fullname" -}}
{{- printf "%s-seaweedfs" (include "protodex.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "protodex.seaweedfs.selectorLabels" -}}
app.kubernetes.io/name: {{ include "protodex.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: seaweedfs
{{- end }}

{{- define "protodex.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "protodex.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* image reference: repository[:tag][@digest]. Arg: dict with image and default tag. */}}
{{- define "protodex.imageRef" -}}
{{- $tag := .image.tag | default .defaultTag | toString }}
{{- if .image.digest }}
{{- printf "%s@%s" .image.repository .image.digest }}
{{- else }}
{{- printf "%s:%s" .image.repository $tag }}
{{- end }}
{{- end }}

{{- define "protodex.image" -}}
{{- include "protodex.imageRef" (dict "image" .Values.image "defaultTag" .Chart.AppVersion) }}
{{- end }}

{{- define "protodex.secretName" -}}
{{- default (include "protodex.fullname" .) .Values.existingSecret }}
{{- end }}

{{/* Effective S3 endpoint: the bundled SeaweedFS gateway when enabled. */}}
{{- define "protodex.s3.endpoint" -}}
{{- if .Values.seaweedfs.enabled }}
{{- printf "http://%s:8333" (include "protodex.seaweedfs.fullname" .) }}
{{- else }}
{{- .Values.s3.endpoint }}
{{- end }}
{{- end }}

{{- define "protodex.s3.forcePathStyle" -}}
{{- if .Values.seaweedfs.enabled }}true{{ else }}{{ .Values.s3.forcePathStyle }}{{ end }}
{{- end }}

{{/* Data directory = directory of config.dbPath. */}}
{{- define "protodex.dataDir" -}}
{{- dir .Values.config.dbPath }}
{{- end }}

{{/* Secret-backed env var; optional so ambient credentials can be used. */}}
{{- define "protodex.secretEnv" -}}
- name: {{ .name }}
  valueFrom:
    secretKeyRef:
      name: {{ include "protodex.secretName" .root }}
      key: {{ .key }}
      optional: {{ .optional }}
{{- end }}

{{/* Fail early on unsupported combinations. */}}
{{- define "protodex.validate" -}}
{{- if ne .Values.mode "sqlite" }}
{{- fail (printf "mode %q is not supported; only \"sqlite\" is implemented" .Values.mode) }}
{{- end }}
{{- if and .Values.seaweedfs.enabled (not .Values.existingSecret) (or (not .Values.s3.accessKeyId) (not .Values.s3.secretAccessKey)) }}
{{- fail "seaweedfs.enabled=true needs S3 credentials: set s3.accessKeyId and s3.secretAccessKey, or existingSecret" }}
{{- end }}
{{- if and .Values.litestream.enabled (not .Values.s3.bucket) }}
{{- fail "litestream.enabled=true requires s3.bucket" }}
{{- end }}
{{- end }}

{{- define "protodex.litestreamEnv" -}}
{{ include "protodex.secretEnv" (dict "root" . "name" "LITESTREAM_ACCESS_KEY_ID" "key" .Values.secretKeys.s3AccessKeyId "optional" true) }}
{{ include "protodex.secretEnv" (dict "root" . "name" "LITESTREAM_SECRET_ACCESS_KEY" "key" .Values.secretKeys.s3SecretAccessKey "optional" true) }}
{{- end }}
{{- define "protodex.litestreamMounts" -}}
- name: data
  mountPath: {{ include "protodex.dataDir" . }}
- name: litestream-config
  mountPath: /etc/litestream.yml
  subPath: litestream.yml
  readOnly: true
- name: tmp
  mountPath: /tmp
{{- end }}
{{- define "protodex.litestreamSidecar" -}}
{{- $root := .root -}}
- name: litestream
  image: {{ include "protodex.imageRef" (dict "image" $root.Values.litestream.image "defaultTag" "latest") | quote }}
  imagePullPolicy: {{ $root.Values.litestream.image.pullPolicy }}
  args: ["replicate", "-config", "/etc/litestream.yml"]
  {{- if .native }}
  restartPolicy: Always
  {{- end }}
  env:
    {{- include "protodex.litestreamEnv" $root | nindent 4 }}
  {{- if $root.Values.litestream.metrics.enabled }}
  ports:
    - name: ls-metrics
      containerPort: {{ $root.Values.litestream.metrics.port }}
      protocol: TCP
  {{- end }}
  securityContext:
    {{- toYaml $root.Values.securityContext | nindent 4 }}
  resources:
    {{- toYaml $root.Values.litestream.resources | nindent 4 }}
  volumeMounts:
    {{- include "protodex.litestreamMounts" $root | nindent 4 }}
{{- end }}
