{{- define "apple-importer.name" -}}
{{- default "apple-importer" .Chart.Name -}}
{{- end -}}

{{- define "apple-importer.fullname" -}}
{{- printf "%s-%s" .Release.Name "apple-importer" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "apple-importer.labels" -}}
app.kubernetes.io/name: {{ include "apple-importer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "apple-importer.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "apple-importer.archivePath" -}}
{{ .Values.archive.storagePath }}/archives/{{ .Values.archive.archiveID }}
{{- end -}}
