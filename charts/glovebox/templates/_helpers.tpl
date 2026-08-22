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

{{/*
Whether one workload gets a read-only container root filesystem.
`.workload` is the per-workload map (a connector or importer entry), `.root`
the chart root context.

Returns the string "true" or nothing at all, so callers gate on it with a bare
`if` -- `{{- if (include "glovebox.readOnlyRootFilesystem" ...) }}`. All three
pieces this setting needs (the securityContext field, the /tmp mount, and the
emptyDir behind it) read the same helper, because a pod that got the read-only
root but not the writable /tmp is the failure this is most likely to ship.

A per-workload key wins over the chart-wide default when present, and `hasKey`
rather than `default` is the test on purpose: a per-workload `false` has to
survive, and `false | default true` is `true`.
*/}}
{{- define "glovebox.readOnlyRootFilesystem" -}}
{{- $w := .workload | default dict -}}
{{- $enabled := (.root.Values.readOnlyRootFilesystem | default dict).enabled -}}
{{- if hasKey $w "readOnlyRootFilesystem" -}}
{{- $enabled = $w.readOnlyRootFilesystem -}}
{{- end -}}
{{- if $enabled }}true{{ end -}}
{{- end }}

{{/*
Size cap for the writable /tmp emptyDir that accompanies a read-only root.
Same `.workload` / `.root` shape as the helper above.

An emptyDir with no sizeLimit is unbounded and backed by the node's ephemeral
storage, so a crafted document that made an enricher spool gigabytes would fill
the node's disk instead of failing its own pod. sizeLimit turns that into an
eviction of the one pod responsible.

Per-workload override exists because the two kinds of workload want different
numbers: a connector holds one item's temp dir at a time, while the apple
importer unpacks whole nested zips there. The chart-wide default is sized for
the importer, since the cost of being generous is a cap that never binds and
the cost of being tight is a failed import.
*/}}
{{- define "glovebox.tmpSizeLimit" -}}
{{- (.workload).tmpSizeLimit | default (.root.Values.readOnlyRootFilesystem | default dict).tmpSizeLimit | default "1Gi" -}}
{{- end }}
