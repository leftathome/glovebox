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

{{/*
Expand one numeric cron field into the values it matches, comma-joined.
`.field` is the field text, `.lo`/`.hi` its legal range, `.what` a label for
error messages.

Understands what the chart has to be able to reason about: `*`, a number, a
range `a-b`, a step on any of those (star-slash-n, `a-b/n`, `a/n` meaning
"from a"), and comma lists of them. (The star-slash form cannot be written
out here: it would end this template comment.) Anything else -- names, `?`, `L`, a macro -- fails
the render: a schedule the chart cannot evaluate is a schedule it cannot
check, and the caller exists to check one.
*/}}
{{- define "glovebox.cronFieldValues" -}}
{{- $lo := int .lo -}}
{{- $hi := int .hi -}}
{{- $what := .what -}}
{{- $out := list -}}
{{- range $part := splitList "," .field -}}
  {{- $step := 1 -}}
  {{- $base := $part -}}
  {{- $stepped := false -}}
  {{- if contains "/" $part -}}
    {{- $bits := splitList "/" $part -}}
    {{- if or (ne (len $bits) 2) (not (regexMatch "^[0-9]+$" (index $bits 1))) -}}
      {{- fail (printf "%s: cannot evaluate %q" $what $part) -}}
    {{- end -}}
    {{- $base = index $bits 0 -}}
    {{- $step = atoi (index $bits 1) -}}
    {{- $stepped = true -}}
    {{- if lt $step 1 -}}
      {{- fail (printf "%s: step in %q must be at least 1" $what $part) -}}
    {{- end -}}
  {{- end -}}
  {{- $from := $lo -}}
  {{- $to := $hi -}}
  {{- if eq $base "*" -}}
  {{- else if regexMatch "^[0-9]+$" $base -}}
    {{- $from = atoi $base -}}
    {{- if not $stepped -}}
      {{- $to = $from -}}
    {{- end -}}
  {{- else if regexMatch "^[0-9]+-[0-9]+$" $base -}}
    {{- $ends := splitList "-" $base -}}
    {{- $from = atoi (index $ends 0) -}}
    {{- $to = atoi (index $ends 1) -}}
  {{- else -}}
    {{- fail (printf "%s: cannot evaluate %q (use numbers, lists, ranges and steps)" $what $part) -}}
  {{- end -}}
  {{- if or (lt $from $lo) (gt $to $hi) (gt $from $to) -}}
    {{- fail (printf "%s: %q is outside %d-%d" $what $part $lo $hi) -}}
  {{- end -}}
  {{- range $v := untilStep $from (int (add $to 1)) $step -}}
    {{- $out = append $out $v -}}
  {{- end -}}
{{- end -}}
{{- $out | uniq | join "," -}}
{{- end }}

{{/*
"HH:MM" as minutes since midnight, or a failed render. `.value` is the text,
`.what` a label for the error message.
*/}}
{{- define "glovebox.clockMinutes" -}}
{{- $v := toString .value -}}
{{- if not (regexMatch "^([01][0-9]|2[0-3]):[0-5][0-9]$" $v) -}}
  {{- fail (printf "%s must be HH:MM (00:00-23:59), got %q" .what $v) -}}
{{- end -}}
{{- add (mul (atoi (substr 0 2 $v)) 60) (atoi (substr 3 5 $v)) -}}
{{- end }}

