{{/*
Deployment strategy for the core and connector Deployments, from
.Values.rolloutStrategy (values.yaml says when to set it). Call with the
root context. Renders nothing when the value is empty; rollingUpdate is
rendered only for type RollingUpdate.
*/}}
{{- define "glovebox.rolloutStrategy" -}}
{{- $s := .Values.rolloutStrategy | default dict -}}
{{- if $s -}}
{{- $type := $s.type | default "RollingUpdate" -}}
{{- if not (has $type (list "RollingUpdate" "Recreate")) -}}
{{- fail (printf "rolloutStrategy.type must be RollingUpdate or Recreate, got %v" $type) -}}
{{- end -}}
strategy:
  type: {{ $type }}
{{- if and (eq $type "RollingUpdate") $s.rollingUpdate }}
  rollingUpdate:
    {{- toYaml $s.rollingUpdate | nindent 4 }}
{{- end }}
{{- end }}
{{- end }}

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
ServiceAccount of the core service (the scanner Deployment) and of nothing
else. Its default name, <fullname>, is the name every pod used to share;
the core keeps it so that a Vault Kubernetes-auth role bound to it (the
ingest/archive listener's, ingest.auth.vault.k8sRole) keeps working across
the upgrade that gave every other workload an account of its own.
*/}}
{{- define "glovebox.serviceAccountName" -}}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- include "glovebox.fullname" . }}
{{- end }}
{{- end }}

{{/*
One ServiceAccount per connector Deployment (glovebox-wekc.19).

A connector is an identity: the record journal (spec 2026-10-16, section
4.9c) authorises writes by ServiceAccount, and the RSS connector, which
parses whatever the open internet sends it, must not share one with the
connectors that write children's school records -- or with the core.

These helpers take (dict "name" <connector key> "connector" <its values
entry> "root" $). The entry's optional `serviceAccount` map follows the
core's convention: `name` replaces the default <fullname>-connector-<name>,
`create: false` leaves the object to the operator, `annotations` are copied
onto it.
*/}}
{{- define "glovebox.connectorServiceAccountName" -}}
{{- $sa := (.connector | default dict).serviceAccount | default dict -}}
{{- if $sa.name -}}
{{- $sa.name -}}
{{- else -}}
{{- printf "%s-connector-%s" (include "glovebox.fullname" .root) .name -}}
{{- end -}}
{{- end }}

{{/*
Whether the chart renders that connector's ServiceAccount: "true" or "".
`hasKey` so that an explicit `create: false` survives.
*/}}
{{- define "glovebox.connectorServiceAccountCreate" -}}
{{- $sa := (.connector | default dict).serviceAccount | default dict -}}
{{- if or (not (hasKey $sa "create")) $sa.create -}}true{{- end -}}
{{- end }}

{{/*
The ServiceAccount of the connector with key .name, wherever it is
configured: connectors.<name>, or schoologyConnector for "schoology" (which
has a template of its own). Takes (dict "name" <key> "root" $). Used where
another workload has to name a connector's identity (the login broker's
allowed callers).
*/}}
{{- define "glovebox.connectorServiceAccountByKey" -}}
{{- $conns := .root.Values.connectors | default dict -}}
{{- $entry := dict -}}
{{- if hasKey $conns .name -}}
{{- $entry = index $conns .name | default dict -}}
{{- else if eq .name "schoology" -}}
{{- $entry = .root.Values.schoologyConnector | default dict -}}
{{- end -}}
{{- include "glovebox.connectorServiceAccountName" (dict "name" .name "connector" $entry "root" .root) -}}
{{- end }}

{{/*
Refuse a render in which two workloads would run as one ServiceAccount.

Per-workload accounts are only worth having if they stay per-workload, and
every one of them can be renamed: `connectors.rss.serviceAccount.name:
glovebox-glovebox` would put RSS back on the core's account (and render that
object twice), and naming another connector's account for the PowerSchool
connector would bind its ConfigMap-write Role to that connector. With
`create: false` nothing would even show the collision as a duplicate object.
So every resolved name is collected here -- the core, each enabled
connector (schoologyConnector included), importer and login broker, and both
refresher CronJobs -- and any name claimed twice fails the render.

Renders nothing. Included once, from serviceaccount.yaml.
*/}}
{{- define "glovebox.serviceAccountsUnique" -}}
{{- $seen := dict -}}
{{- $claims := list (list (include "glovebox.serviceAccountName" .) "the core (serviceAccount)") -}}
{{- range $name, $c := .Values.connectors -}}
{{- if and $c $c.enabled -}}
{{- $claims = append $claims (list (include "glovebox.connectorServiceAccountName" (dict "name" $name "connector" $c "root" $)) (printf "connectors.%s" $name)) -}}
{{- end -}}
{{- end -}}
{{- if .Values.schoologyConnector.enabled -}}
{{- $claims = append $claims (list (include "glovebox.connectorServiceAccountName" (dict "name" "schoology" "connector" .Values.schoologyConnector "root" .)) "schoologyConnector") -}}
{{- end -}}
{{- range $name, $imp := .Values.importers -}}
{{- if and $imp $imp.enabled -}}
{{- $claims = append $claims (list (include "glovebox.importerServiceAccountName" (dict "name" $name "importer" $imp "root" $)) (printf "importers.%s" $name)) -}}
{{- end -}}
{{- end -}}
{{- range $name, $b := .Values.loginBrokers -}}
{{- if and $b $b.enabled -}}
{{- $claims = append $claims (list (include "glovebox.loginBrokerName" (dict "name" $name "broker" $b "root" $)) (printf "loginBrokers.%s" $name)) -}}
{{- end -}}
{{- end -}}
{{- if .Values.powerschoolAuthRefresher.enabled -}}
{{- $claims = append $claims (list (printf "%s-powerschool-auth-refresher" (include "glovebox.fullname" .)) "powerschoolAuthRefresher") -}}
{{- end -}}
{{- if .Values.schoologyAuthRefresher.enabled -}}
{{- $claims = append $claims (list (printf "%s-schoology-auth-refresher" (include "glovebox.fullname" .)) "schoologyAuthRefresher") -}}
{{- end -}}
{{- range $claim := $claims -}}
{{- $sa := index $claim 0 -}}
{{- $owner := index $claim 1 -}}
{{- if hasKey $seen $sa -}}
{{- fail (printf "ServiceAccount %q would be used by both %s and %s. Every workload runs as an account of its own: the account is its identity to RBAC, to the login broker and to the record journal, so sharing one hands each workload the other's rights. Give one of them a different serviceAccount.name" $sa (index $seen $sa) $owner) -}}
{{- end -}}
{{- $_ := set $seen $sa $owner -}}
{{- end -}}
{{- end }}

