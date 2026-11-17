# deployer

Deployer agent operates within Kubernetes cluster,
retrieves commands from API server then transforms commands to Kubernetes configurations,
and applies those configurations to the cluster.

## CNPG

The location-configured path is disabled unless `CNPG_ENABLED=true` and a complete
profile is supplied: allocation namespace/range, digest-pinned PostgreSQL image,
StorageClass, and `CNPG_PVC_RETENTION=delete`. It shares the KDB port-allocation
ConfigMap and only supports disposable PVC deletion; production PVC retention is not
implemented. No location is enabled by this repository change. Do not enable it
before reviewing endpoint, TLS, RBAC, backend compatibility and cleanup on target.

Dedicated create code now accepts only a valid sole CNPG create in the operational
profile, rechecks the authenticated Backend's current command and synced Secret
revision, and reuses stock reconciliation with immutable database-ID names. Ready
requires two ready instances and an authenticated `verify-full` query. Legacy CNPG
and mixed commands remain rejected. The Backend's exact-location
`database_cnpg_create` integration gate is not installed/enabled by this change;
customer availability and real Operator/SQL acceptance remain gated.

Restored targets use `bootstrap.recovery`. Guarded deletion supports both initdb
and recovery targets and retains the same ownership, PVC/PV and port-release checks;
plugin-enabled backup sources still remain blocked. Cleanup does not delete the
external recovery source or its ObjectStore. Restored targets now support Backup
and daily admission after their Restore completes and their own backup readiness is
verified. They use a distinct archive server prefix and independent retention set;
unready or unsupported targets remain unavailable.

## Creation-time Barman binding

Only a **new** operational Cluster is given `spec.plugins` when the optional backup
profile has a positive revision and the referenced ObjectStore/credential metadata
passes the existing supported-profile preflight. The WAL archiver is
`barman-cloud.cloudnative-pg.io`, with `barmanObjectName` from the location profile and
`serverName` equal to the immutable Cluster name (`cnpg-<database ID>-<project ID>`).
The Worker does not create or modify an ObjectStore, credentials, IAM or the
ObjectStore's retention policy.

Missing/incomplete/unsupported/unreadable backup prerequisites create an ordinary
Cluster without the plugin; they do not prevent database provisioning. Creation-time
preflight is not runtime proof: plugin/operator/storage failures or changes after the
snapshot can still delay provisioning or disable backups. Existing TLS/query checks
remain authoritative for database Ready, and the separate verifier/Backend gates
remain authoritative for backup availability.

The binding intent is evaluated only when the Cluster GET returns NotFound. A new
bound Cluster starts with `cnpg.io/reconciliationLoop=disabled`, so CNPG cannot create
a Pod before the actual UID is known. After UID/profile/scope checks, one guarded
patch commits the bound annotation and plugin while removing the fence. A lost
response is reconciled against the same root; a denied/uncertain activation leaves
it fenced, not silently available. A fenced root missing its binding intent fails
closed rather than continuing as an ordinary root. Older unbound roots without this fence need
explicit review rather than an unsafe retry activation. A Cluster first created
without backup remains ordinary and is never silently backfilled.

Restored targets keep their selected recovery source in `externalClusters` and receive
a distinct target WAL archive binding under the same creation-only policy. A historical
source archive name equal to the target's immutable name suppresses target archiving
rather than reusing that prefix; recovery remains bound to the selected source.
Backup/readiness validation supports recovery bootstrap with the target's own immutable
Secret, the same supported ObjectStore and a source server distinct from the target.

Enabling a profile starts continuous WAL archiving on new ordinary/restored databases,
even before a manual backup; cap7 is not a WAL-byte bound. **Existing deletion guards
still block plugin-enabled databases**, even without a successful backup. Scoped
archive cleanup/target-environment acceptance remains separately gated; this change is not rollout
or destructive-cleanup approval and does not remediate the existing `cpng` incident.

## Daily Backup and count retention

Backend command polling admits daily Backups at 02:00 `Asia/Bangkok`, with one retry
five minutes after a definitive failure. Unknown/running attempts reconcile the same
operation; missed slots coalesce into one latest-slot catch-up. Manual and daily
successes share the same latest-seven retention set.

A successful replacement may temporarily bring the retained count to eight. Backend
persists the exact retention intent; the Worker reconciles one deterministic Job using
the pinned stock Barman image, with delete init and catalog main containers. Unresolved
retention blocks conflicting Backup/Restore/deletion. Completion requires matching
Job/Pod identity, terminal execution, empty delete logs and the exact retained-seven
catalog. Unknown or blocked cleanup does not authorize another deletion Job.

Availability is separate from immutable Backup execution history. Expiration does not
turn a successful Backup into a failure; Backend Restore admission and dispatch reject
unavailable selections. Historical unbound material is not automatically enrolled in
rotation. Count convergence is not proof of physical WAL erasure or backup integrity.

This path has local SDK/Backend and fake-Kubernetes test evidence only. The SDK contract
is unpublished, consumer pins are unchanged, and Console integration remains pending.
Actual stock-image/CLI/log behavior, provider 8→7 and surviving selected Restore,
credentials/RBAC/IAM, migration, publication and rollout require separate acceptance
and authorization. Keep runtime gates closed.

## Backup target

New manual and daily Backup resources explicitly use `spec.target: primary`. On the
pinned PostgreSQL16/Barman3.20.1 path, stock `pg_backup_stop()` switches WAL and waits for
archival on the primary. The previous standby path with `archive_mode=on` does not
provide that wait. This trades extra primary I/O for the stock completion behavior;
there is no new poller, WAL-switch query, permission or SDK/Backend field.
Existing immutable Backup resources are never patched or recreated, including those
with an omitted/`prefer-standby` target. This does not retroactively certify their
required WAL. Manifest/retry/restore regressions and a new pinned local native test
(2026-10-20) passed: actual primary instance, required end-WAL present on the first
read456ms after observed product completion, selected Restore admitted594ms after
completion, exact data/fresh password/verify-full TLS/source preservation. No test WAL
switch, archive wait or object-read retry was used. The isolated fixture/private
credentials were removed; no owner environment/production rollout was performed.

