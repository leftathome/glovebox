{{- define "mbox-importer.name" -}}
{{- default "mbox-importer" .Chart.Name -}}
{{- end -}}

{{- define "mbox-importer.fullname" -}}
{{- printf "%s-%s" .Release.Name "mbox-importer" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mbox-importer.labels" -}}
app.kubernetes.io/name: {{ include "mbox-importer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "mbox-importer.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "mbox-importer.archivePath" -}}
{{- if .Values.archive.filename -}}
{{ .Values.archive.storagePath }}/archives/{{ .Values.archive.archiveID }}/raw/{{ .Values.archive.filename }}
{{- else -}}
{{ .Values.archive.storagePath }}/archives/{{ .Values.archive.archiveID }}/raw
{{- end -}}
{{- end -}}
