{{/*
Expand the name of the chart.
*/}}
{{- define "glovebox.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "glovebox.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-glovebox" .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Standard labels for all resources.
*/}}
{{- define "glovebox.labels" -}}
app.kubernetes.io/name: {{ include "glovebox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels for the main glovebox deployment.
*/}}
{{- define "glovebox.selectorLabels" -}}
app.kubernetes.io/name: {{ include "glovebox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: scanner
{{- end }}

{{/*
Selector labels for a connector deployment.
*/}}
{{- define "glovebox.connectorSelectorLabels" -}}
app.kubernetes.io/name: {{ include "glovebox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: connector
glovebox.io/connector: {{ .connector }}
{{- end }}

{{/*
Image string with appVersion fallback for tag.
*/}}
{{- define "glovebox.image" -}}
{{- printf "%s:%s" .repository (.tag | default $.Chart.AppVersion) }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "glovebox.serviceAccountName" -}}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- include "glovebox.fullname" . }}
{{- end }}
{{- end }}

{{/*
Port the bearer-authenticated endpoints (/v1/archives*, /v1/sanitize) are
served on. config.ingest.bearerPort unset or 0 means they share the
connector ingest port, which is what every install before this key did.
Setting it to a distinct port splits them onto a listener of their own.
*/}}
{{- define "glovebox.bearerPort" -}}
{{- if and .Values.config.ingest.bearerPort (ne (.Values.config.ingest.bearerPort | int) 0) -}}
{{- .Values.config.ingest.bearerPort | int -}}
{{- else -}}
{{- .Values.config.ingest.port | int -}}
{{- end -}}
{{- end }}

{{/*
Whether the bearer endpoints have a listener of their own. Renders a
non-empty string when they do and "" when they share the ingest port, so
callers can use it directly in an `if`.
*/}}
{{- define "glovebox.bearerPortSplit" -}}
{{- if ne (include "glovebox.bearerPort" .) (.Values.config.ingest.port | int | toString) -}}
true
{{- end -}}
{{- end }}

{{/*
Whether anything is served on config.ingest.port. It carries /v1/ingest in
every tls mode except `required`, where the connector intake exists only on
the mTLS listener; in that mode the port is still bound when the bearer
endpoints share it, and bound by nothing at all when they are split onto
their own port or switched off. Declaring a containerPort or a Service port
that nothing binds is how a Service ends up quietly routing to a closed
port, so the templates gate on this.
*/}}
{{- define "glovebox.ingestPortServed" -}}
{{- if ne .Values.ingest.tls.mode "required" -}}
true
{{- else if and .Values.ingest.auth.enabled (not (include "glovebox.bearerPortSplit" .)) -}}
true
{{- end -}}
{{- end }}

{{/*
Whether a separate bearer listener is actually opened. Both /v1/archives*
and /v1/sanitize are gated on ingest.auth.enabled server-side, so a split
port with auth off binds nothing and must not be declared.
*/}}
{{- define "glovebox.bearerPortServed" -}}
{{- if and (include "glovebox.bearerPortSplit" .) .Values.ingest.auth.enabled -}}
true
{{- end -}}
{{- end }}