## Read-only backup readiness

Published SDK `.8` readiness commands now have a read-only Worker verifier. The
verifier itself never configures plugins, backfills databases, creates backups,
changes IAM or enables a location. Ordinary create remains independent of missing
optional backup settings:

- `CNPG_BACKUP_OBJECT_STORE`: exact ObjectStore name in the database namespace.
- `CNPG_BACKUP_PROFILE_REVISION`: positive revision matching the Backend-owned profile.

Missing/invalid settings and mismatched revisions produce `backup_unavailable` for
an otherwise valid current operational check. Local-spike, stale and mixed commands
remain non-executable. Results preserve the exact original check; they cannot fail a
Backup/Restore operation or release its guard. Backend owns expiry and expected
profile/hash/ObjectStore identity. The existing Backend `0032.sql` migration/profile
configuration requires separate owner approval before use on an existing environment.

The supported observation is an ordinary initdb or supported recovery-bootstrap source
with its own immutable bootstrap Secret, HA2/Ready/ContinuousArchiving success, loaded
plugin `0.15.1`, and a running owned primary with the profile PostgreSQL image and
the pinned Barman sidecar
from the recorded `0.15.1` manifest. The archiver can be a regular container or a
restartable init container (`restartPolicy: Always`); its corresponding status must
be running and ready. One-shot init containers and conflicting layouts are rejected.
S3 configuration must use an explicit endpoint and
`accessKeyId`/`secretAccessKey` name/key Secret selectors. Optional compression is gzip;
positive WAL `maxParallel`, data `jobs` and boolean `immediateCheckpoint` are supported.
ObjectStore automatic retention, other providers/credential modes, endpoint CA overrides,
custom sidecar env/args/resources and nondefault sidecar settings fail closed rather than
being guessed compatible. This is not R2 compatibility or production acceptance.

Verification makes Kubernetes GETs only. It reads the existing owned allocation/LB
endpoint (never reserves a port), connects using the source CA with `verify-full`,
and queries `pg_catalog.pg_stat_archiver` in a **READ ONLY** transaction with a fixed
`pg_catalog` search path. A successful archive must postdate the primary Pod and
source/ObjectStore/credential configuration changes, with no newer failed archive.
Metadata timestamps require a one-second conservative boundary. Credential data
requires managed-field timestamp provenance even when the Secret is now immutable;
changed Cluster/ObjectStore specs without it also stay unavailable. Idle/new/changed sources
can remain unavailable until a real WAL is archived; the verifier never switches WAL
or fabricates readiness. Relevant identities/configuration are rechecked after SQL.
This is a bounded observation, not a write fence or a future-availability guarantee.

### Configuration fingerprint v1

`ConfigurationHash` is lowercase SHA-256 of the compact UTF-8 `encoding/json.Marshal`
output of `readinessProfile`, with its declared field order (map keys sorted by Go):
`Format`, `Namespace`, `Image`, `StorageClass`, `Plugin`, `PluginVersion`, `SidecarImage`,
`ObjectStoreName`, `ObjectStoreUID`, `Spec`, `Credentials`. `Format` is
`nortezh.cnpg.backup-profile.v1`; `Spec` is the validated observed ObjectStore spec,
including API defaults, without extra normalization. `Credentials` is ordered
access-key then secret-key, each containing `Name`, `Key`, `UID`, `ResourceVersion`.
No credential values, source passwords or CA bytes enter the fingerprint/receipt.
Source IDs, WAL timestamps and profile revision are not profile hash inputs; their
identity/freshness is checked separately. Secret replacements/revisions invalidate the
digest even if values happen to be unchanged. The Backend expected digest must be
reviewed/configured independently, not auto-adopted from a Worker observation. The
fingerprint test fixes a public synthetic golden vector for this format.

Unit tests use HTTP Kubernetes mocks and a synthetic PostgreSQL wire server with real
TLS/password exchange. Separately approved local native acceptance (2026-10-20) used a
fresh k3s1.31.11/CNPG1.27.4/PostgreSQL16.15/Barman0.15.1/MinIO fixture and published SDK
`.8`: automatic source binding, actual read-only SQL/WAL verification and Backend
admission, Backup catalog/base/end-WAL, selected physical Restore with fresh password/TLS,
and distinct target WAL prefix passed. The fixture exposed the restartable-init layout;
the verifier fix passed native execution and regression tests. Source creation preceded
that verifier-only fix; Restore used the rebuilt Worker. No Cluster was manually patched.

Stock Backup `completed`/catalog `DONE` can precede archival of its still-open tail WAL
(`archive_timeout=5min` in the fixture). Explicit fixture WAL switches and read-only
object checks established required end-WAL persistence before Restore; this does not
prove immediate restore-after-DONE or change product completion semantics. The verifier
never switches WAL. The whole isolated fixture/private credentials were removed; this
is not product archive-cleanup proof or native acceptance of the new daily/retention
path. Explicit administrator backfill, native restored-source Backup/retention and
failure-injection checks, target-environment/R2 acceptance and safe whole-archive
deletion remain separate follow-ups.

The isolated local spike remains behind `LOCAL=true` and
`CNPG_DISPOSABLE_SPIKE=true` with its separate explicit profile.

Local checks use an HTTP mock, not a cluster. Disable the disposable integration
opt-in when running them:

```sh
env -u CNPG_TEST_PASSWORD go test ./...
```
