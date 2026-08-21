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
Pod-level seccomp profile, as a fragment to splice into a pod securityContext.

It comes from one top-level value so every pod the chart owns gets the same
answer. The enricher-capable pods are the reason it exists -- they fork
pandoc/tesseract/pdftotext on attacker-chosen bytes, so the kernel ABI those
parsers can reach is the remote-code-execution surface -- but a syscall filter
is cheap enough that carving out exceptions for the other pods would cost more
review attention than it saves.

Rendering nothing when `seccompProfile.type` is empty is the escape hatch: an
operator whose runtime default profile breaks a workload sets it to "" and gets
the previous behaviour back, instead of forking the chart.
*/}}
{{- define "glovebox.seccompProfile" -}}
{{- $p := .Values.seccompProfile | default dict -}}
{{- if $p.type -}}
seccompProfile:
  type: {{ $p.type }}
{{- if eq $p.type "Localhost" }}
  localhostProfile: {{ required "seccompProfile.localhostProfile is required when seccompProfile.type is Localhost: the kubelet loads the profile from a path relative to its own seccomp root, and there is no sensible default for it" $p.localhostProfile | quote }}
{{- end }}
{{- end -}}
{{- end }}

{{/*
RuntimeClass for a pod, as a fragment: `.workload` is the per-workload map
(a connector or importer entry), `.root` the chart root context.

Empty by default and rendered only when set, because choosing a runtime is the
operator's decision, not the chart's: gVisor and Kata are per-cluster
installations with their own node pools and their own failure modes, so a
chart that assumed one would break every cluster that does not have it. The
value exists so an operator who HAS installed one can point the enricher
workloads at it without patching manifests -- which is the only part of that
decision the chart can usefully own.

Returns the bare name (empty when unset) so callers can wrap it in `with` and
render nothing at all rather than a blank line.
*/}}
{{- define "glovebox.runtimeClassName" -}}
{{- (.workload).runtimeClassName | default .root.Values.runtimeClassName -}}
{{- end }}