{{/*
One ServiceAccount per importer Job, for the same reason: an importer is a
producer of records with an identity of its own, not the core's. Same
(dict "name" "importer" "root") shape and the same `serviceAccount` map,
default name <fullname>-importer-<name>.
*/}}
{{- define "glovebox.importerServiceAccountName" -}}
{{- $sa := (.importer | default dict).serviceAccount | default dict -}}
{{- if $sa.name -}}
{{- $sa.name -}}
{{- else -}}
{{- printf "%s-importer-%s" (include "glovebox.fullname" .root) .name -}}
{{- end -}}
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
{{- $where := .where | default (printf "powerschoolAuthRefresher.schedules.%s" .name) -}}
{{- $zone := "America/Los_Angeles" -}}
{{- if ne (toString $r.blackout.timeZone) $zone -}}
  {{- fail (printf "powerschoolAuthRefresher.blackout.timeZone is fixed at %s (got %q): the login blackout may be widened, never moved to another zone" $zone (toString $r.blackout.timeZone)) -}}
{{- end -}}
{{- if ne (toString $r.timeZone) $zone -}}
  {{- fail (printf "powerschoolAuthRefresher.timeZone (%q) must equal powerschoolAuthRefresher.blackout.timeZone (%q): a schedule can only be checked against the login blackout when both are in the same zone" (toString $r.timeZone) $zone) -}}
{{- end -}}
{{- $fields := regexSplit "\\s+" (trim $schedule) -1 -}}
{{- if ne (len $fields) 5 -}}
  {{- fail (printf "%s.schedule %q must be a five-field cron expression (no @macros): the chart has to check it against the login blackout" $where $schedule) -}}
{{- end -}}
{{- /* The floor, in minutes since midnight: 16:00 and 21:00. */ -}}
{{- $floorStart := 960 -}}
{{- $floorEnd := 1260 -}}
{{- $start := int (include "glovebox.clockMinutes" (dict "value" $r.blackout.start "what" "powerschoolAuthRefresher.blackout.start")) -}}
{{- $end := int (include "glovebox.clockMinutes" (dict "value" $r.blackout.end "what" "powerschoolAuthRefresher.blackout.end")) -}}
{{- if eq $start $end -}}
  {{- fail "powerschoolAuthRefresher.blackout.start and .end are equal: the window cannot be empty" -}}
{{- end -}}
{{- $covers := false -}}
{{- if lt $start $end -}}
  {{- $covers = and (le $start $floorStart) (ge $end $floorEnd) -}}
{{- else -}}
  {{- $covers = or (le $start $floorStart) (ge $end $floorEnd) -}}
{{- end -}}
{{- if not $covers -}}
  {{- fail (printf "powerschoolAuthRefresher.blackout %s-%s does not contain 16:00-21:00: the login blackout may only be widened (an earlier start, a later end, extraWindows), never shortened or moved. The binary enforces 16:00-21:00 %s whatever is configured" (toString $r.blackout.start) (toString $r.blackout.end) $zone) -}}
{{- end -}}
{{- $windows := list (dict "s" $floorStart "e" $floorEnd "label" "16:00-21:00") -}}
{{- $windows = append $windows (dict "s" $start "e" $end "label" (printf "%s-%s" (toString $r.blackout.start) (toString $r.blackout.end))) -}}
{{- range $i, $w := ($r.blackout.extraWindows | default list) -}}
  {{- $ws := int (include "glovebox.clockMinutes" (dict "value" $w.start "what" (printf "powerschoolAuthRefresher.blackout.extraWindows[%d].start" $i))) -}}
  {{- $we := int (include "glovebox.clockMinutes" (dict "value" $w.end "what" (printf "powerschoolAuthRefresher.blackout.extraWindows[%d].end" $i))) -}}
  {{- if eq $ws $we -}}
    {{- fail (printf "powerschoolAuthRefresher.blackout.extraWindows[%d]: start and end are equal: a window cannot be empty" $i) -}}
  {{- end -}}
  {{- $windows = append $windows (dict "s" $ws "e" $we "label" (printf "%s-%s" (toString $w.start) (toString $w.end))) -}}
{{- end -}}
{{- $deadline := int (.deadline | default $r.startingDeadlineSeconds | default 0) -}}
{{- if lt $deadline 10 -}}
  {{- fail (printf "the startingDeadlineSeconds that applies to %s must be set (10 or more): without it a missed run can start at any later time, including inside the login blackout" $where) -}}
{{- end -}}
{{- /*
A run may start up to the deadline late, and the binary refuses a login that
could still be in progress when a window begins: its login timeout (90s) plus
a margin (60s), 150s, rounded up here to 3 minutes.
*/ -}}
{{- $headroom := 3 -}}
{{- $late := int (add (div (add $deadline 59) 60) $headroom) -}}
{{- $minutes := splitList "," (include "glovebox.cronFieldValues" (dict "field" (index $fields 0) "lo" 0 "hi" 59 "what" (printf "%s.schedule minute field" $where))) -}}
{{- $hours := splitList "," (include "glovebox.cronFieldValues" (dict "field" (index $fields 1) "lo" 0 "hi" 23 "what" (printf "%s.schedule hour field" $where))) -}}
{{- range $h := $hours -}}
  {{- range $m := $minutes -}}
    {{- $t := int (add (mul (atoi $h) 60) (atoi $m)) -}}
    {{- range $w := $windows -}}
      {{- $ws := int $w.s -}}
      {{- $we := int $w.e -}}
      {{- $inside := false -}}
      {{- if lt $ws $we -}}
        {{- $inside = and (ge $t $ws) (lt $t $we) -}}
      {{- else -}}
        {{- $inside = or (ge $t $ws) (lt $t $we) -}}
      {{- end -}}
      {{- if $inside -}}
        {{- fail (printf "%s.schedule %q fires at %02d:%02d, inside the login blackout %s %s: a login then signs the account's owner out of the portal. Move the schedule outside the window" $where $schedule (atoi $h) (atoi $m) $w.label $zone) -}}
      {{- end -}}
      {{- $untilStart := int (mod (add (sub $ws $t) 1440) 1440) -}}
      {{- if le $untilStart $late -}}
        {{- fail (printf "%s.schedule %q fires at %02d:%02d, %d minute(s) before the login blackout %s %s; a missed run may start up to startingDeadlineSeconds (%d) late and a login needs %d minutes of clear time, %d minutes in all. Move the schedule earlier or shorten the deadline" $where $schedule (atoi $h) (atoi $m) $untilStart $w.label $zone $deadline $headroom $late) -}}
      {{- end -}}
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

