# The record journal and the household roster

The record journal decides who may read what from one file: the household
roster. Every reader's grants are computed from it on every request. A reader
cannot name its own grants, and no glovebox value can widen them. This document
covers who owns the roster, what glovebox reads from it, the contract that
keeps it immutable, and what glovebox needs in place before the journal is
enabled.

Every id below (`e_parent1`, `e_child1`, ...) is a placeholder.

- [1. Ownership](#1-ownership)
- [2. Format: openclaw-household-v1](#2-format-openclaw-household-v1)
- [3. The immutability contract](#3-the-immutability-contract)
- [4. Reloads](#4-reloads)
- [5. Membership and what callers see](#5-membership-and-what-callers-see)
- [6. Erase and the roster](#6-erase-and-the-roster)
- [7. Enabling checklist](#7-enabling-checklist)

---

## 1. Ownership

The roster is owned outside glovebox, by whoever owns the household and its
gateway (openclaw), and it changes only through GitOps. It is a ConfigMap,
`openclaw-household` with the key `household.json` in the reference
deployment, applied by Flux from the gitops repository. The openclaw gateway
reads the same object.

Glovebox only reads it:

- The chart mounts the ConfigMap **read-only** at `/etc/glovebox-roster`, as a
  whole volume (through `items`, never `subPath`), so the kubelet's updates
  reach the running process. ConfigMap volumes are read-only to the pod in
  any case.
- Glovebox holds **no RBAC on ConfigMaps** for it. The core's ServiceAccount
  has one grant: `create` on `tokenreviews`. Glovebox never reads the roster
  through the API and has no code path that writes it.
- The ConfigMap has to be in the release's own namespace, because a
  ConfigMap volume cannot cross namespaces. If the owner keeps the
  canonical copy elsewhere, gitops applies a copy into glovebox's namespace.
  Never give glovebox RBAC to read across namespaces.

Chart values (`journal.roster`):

| Value | Default | Meaning |
|---|---|---|
| `configMap` | `""` (required) | The roster ConfigMap, in this namespace. |
| `key` | `household.json` | Its key. The file appears as `/etc/glovebox-roster/<key>`. |
| `format` | `openclaw-household-v1` | The only format there is. |
| `reloadSeconds` | `10` | How often the file is checked, 1 to 300. |

## 2. Format: openclaw-household-v1

This is the file openclaw reads, `household.json`. The authoritative parser
is `internal/journal/roster` (`roster.go`), which mirrors openclaw's
`image/internal/household`.

```json
{
  "people": {
    "e_parent1": {"agent": "agent-parent1", "guardian_of": ["e_child1", "e_child2"]},
    "e_parent2": {"agent": "agent-parent2", "guardian_of": ["e_child1", "e_child2"]},
    "e_child1":  {"agent": "agent-child1",  "siblings": ["e_child2"]},
    "e_child2":  {"agent": "agent-child2",  "siblings": ["e_child1"]}
  },
  "household_members": ["e_parent1", "e_parent2", "e_child1", "e_child2"]
}
```

What glovebox reads:

| Field | Used for |
|---|---|
| `people` | The members. The keys are member ids. It must be present and non-empty. |
| `people.<id>.agent` | Not used for grants. It must be non-empty and unique: the parser refuses two members bound to one agent, as openclaw does. |
| `people.<id>.guardian_of` | Member ids this member is a guardian of. Every id must be defined in `people`. |
| `people.<id>.siblings` | Member ids this member is a sibling of. Every id must be defined in `people`. |
| `household_members` | Members of the household audience. Every id must be defined in `people`. |

Parsing rules:

- **Ids** match `[A-Za-z0-9._-]+`, and `.` and `..` are refused. An id never
  contains `:`. The journal relies on that: household note keys are
  `<date>:<author>`, and an erased member's kept notes live under subjects
  `erased:<pseudonym>:<n>`, which no roster grant can name.
- **Unknown keys are refused, anywhere in the document.** The decoder runs
  with `DisallowUnknownFields`, so a field that openclaw adds to
  `household.json` later makes glovebox refuse the whole file until
  glovebox learns it (see [4](#4-reloads)). Data after the document is
  refused too.
- Object keys match case-insensitively (`encoding/json`), and a person id
  repeated in `people` keeps the last entry. Both behaviours are the same as
  openclaw's.

Grants. A grant is a pair (subject, audience token). A row is visible to a
reader when one of the reader's grants has the row's `data_subject` as its
subject and a token in the row's audience. Rows whose audience includes
`public` are visible to every roster member. For a member `m`:

- `(m, subject)`: rows about themselves;
- `(c, guardians)` for every `c` in `m.guardian_of`, plus `("", guardians)`
  when `guardian_of` is non-empty (household-wide rows for guardians);
- `(s, siblings)` for every `s` in `m.siblings` (`m` is a sibling of `s`);
- if `m` is in `household_members`: `(x, household)` for every household
  member `x`, plus `("", household)`.

Nothing else grants access. `caregivers` is never produced, and no `subject`
or `siblings` grant is ever produced for the household subject `""`. In the
example above, `e_parent1` reads `e_child1`'s rows whose audience includes
`guardians` or `household`, and `e_child2` reads `e_child1`'s rows whose
audience includes `siblings` or `household`.

## 3. The immutability contract

**Only a GitOps-triggered deployment may change the roster.** This is a
precondition for enabling the journal, not an assumption the journal makes.
A change to the roster changes what every reader can see, so a change has to
be a reviewed commit. Four things together enforce it:

1. No ServiceAccount in the namespace can create, update, patch or delete
   the roster ConfigMap. Check every account with
   `kubectl auth can-i update configmap/<roster-configmap> -n <roster-namespace> --as=system:serviceaccount:<roster-namespace>:<sa>`.
   Any Role that grants ConfigMap writes must name its ConfigMaps in
   `resourceNames`.
2. Both applications mount it read-only.
3. Flux applies it from gitops. With `prune: true` and drift correction,
   Flux reverts a hand edit at its next reconcile.
4. A ValidatingAdmissionPolicy denies `CREATE`, `UPDATE` and `DELETE` of
   the ConfigMap to every requester except Flux's kustomize-controller.
   Without it, a cluster administrator could edit the roster outside gitops,
   and the edit would stand until Flux reverted it.

Example policy and binding (Kubernetes 1.30 or later,
`admissionregistration.k8s.io/v1`). Replace `<roster-namespace>` and
`<roster-configmap>`:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: household-roster-gitops-only
spec:
  # If the policy cannot be evaluated, refuse the change.
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: ["v1"]
        operations: ["CREATE", "UPDATE", "DELETE"]
        resources: ["configmaps"]
        resourceNames: ["<roster-configmap>"]
  validations:
    - expression: >-
        request.userInfo.username ==
        'system:serviceaccount:flux-system:kustomize-controller'
      reason: Forbidden
      message: >-
        The household roster can only be changed by a GitOps deployment
        (Flux kustomize-controller). Change it in the gitops repository.
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: household-roster-gitops-only
spec:
  policyName: household-roster-gitops-only
  validationActions: [Deny]
  matchResources:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: <roster-namespace>
```

Notes on the example:

- If the Flux Kustomization that applies the roster impersonates its own
  ServiceAccount (`spec.serviceAccountName`), allow that account's username
  instead of kustomize-controller.
- Deleting the namespace would also be refused for this ConfigMap, because
  the namespace controller is not Flux. Break-glass, for that case or any
  other, is removing the policy through gitops.
- Verify the policy with a server-side dry run as an administrator:
  `kubectl -n <roster-namespace> annotate configmap <roster-configmap> test=1 --dry-run=server`
  must be refused.

**Why the policy lives with the roster's owner, not in the glovebox chart.**
The policy protects data that glovebox consumes from everyone, glovebox's
own deployment included. If the chart rendered it, a values change to the
glovebox release, or a `helm uninstall`, could loosen or remove the guard on
the very input that decides glovebox's access control. The chart would also
need cluster-scoped rights over admission policies, which it has no other
use for. The openclaw gateway depends on the same guarantee, so the guard
belongs next to the roster, in the gitops repository that owns both.

## 4. Reloads

- The journal checks the file every `journal.roster.reloadSeconds` (1 to
  300; the config validator refuses anything outside that range). If the
  bytes are unchanged (same SHA-256), nothing is done.
- The kubelet delivers a changed ConfigMap to a mounted volume within
  roughly one to two minutes (its sync period plus its cache). A roster
  change therefore reaches the journal within about `reloadSeconds` plus
  two minutes. The gateway and glovebox can briefly hold different
  rosters; openclaw's own pre-write check fails safe in that window.
- A reload swaps the roster atomically. **A file that cannot be read or
  does not parse is ignored, and the previous roster stays in use**
  (`roster.Holder.Reload`). Grants do not follow the change: a member added
  in it cannot read yet, and a member removed in it can still read. This
  holds even when the ConfigMap is deleted: the journal keeps serving with
  the last roster it loaded until the process restarts.
- On a start (a new pod, a restart) there is no previous roster. Until the
  file loads, reads answer `503 unavailable` (`ErrNoRoster`) and snapshots
  answer `409 unknown_subject`. An invalid roster at start is therefore an
  outage of the journal, never a fallback to an older roster.
- Every failed reload counts in `glovebox_journal_roster_reload_errors_total`
  and raises `GloveboxJournalRosterReloadFailing`. The log repeats an error
  only when it changes.
- `glovebox_journal_roster_info{sha256="..."}` is 1, labelled with the
  SHA-256 of the roster file's bytes as loaded. openclaw logs the same hash
  of the file it read. Equal hashes mean both applications use the same
  roster. To compute it from the cluster:
  `kubectl -n <ns> get configmap <roster-configmap> -o jsonpath='{.data.household\.json}' | sha256sum`.
  Hash differences shorter than about ten minutes are expected during a
  rollout.

## 5. Membership and what callers see

- **A snapshot or note about a subject not on the roster** gets
  `409 unknown_subject`. That answer means retry: the connector keeps the
  snapshot in its spool and sends it again with backoff, so a member being
  added, or a reload in progress, loses nothing. Once the subject is on the
  roster, the waiting snapshots apply. An erased subject gets `410` instead
  (the ban is checked first; see [6](#6-erase-and-the-roster)).
- **A reader not on the roster** gets `403`. The same applies to a reader
  not in the calling service's `readers` (`journal.services`), and to an
  erased member, whether or not they are still listed.
- **A household note written on behalf of a non-member** is refused with
  `403 policy`.
- **Removing a member from the roster revokes their reads** as soon as the
  journal loads the new roster: within `reloadSeconds` plus the kubelet's
  propagation, about two minutes with the defaults. Their rows stay in the
  database. With no grants left on them, those rows are unreadable until
  the member returns or is erased. Snapshots about a removed member get
  `409` and wait in the spool. To discard what the spool holds and stop
  new data, erase the member.

## 6. Erase and the roster

Erase (`POST /v1/admin/erase`, for a ServiceAccount with `admin: true` in
`journal.services`) removes what the journal holds about one member. It is also a **ban**.

- **Erase works while the member is still listed** (and after they are
  gone). The roster in use at the start of the erase decides who keeps
  reading the member's kept notes.
- **The ban.** From the commit on, every snapshot about the member and every
  note about or by them is refused with `410 erased_subject`, whatever its
  time. The member cannot read either (`403`), even while still on the
  roster. Nothing un-erases.
- **Remove them from the roster afterwards, through gitops, at any time.**
  The order does not matter to the journal. Until the removal, the ban is
  what keeps them out.
- **Kept notes.** A household note someone else wrote about the member is
  kept if anyone could read it at erase time. Its readers are frozen: the
  roster members who could read it at that moment, plus its author if on
  the roster, never the erased member. Those readers keep exactly that
  access. Later roster changes do not add readers. A frozen reader still
  has to be on the roster to read anything at all, so removing them from
  the roster ends their access to kept notes too.
- **Never reuse a roster id for a different person.** The ban and the
  frozen-reader lists are keyed by member id. A new person given an erased
  person's id would be banned from the start. A new person given the id of
  a frozen reader would inherit that reader's access to kept notes about
  someone else. Give every person a new id.

## 7. Enabling checklist

The chart side is `journal.*` in `charts/glovebox/values.yaml`, and
[upgrading.md, "Record journal"](upgrading.md#record-journal) lists what it
renders. Before setting `journal.enabled: true`, all five of these must be in
place.

### 7.1 The roster admission policy

The ValidatingAdmissionPolicy and binding from [section 3](#3-the-immutability-contract),
in the roster owner's gitops, with the four checks there passing.

### 7.2 The CNPG database and its role

The database is owned by a non-superuser login role. The journal runs its
own schema migrations as that role.

```yaml
# In the CNPG Cluster's spec (managed roles):
spec:
  managed:
    roles:
      - name: glovebox_journal
        ensure: present
        login: true
        superuser: false
        createdb: false
        createrole: false
        passwordSecret:
          name: glovebox-journal-db-role   # synced from the same Vault path as below
---
apiVersion: postgresql.cnpg.io/v1
kind: Database
metadata:
  name: glovebox-journal
  namespace: <database-namespace>
spec:
  cluster:
    name: <cnpg-cluster>
  name: glovebox_journal
  owner: glovebox_journal
```

The chart connects with `sslmode=require` to `journal.database.host`, for
example `<cnpg-cluster>-rw.<database-namespace>.svc.cluster.local`.

### 7.3 Vault path, policy and the ExternalSecret values

Store the role's credentials once in Vault, for example at
`secret/glovebox/journal-db` with the properties `username` and `password`.
The ESO store that syncs it must be allowed to read that path. Add the
policy in the platform repository, with the rest of the Vault
configuration:

```hcl
path "secret/data/glovebox/journal-db" {
  capabilities = ["read"]
}
```

Then, in the glovebox HelmRelease:

```yaml
journal:
  database:
    host: <cnpg-cluster>-rw.<database-namespace>.svc.cluster.local
    externalSecret:
      enabled: true
      path: glovebox/journal-db
      secretStoreRef:
        kind: ClusterSecretStore
        name: <store>
```

Alternatively, sync a `kubernetes.io/basic-auth` Secret yourself and set
`journal.database.existingSecret`. Only the `username` and `password` keys
are mounted, mode 0400. The CNPG role's `passwordSecret` and glovebox's
Secret come from the same Vault entry, so a rotation updates both.

### 7.4 The database's NetworkPolicy admits the core on 5432

The chart's own policy allows the core's egress to the database pods. If
the database namespace restricts ingress, admit the glovebox core there:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-glovebox-journal
  namespace: <database-namespace>
spec:
  podSelector:
    matchLabels:
      cnpg.io/cluster: <cnpg-cluster>
  policyTypes: [Ingress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: <glovebox-namespace>
          podSelector:
            matchLabels:
              app.kubernetes.io/name: glovebox
              app.kubernetes.io/component: scanner
      ports:
        - port: 5432
          protocol: TCP
```

### 7.5 HelmRelease values: API server, peers, policy

```yaml
journal:
  enabled: true
  roster:
    configMap: <roster-configmap>
  networkPolicy:
    # TokenReview goes to the API server: every control-plane endpoint
    # (kubectl get endpoints kubernetes -n default) and the kubernetes
    # Service's cluster IP, each a /32.
    kubernetesAPICIDRs: [192.0.2.10/32, 192.0.2.11/32, 10.96.0.1/32]
    # Who may connect to port 9095, by label. Empty selectors are refused.
    from:
      - podSelector:
          matchLabels:
            app: <gateway-app-label>
            component: <gateway-component-label>
  # Who may do what, by ServiceAccount (namespace/name). Accounts not listed
  # cannot call the journal at all.
  services:
    <gateway-namespace>/<gateway-serviceaccount>:
      read: true
      readers: "*"          # may read as any roster member
      counts: true
    <dashboard-namespace>/<dashboard-serviceaccount>:
      read: true
      readers: [e_parent1, e_parent2]
      counts: true
  staleness:
    - prefix: "source=powerschool/"
      maxAgeHours: 96
metrics:
  prometheusRule:
    enabled: true
```

Connector write rights (`write_types`, `write_scopes`, `write_subjects`) and
`networkPolicy.writerConnectors` arrive with the connectors' journal support.