{{/*
Refuse a PowerSchool login schedule that can start a run inside the blackout.

The account behind the job is its owner's own, and the portal keeps one live
session per account: a login inside the window signs the owner out. The
binary refuses to log in there whatever started it; this is the earlier,
louder check, at render time, so a bad schedule never reaches the cluster.

`.name` is the schedules key, `.schedule` its cron expression, `.root` the
chart root. Every (hour, minute) the expression matches is tested, on every
day (the window is daily, so the day fields do not matter), together with the
catch-up span after it: a run may start up to startingDeadlineSeconds late.

Renders nothing; fails the render on a violation.
*/}}
{{- define "glovebox.powerschoolScheduleCheck" -}}
{{- $r := .root.Values.powerschoolAuthRefresher -}}
{{- $schedule := toString .schedule -}}
{{- $where := printf "powerschoolAuthRefresher.schedules.%s" .name -}}
{{- if ne (toString $r.timeZone) (toString $r.blackout.timeZone) -}}
  {{- fail (printf "powerschoolAuthRefresher.timeZone (%q) must equal powerschoolAuthRefresher.blackout.timeZone (%q): a schedule can only be checked against the login blackout when both are in the same zone" (toString $r.timeZone) (toString $r.blackout.timeZone)) -}}
{{- end -}}
{{- $fields := regexSplit "\\s+" (trim $schedule) -1 -}}
{{- if ne (len $fields) 5 -}}
  {{- fail (printf "%s.schedule %q must be a five-field cron expression (no @macros): the chart has to check it against the login blackout" $where $schedule) -}}
{{- end -}}
{{- $start := int (include "glovebox.clockMinutes" (dict "value" $r.blackout.start "what" "powerschoolAuthRefresher.blackout.start")) -}}
{{- $end := int (include "glovebox.clockMinutes" (dict "value" $r.blackout.end "what" "powerschoolAuthRefresher.blackout.end")) -}}
{{- if eq $start $end -}}
  {{- fail "powerschoolAuthRefresher.blackout.start and .end are equal: the window cannot be empty" -}}
{{- end -}}
{{- $deadline := int ($r.startingDeadlineSeconds | default 0) -}}
{{- if lt $deadline 10 -}}
  {{- fail "powerschoolAuthRefresher.startingDeadlineSeconds must be set (10 or more): without it a missed run can start at any later time, including inside the login blackout" -}}
{{- end -}}
{{- $late := int (div (add $deadline 59) 60) -}}
{{- $window := printf "%s-%s %s" (toString $r.blackout.start) (toString $r.blackout.end) (toString $r.blackout.timeZone) -}}
{{- $minutes := splitList "," (include "glovebox.cronFieldValues" (dict "field" (index $fields 0) "lo" 0 "hi" 59 "what" (printf "%s.schedule minute field" $where))) -}}
{{- $hours := splitList "," (include "glovebox.cronFieldValues" (dict "field" (index $fields 1) "lo" 0 "hi" 23 "what" (printf "%s.schedule hour field" $where))) -}}
{{- range $h := $hours -}}
  {{- range $m := $minutes -}}
    {{- $t := int (add (mul (atoi $h) 60) (atoi $m)) -}}
    {{- $inside := false -}}
    {{- if lt $start $end -}}
      {{- $inside = and (ge $t $start) (lt $t $end) -}}
    {{- else -}}
      {{- $inside = or (ge $t $start) (lt $t $end) -}}
    {{- end -}}
    {{- if $inside -}}
      {{- fail (printf "%s.schedule %q fires at %02d:%02d, inside the login blackout %s: a login then signs the account's owner out of the portal. Move the schedule outside the window" $where $schedule (atoi $h) (atoi $m) $window) -}}
    {{- end -}}
    {{- $untilStart := int (mod (add (sub $start $t) 1440) 1440) -}}
    {{- if le $untilStart $late -}}
      {{- fail (printf "%s.schedule %q fires at %02d:%02d, %d minute(s) before the login blackout %s, and a missed run may start up to %d minute(s) late (startingDeadlineSeconds: %d). Move the schedule earlier or shorten the deadline" $where $schedule (atoi $h) (atoi $m) $untilStart $window $late $deadline) -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- end }}

{{/*
Name of the Secret holding the PowerSchool account's hostname, username and
password for the login job: the operator's existing one, or the one the
chart's own ExternalSecret produces.
*/}}
{{- define "glovebox.powerschoolRefresherCredentialsSecret" -}}
{{- $c := .Values.powerschoolAuthRefresher.credentials | default dict -}}
{{- if $c.existingSecret -}}
{{- $c.existingSecret -}}
{{- else -}}
{{- include "glovebox.fullname" . }}-powerschool-refresher-credentials
{{- end -}}
{{- end }}