{{/*
Login brokers (loginBrokers.<name>): one always-on login service per site.
These helpers take (dict "name" <key> "broker" <entry> "root" $).
*/}}

{{/*
Name of a broker's Deployment, Service, ServiceAccount and policies.
*/}}
{{- define "glovebox.loginBrokerName" -}}
{{- $n := printf "%s-login-broker-%s" (include "glovebox.fullname" .root) .name -}}
{{- if gt (len $n) 63 -}}
{{- fail (printf "login broker name %q is %d characters; the limit is 63. Shorten the key loginBrokers.%s or set fullnameOverride" $n (len $n) .name) -}}
{{- end -}}
{{- $n -}}
{{- end }}

{{/*
The audience callers' tokens must be minted for. Each instance has its own,
and the default includes the namespace: a token minted for one broker is
refused by every other, in this release or any other.
*/}}
{{- define "glovebox.loginBrokerAudience" -}}
{{- .broker.audience | default (printf "%s.%s" (include "glovebox.loginBrokerName" .) .root.Release.Namespace) -}}
{{- end }}

{{/*
Selector labels of a broker's pods.
*/}}
{{- define "glovebox.loginBrokerSelectorLabels" -}}
app.kubernetes.io/name: {{ include "glovebox.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: login-broker
glovebox.io/login-broker: {{ .name }}
{{- end }}

{{/*
Name of the Secret holding a broker's account credentials: the operator's
existing one, or the one the chart's ExternalSecret produces.
*/}}
{{- define "glovebox.loginBrokerCredentialsSecret" -}}
{{- $c := .broker.credentials | default dict -}}
{{- if $c.existingSecret -}}
{{- $c.existingSecret -}}
{{- else -}}
{{- include "glovebox.loginBrokerName" . }}-credentials
{{- end -}}
{{- end }}

{{/*
Render-time checks of one enabled broker. Included by the Deployment
template, so they run exactly once per broker.
*/}}
{{- define "glovebox.loginBrokerCheck" -}}
{{- $b := .broker -}}
{{- $where := printf "loginBrokers.%s" .name -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]*[a-z0-9])?$" .name) -}}
{{- fail (printf "%s: the key must be a lower-case DNS label (it becomes part of object names and of the token audience)" $where) -}}
{{- end -}}
{{- if not $b.site -}}
{{- fail (printf "%s.site is required: it selects the site driver (for example powerschool)" $where) -}}
{{- end -}}
{{- if not (hasPrefix "https://" (lower (toString $b.vault.addr))) -}}
{{- fail (printf "%s.vault.addr must be an https URL: the broker sends the session and its ServiceAccount token to Vault and refuses a plaintext address" $where) -}}
{{- end -}}
{{- if not $b.vault.k8sRole -}}
{{- fail (printf "%s.vault.k8sRole is required: each broker has a Vault role of its own, bound to its own ServiceAccount, so that one broker's token cannot read another site's credentials" $where) -}}
{{- end -}}
{{- if not $b.vault.sessionPath -}}
{{- fail (printf "%s.vault.sessionPath is required" $where) -}}
{{- end -}}
{{- if and $b.vault.statePath (eq (trimAll "/" (toString $b.vault.statePath)) (trimAll "/" (toString $b.vault.sessionPath))) -}}
{{- fail (printf "%s.vault.statePath must not be the session path: the session secret is what ESO copies to the connector" $where) -}}
{{- end -}}
{{- $blackout := $b.blackout | default dict -}}
{{- if and $blackout.windows (not $blackout.timeZone) -}}
{{- fail (printf "%s.blackout.timeZone is required with blackout.windows: a window means nothing without a zone" $where) -}}
{{- end -}}
{{- $cred := ($b.credentials | default dict).env | default dict -}}
{{- if or (not (hasKey $cred "LOGIN_BROKER_USERNAME")) (not (hasKey $cred "LOGIN_BROKER_PASSWORD")) -}}
{{- fail (printf "%s.credentials.env must map LOGIN_BROKER_USERNAME and LOGIN_BROKER_PASSWORD to keys of the credentials Secret" $where) -}}
{{- end -}}
{{- $budget := int ($b.dailyLoginBudget | default 6) -}}
{{- if or (lt $budget 1) (gt $budget 6) -}}
{{- fail (printf "%s.dailyLoginBudget must be between 1 and 6: six logins a day is compiled into the broker as the ceiling, and this value can only lower it" $where) -}}
{{- end -}}
{{- /*
The broker speaks plain HTTP. Without a NetworkPolicy every pod in the
cluster can reach its port, and the token check is the only lock. That is
not rendered by accident: not by the chart-wide switch, not by the
instance's own.
*/ -}}
{{- $np := $b.networkPolicy | default dict -}}
{{- if and (not (and .root.Values.networkPolicy.enabled $np.enabled)) (not $b.acknowledgeNoNetworkPolicy) -}}
{{- fail (printf "%s would be rendered WITHOUT its NetworkPolicy (networkPolicy.enabled or %s.networkPolicy.enabled is false). The broker speaks plain HTTP inside the cluster and holds an account's password; the policy that admits only its connector's pods is what makes that acceptable. If an equivalent policy is provided outside this chart, say so with %s.acknowledgeNoNetworkPolicy=true" $where $where $where) -}}
{{- end -}}
{{- /*
Two brokers must never share an account's record or a Vault role: the first
would make them log in over each other, the second would let one site's
broker read another site's password.
*/ -}}
{{- range $other, $ob := .root.Values.loginBrokers -}}
{{- if and $ob $ob.enabled (ne $other $.name) -}}
{{- if eq (trimAll "/" (toString $ob.vault.sessionPath)) (trimAll "/" (toString $b.vault.sessionPath)) -}}
{{- fail (printf "loginBrokers.%s and loginBrokers.%s write the same vault.sessionPath: two brokers for one account would log in over each other" $.name $other) -}}
{{- end -}}
{{- if eq (toString $ob.vault.k8sRole) (toString $b.vault.k8sRole) -}}
{{- fail (printf "loginBrokers.%s and loginBrokers.%s use the same vault.k8sRole: each broker needs a Vault role of its own, or one compromise exposes both sites' credentials" $.name $other) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /*
The request mailbox (powerschoolAuthRefresher.onRequest) and a broker for
the same session are two login workloads answering the same dead session.
*/ -}}
{{- $r := .root.Values.powerschoolAuthRefresher -}}
{{- if and $r.enabled ($r.onRequest | default dict).enabled (eq (trimAll "/" (toString $r.vault.sessionPath)) (trimAll "/" (toString $b.vault.sessionPath))) -}}
{{- fail (printf "%s and powerschoolAuthRefresher.onRequest are both enabled for the session at %q: choose one way to log in on demand" $where (toString $b.vault.sessionPath)) -}}
{{- end -}}
{{- /*
Scheduled logins beside a broker for the same account are a second writer
on a clock: every one of them ends the session the broker made, and spends
one of the day's logins. The two do coordinate through the shared record
(neither logs in while the other's login is in flight), but that makes it
safe, not sensible. Suspended CronJobs are fine: they are templates for a
manual run.
*/ -}}
{{- if and $r.enabled (eq (trimAll "/" (toString $r.vault.sessionPath)) (trimAll "/" (toString $b.vault.sessionPath))) (not $b.acknowledgeScheduledLogins) -}}
{{- range $sname, $entry := $r.schedules -}}
{{- if and $entry (not (or $r.suspendSchedules $entry.suspend)) -}}
{{- fail (printf "%s is enabled and powerschoolAuthRefresher.schedules.%s is a live schedule for the same session (%q): a scheduled login ends the session the broker made and spends one of the day's logins. Suspend the schedules (powerschoolAuthRefresher.suspendSchedules=true keeps them as templates for a manual run), remove them, or, if both are really wanted, set %s.acknowledgeScheduledLogins=true" $where $sname (toString $b.vault.sessionPath) $where) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}


{{/*
Record journal (journal.*). Every check the chart can make before the core
starts, in one place, so a bad value fails the render with a message that
names the setting instead of a pod that crash-loops. The core validates the
rendered block again (internal/config/journal.go). Included only with
journal.enabled.
*/}}
{{- define "glovebox.journalChecks" -}}
{{- $j := .Values.journal -}}
{{- if not $j.roster.configMap -}}
{{- fail "journal.enabled needs journal.roster.configMap: the journal computes every reader's grants from the household roster and has nothing to serve without it" -}}
{{- end -}}
{{- if not (regexMatch "^[A-Za-z0-9_][A-Za-z0-9._-]*$" (toString $j.roster.key)) -}}
{{- fail "journal.roster.key must be a plain ConfigMap key (letters, digits, '.', '_', '-'): it is also the file name under the read-only roster mount" -}}
{{- end -}}
{{- if or (not $j.database.host) (not (include "glovebox.journalDBSecret" .)) -}}
{{- fail "journal.enabled needs journal.database.host and journal.database.existingSecret, or journal.database.externalSecret.enabled with a Vault path (a basic-auth Secret synced by ESO; see docs/upgrading.md, \"Record journal\")" -}}
{{- end -}}
{{- if and (not $j.database.existingSecret) $j.database.externalSecret.enabled (not $j.database.externalSecret.path) -}}
{{- fail "journal.database.externalSecret.enabled needs journal.database.externalSecret.path, the Vault KV path of the journal's database credentials" -}}
{{- end -}}
{{- if not (.Values.podSecurityContext | default dict).fsGroup -}}
{{- fail "journal.enabled needs podSecurityContext.fsGroup: the database credentials are mounted with defaultMode 0400, and only the fsGroup (which kubelet applies as group ownership plus group read, 0440 in effect) makes them readable to the non-root process" -}}
{{- end -}}
{{- if ne (toString $j.database.sslmode) "require" -}}
{{- fail "journal.database.sslmode must be \"require\": plaintext is refused, and verify-ca / verify-full need the CNPG CA certificate mounted, which this chart does not do yet (glovebox-wekc.18.2)" -}}
{{- end -}}
{{- if not (toString $j.scannerVersion | trim) -}}
{{- fail "journal.scannerVersion must not be empty: it labels every scan verdict, and an empty one means \"judged by no scanner\"" -}}
{{- end -}}
{{- range $j.staleness -}}
{{- if not (regexMatch "^source=[^=/]+/(surface=[^=/]+/)?$" (toString .prefix)) -}}
{{- fail (printf "journal.staleness: prefix %q must be source=<value>/ or source=<value>/surface=<value>/ (it becomes a metric label, so nothing finer than a surface is allowed)" (toString .prefix)) -}}
{{- end -}}
{{- if le (int .maxAgeHours) 0 -}}
{{- fail (printf "journal.staleness: prefix %q needs maxAgeHours > 0" (toString .prefix)) -}}
{{- end -}}
{{- end -}}
{{- if and ($j.networkPolicy | default dict).enabled (not .Values.networkPolicy.enabled) -}}
{{- fail "journal.networkPolicy.enabled needs networkPolicy.enabled: the journal's policy only adds to the core's (which isolates the pod and allows DNS); without it the journal port would be open to every pod. Set journal.networkPolicy.enabled=false only if the journal's policy is managed outside this chart" -}}
{{- end -}}
{{- /*
  The Go validator's ranges (internal/config/journal.go, validate), so that
  values that render are values the core accepts.
*/ -}}
{{- $port := int $j.port -}}
{{- if or (lt $port 1) (gt $port 65535) -}}
{{- fail (printf "journal.port must be 1-65535, got %v" $j.port) -}}
{{- end -}}
{{- $inUse := dict -}}
{{- if .Values.metrics.port -}}
{{- $_ := set $inUse (toString (int .Values.metrics.port)) "metrics.port" -}}
{{- end -}}
{{- $_ := set $inUse (toString (int .Values.config.metricsPort)) "config.metricsPort" -}}
{{- if .Values.config.ingest.enabled -}}
{{- $_ := set $inUse (toString (int .Values.config.ingest.port)) "config.ingest.port" -}}
{{- if include "glovebox.bearerPortSplit" . -}}
{{- $_ := set $inUse (include "glovebox.bearerPort" .) "config.ingest.bearerPort" -}}
{{- end -}}
{{- if ne (toString .Values.ingest.tls.mode) "disabled" -}}
{{- $tls := int .Values.ingest.tls.port -}}
{{- if eq $tls 0 -}}{{- $tls = add1 (int .Values.config.ingest.port) -}}{{- end -}}
{{- $_ := set $inUse (toString $tls) "ingest.tls.port" -}}
{{- end -}}
{{- end -}}
{{- with index $inUse (toString $port) -}}
{{- fail (printf "journal.port %d is already used by %s" $port .) -}}
{{- end -}}
{{- if not (and (ge (int $j.requestTimeoutSeconds) 1) (le (int $j.requestTimeoutSeconds) 600)) -}}
{{- fail (printf "journal.requestTimeoutSeconds must be 1-600, got %v" $j.requestTimeoutSeconds) -}}
{{- end -}}
{{- if not (and (ge (int $j.scanTimeoutSeconds) 1) (le (int $j.scanTimeoutSeconds) 300)) -}}
{{- fail (printf "journal.scanTimeoutSeconds must be 1-300, got %v" $j.scanTimeoutSeconds) -}}
{{- end -}}
{{- if ne (toString $j.roster.format) "openclaw-household-v1" -}}
{{- fail (printf "journal.roster.format must be openclaw-household-v1 (the only roster format), got %q" (toString $j.roster.format)) -}}
{{- end -}}
{{- if not (and (ge (int $j.roster.reloadSeconds) 1) (le (int $j.roster.reloadSeconds) 300)) -}}
{{- fail (printf "journal.roster.reloadSeconds must be 1-300, got %v" $j.roster.reloadSeconds) -}}
{{- end -}}
{{- $mf := float64 $j.removalGuard.maxFraction -}}
{{- if not (and (gt $mf 0.0) (lt $mf 1.0)) -}}
{{- fail (printf "journal.removalGuard.maxFraction must be strictly between 0 and 1, got %v: 1 or more would let a snapshot remove every record unheld, 0 or less would hold every removal" $j.removalGuard.maxFraction) -}}
{{- end -}}
{{- if not (and (ge (int $j.removalGuard.minKeys) 0) (le (int $j.removalGuard.minKeys) 1000)) -}}
{{- fail (printf "journal.removalGuard.minKeys must be 0-1000, got %v" $j.removalGuard.minKeys) -}}
{{- end -}}
{{- if not (toString $j.database.name) -}}
{{- fail "journal.database.name must not be empty" -}}
{{- end -}}
{{- if regexMatch "[,/?]" (toString $j.database.host) -}}
{{- fail "journal.database.host must be a single host name (no ',', '/' or '?')" -}}
{{- end -}}
{{- if not (and (ge (int $j.database.port) 1) (le (int $j.database.port) 65535)) -}}
{{- fail (printf "journal.database.port must be 1-65535, got %v" $j.database.port) -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $j.staleness -}}
{{- if hasKey $seen (toString .prefix) -}}
{{- fail (printf "journal.staleness: prefix %q is listed twice" (toString .prefix)) -}}
{{- end -}}
{{- $_ := set $seen (toString .prefix) true -}}
{{- end -}}
{{- /* Identity cross-checks. */ -}}
{{- $core := include "glovebox.serviceAccountName" . -}}
{{- if eq $core "default" -}}
{{- fail "journal.enabled refuses serviceAccount.name=default: the core's account gets the TokenReview grant and a mounted token, and the namespace's default account is the one every pod without an account of its own runs as" -}}
{{- end -}}
{{- $services := $j.services | default dict -}}
{{- if not $services -}}
{{- fail "journal.enabled needs journal.services to name at least one caller: an empty policy leaves TokenReview an empty allow-list and every request is refused" -}}
{{- end -}}
{{- $coreKey := printf "%s/%s" .Release.Namespace $core -}}
{{- if hasKey $services $coreKey -}}
{{- fail (printf "journal.services must not list the core's own ServiceAccount %q: the core serves the journal and needs no rights in it" $coreKey) -}}
{{- end -}}
{{- range (($j.networkPolicy | default dict).writerConnectors | default list) -}}
{{- $key := printf "%s/%s" $.Release.Namespace (include "glovebox.connectorServiceAccountByKey" (dict "name" (toString .) "root" $)) -}}
{{- $entry := index $services $key | default dict -}}
{{- if not $entry.write_types -}}
{{- fail (printf "journal.networkPolicy.writerConnectors names %q, but journal.services gives its ServiceAccount %q no write_types: only connectors with write rights may reach the journal port" (toString .) $key) -}}
{{- end -}}
{{- end -}}
{{- /*
  Every write_type must be a record type the core loads: an unknown one is
  a policy error at startup (policy.New), and the core exits -- item
  processing included. With connector records on this usually means
  journal.connectorRecordTypes is missing the connector.
*/ -}}
{{- $typeNames := list -}}
{{- range (include "glovebox.journalRecordTypes" . | fromJsonArray) -}}
{{- $typeNames = append $typeNames (toString .name) -}}
{{- end -}}
{{- range $key := keys $services | sortAlpha -}}
{{- range ((index $services $key) | default dict).write_types | default list -}}
{{- if not (has (toString .) $typeNames) -}}
{{- fail (printf "journal.services %q may write record type %q, which is not in the journal's record_types (journal.recordTypes, or a connector in journal.connectorRecordTypes such as powerschool): the core would refuse its policy and exit at startup" $key (toString .)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $fn := include "glovebox.fullname" . -}}
{{- $cm := toString $j.roster.configMap -}}
{{- if or (eq $cm $fn) (hasPrefix (printf "%s-" $fn) $cm) -}}
{{- fail (printf "journal.roster.configMap %q is a name this chart uses for its own ConfigMaps (%s-*), some of which a workload may write (the PowerSchool login-request ConfigMap). The roster must be a ConfigMap only gitops writes; name it outside the chart's prefix" $cm $fn) -}}
{{- end -}}
{{- end }}

{{/*
"true" when the argument is a CIDR naming exactly one address: a dotted
IPv4 address with every octet 0-255 and /32, or an IPv6 address (at most one
"::", 1-4 hex digits per group, 8 groups without "::" and at most 7 with
it) and /128. "" otherwise.
*/}}
{{- define "glovebox.singleAddressCIDR" -}}
{{- $parts := splitList "/" (toString .) -}}
{{- if eq (len $parts) 2 -}}
{{- $a := index $parts 0 -}}
{{- $p := index $parts 1 -}}
{{- $octet := "(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])" -}}
{{- if eq $p "32" -}}
{{- if regexMatch (printf "^(%s\\.){3}%s$" $octet $octet) $a -}}true{{- end -}}
{{- else if and (eq $p "128") (regexMatch "^[0-9A-Fa-f:]+$" $a) (not (contains ":::" $a)) -}}
{{- $halves := splitList "::" $a -}}
{{- $ok := true -}}
{{- $groups := 0 -}}
{{- if gt (len $halves) 2 -}}{{- $ok = false -}}{{- end -}}
{{- range $halves -}}
{{- if . -}}
{{- range (splitList ":" .) -}}
{{- if not (regexMatch "^[0-9A-Fa-f]{1,4}$" .) -}}{{- $ok = false -}}{{- end -}}
{{- $groups = add1 $groups -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if eq (len $halves) 1 -}}
{{- if ne $groups 8 -}}{{- $ok = false -}}{{- end -}}
{{- else if gt $groups 7 -}}{{- $ok = false -}}
{{- end -}}
{{- if $ok -}}true{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The Secret holding the journal's database username and password: the one
named by journal.database.existingSecret, else the one the chart's
ExternalSecret creates, else nothing.
*/}}
{{- define "glovebox.journalDBSecret" -}}
{{- $d := .Values.journal.database -}}
{{- if $d.existingSecret -}}
{{- $d.existingSecret -}}
{{- else if ($d.externalSecret | default dict).enabled -}}
{{- printf "%s-journal-db" (include "glovebox.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
Name of the cluster-scoped TokenReview ClusterRole and its binding. Carries
the namespace, since two releases of the same name in two namespaces would
otherwise collide.
*/}}
{{- define "glovebox.journalTokenReviewName" -}}
{{- printf "%s-%s-journal-tokenreview" .Release.Namespace (include "glovebox.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
The journal's record_types, as JSON: journal.recordTypes, then the types of
each connector in journal.connectorRecordTypes, read from the chart's
journal-record-types/<name>.json (generated from the connector's Go
definitions; decision S14). A name without a file, a name listed twice, and
a type version defined twice (in recordTypes and a connector's file, or in
two files) fail the render. With connectorRecordTypes empty this is exactly
recordTypes, rendered as before.
*/}}
{{- define "glovebox.journalRecordTypes" -}}
{{- $j := .Values.journal -}}
{{- $types := $j.recordTypes | default list -}}
{{- $listed := dict -}}
{{- range ($j.connectorRecordTypes | default list) -}}
{{- $name := toString . -}}
{{- if hasKey $listed $name -}}
{{- fail (printf "journal.connectorRecordTypes lists %q twice" $name) -}}
{{- end -}}
{{- $_ := set $listed $name true -}}
{{- $file := "" -}}
{{- if regexMatch "^[a-z][a-z0-9-]*$" $name -}}
{{- $file = $.Files.Get (printf "journal-record-types/%s.json" $name) -}}
{{- end -}}
{{- if not $file -}}
{{- fail (printf "journal.connectorRecordTypes names %q, but the chart has no journal-record-types/%s.json: only connectors that write records ship their types (powerschool, schoology)" $name $name) -}}
{{- end -}}
{{- $types = concat $types ($file | fromJsonArray) -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $types -}}
{{- $v := printf "%s version %v" (toString .name) .schema_version -}}
{{- if hasKey $seen $v -}}
{{- fail (printf "journal record type %s is defined twice (journal.recordTypes and journal.connectorRecordTypes, or two connectors): drop one of them" $v) -}}
{{- end -}}
{{- $_ := set $seen $v true -}}
{{- end -}}
{{- $types | toJson -}}
{{- end }}

{{/*
"true" when a connector has records enabled (record journal Part B), ""
otherwise. Takes (dict "name" <key> "connector" <entry>) for a
connectors.<name> entry, plus "schoologyConnector" true for the Schoology
connector's own block (Part B2). Only the PowerSchool connector
(connectors.powerschool) and the Schoology connector (schoologyConnector)
build records; records on any other entry fail the render rather than
render a token and a block nothing reads.
*/}}
{{- define "glovebox.recordsEnabled" -}}
{{- $r := (.connector | default dict).records | default dict -}}
{{- if $r.enabled -}}
{{- if not (or (and .schoologyConnector (eq (toString .name) "schoology")) (and (not .schoologyConnector) (eq (toString .name) "powerschool"))) -}}
{{- fail (printf "connectors.%s.records.enabled: only the powerschool connector writes records to the journal (and the Schoology connector, under schoologyConnector.records); no other connector here has record builders" (toString .name)) -}}
{{- end -}}
true
{{- end -}}
{{- end }}

{{/*
The records block of a records-enabled connector's config.json, as JSON
(records.Config, plus the PowerSchool settle time and carry horizon for
any connector but the Schoology one). Takes (dict "connector" <entry> "root" $, and "name"
"schoology" for the Schoology connector). A zero or missing value takes its
default here, as the connector would, so the rendered block shows what
runs. glovebox.recordsChecks bounds every value first.
*/}}
{{- define "glovebox.recordsConfig" -}}
{{- $r := .connector.records | default dict -}}
{{- $spool := $r.spool | default dict -}}
{{- $block := dict
  "enabled" true
  "journal_url" (printf "http://%s-journal:%d" (include "glovebox.fullname" .root) (int .root.Values.journal.port))
  "token_file" "/var/run/secrets/glovebox-journal/token"
  "timeout_seconds" (int ($r.timeoutSeconds | default 90))
  "spool" (dict
    "max_age_hours" (int ($spool.maxAgeHours | default 168))
    "max_bytes" (int64 ($spool.maxBytes | default 268435456))) -}}
{{- if ne (toString .name) "schoology" -}}
{{- $_ := set $block "attendance_day_settle_school_days" (int ($r.attendanceDaySettleSchoolDays | default 3)) -}}
{{- $_ := set $block "attendance_day_carry_max_days" (int ($r.attendanceDayCarryMaxDays | default 120)) -}}
{{- end -}}
{{- $block | toJson -}}
{{- end }}

{{/*
Refuse a records-enabled connector that could not write, or should not
(decisions S12, S13 of the Part B1 plan). Takes (dict "name" <key>
"connector" <entry> "root" $). Renders nothing; included from the
connector's ConfigMap when glovebox.recordsEnabled.

Every grant is an explicit value reviewed in gitops: the chart adds nothing
to journal.services or writerConnectors by itself. journalChecks already
refuses a writer connector without write_types; this is the reverse check.
The ranges are the connector's own (connector/records Config.Validate and
the PowerSchool settle time and carry horizon), so values that render are
values it accepts.
*/}}
{{- define "glovebox.recordsChecks" -}}
{{- $root := .root -}}
{{- $j := $root.Values.journal | default dict -}}
{{- $r := .connector.records | default dict -}}
{{- $sc := and .schoologyConnector (eq (toString .name) "schoology") -}}
{{- $p := printf "connectors.%s.records" (toString .name) -}}
{{- if $sc -}}{{- $p = "schoologyConnector.records" -}}{{- end -}}
{{- if not $j.enabled -}}
{{- fail (printf "%s.enabled needs journal.enabled: the connector would spool snapshots for a journal that does not exist" $p) -}}
{{- end -}}
{{- /*
  Decision T6 (Part B2): the spool is on the connector's state volume, and
  the Schoology connector's state is an emptyDir unless made persistent. A
  spool a pod restart wipes is not durable.
*/ -}}
{{- if $sc -}}
{{- $state := .connector.state | default dict -}}
{{- if not (or $state.persistent $state.existingClaim) -}}
{{- fail (printf "%s.enabled needs a durable state volume: set schoologyConnector.state.persistent (or state.existingClaim). The spool lives under /state/spool, and with the default emptyDir a pod restart would lose every snapshot not yet sent" $p) -}}
{{- end -}}
{{- end -}}
{{- if (.connector.loginOnDemand | default dict).enabled -}}
{{- fail (printf "%s.enabled is refused together with connectors.%s.loginOnDemand.enabled: that mode mounts the connector's Kubernetes API token, and a connector with journal rights holds only a token projected for the journal (spec 4.9c). Use the login broker (loginBroker) instead" $p (toString .name)) -}}
{{- end -}}
{{- $key := printf "%s/%s" $root.Release.Namespace (include "glovebox.connectorServiceAccountName" (dict "name" .name "connector" .connector "root" $root)) -}}
{{- $entry := index ($j.services | default dict) $key | default dict -}}
{{- if not $entry.write_types -}}
{{- fail (printf "%s.enabled, but journal.services gives the connector's ServiceAccount %q no write_types: every snapshot would be refused. Add the entry (write_types, write_scopes, write_subjects: roster_subjects; docs/upgrading.md)" $p $key) -}}
{{- end -}}
{{- if not (has (toString .name) (($j.networkPolicy | default dict).writerConnectors | default list)) -}}
{{- fail (printf "%s.enabled needs %q in journal.networkPolicy.writerConnectors: without it the journal's NetworkPolicy does not admit the connector's pod" $p (toString .name)) -}}
{{- end -}}
{{- $exp := int ($r.tokenExpirationSeconds | default 600) -}}
{{- if or (lt $exp 600) (gt $exp 3600) -}}
{{- fail (printf "%s.tokenExpirationSeconds must be 600-3600 (600 is the kubelet's minimum), got %v" $p $r.tokenExpirationSeconds) -}}
{{- end -}}
{{- $timeout := int ($r.timeoutSeconds | default 90) -}}
{{- if or (lt $timeout 10) (gt $timeout 600) -}}
{{- fail (printf "%s.timeoutSeconds must be 10-600, got %v" $p $r.timeoutSeconds) -}}
{{- end -}}
{{- if not $sc -}}
{{- $settle := int ($r.attendanceDaySettleSchoolDays | default 3) -}}
{{- if or (lt $settle 1) (gt $settle 10) -}}
{{- fail (printf "%s.attendanceDaySettleSchoolDays must be 1-10, got %v" $p $r.attendanceDaySettleSchoolDays) -}}
{{- end -}}
{{- $carry := int ($r.attendanceDayCarryMaxDays | default 120) -}}
{{- if or (lt $carry 30) (gt $carry 200) -}}
{{- fail (printf "%s.attendanceDayCarryMaxDays must be 30-200, got %v" $p $r.attendanceDayCarryMaxDays) -}}
{{- end -}}
{{- end -}}
{{- $spool := $r.spool | default dict -}}
{{- $age := int ($spool.maxAgeHours | default 168) -}}
{{- if or (lt $age 1) (gt $age 2160) -}}
{{- fail (printf "%s.spool.maxAgeHours must be 1-2160, got %v" $p $spool.maxAgeHours) -}}
{{- end -}}
{{- $bytes := int64 ($spool.maxBytes | default 268435456) -}}
{{- if or (lt $bytes 1048576) (gt $bytes 4294967296) -}}
{{- fail (printf "%s.spool.maxBytes must be 1048576-4294967296 (1 MiB - 4 GiB), got %v" $p $spool.maxBytes) -}}
{{- end -}}
{{- end }}
