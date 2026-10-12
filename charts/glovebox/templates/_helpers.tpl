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
