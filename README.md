# deployer

Deployer agent operates within Kubernetes cluster,
retrieves commands from API server then transforms commands to Kubernetes configurations,
and applies those configurations to the cluster.

## CNPG

The location-configured path is disabled unless `CNPG_ENABLED=true` and a complete
profile is supplied: allocation namespace/range, digest-pinned PostgreSQL image,
StorageClass, and `CNPG_PVC_RETENTION=delete`. It shares the KDB port-allocation
ConfigMap and only supports disposable PVC deletion; production retention is not
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
external recovery source or its ObjectStore. New backups on restored targets are
unavailable until target WAL archiving and backup lifecycle are supported; the
Backend rejects admission and reports `backupAvailable=false` for these targets.

## Read-only backup readiness

Published SDK `.8` readiness commands now have a Worker verifier. This does **not**
configure plugins, backfill databases, create backups, change IAM or enable a location.
Ordinary create remains independent of the optional backup settings:

- `CNPG_BACKUP_OBJECT_STORE`: exact ObjectStore name in the database namespace.
- `CNPG_BACKUP_PROFILE_REVISION`: positive revision matching the Backend-owned profile.

Missing/invalid settings and mismatched revisions produce `backup_unavailable` for
an otherwise valid current operational check. Local-spike, stale and mixed commands
remain non-executable. Results preserve the exact original check; they cannot fail a
Backup/Restore operation or release its guard. Backend owns expiry and expected
profile/hash/ObjectStore identity. The existing Backend `0032.sql` migration/profile
configuration requires separate owner approval before use on an existing environment.

The supported observation is an ordinary initdb source with the owned immutable
bootstrap Secret, HA2/Ready/ContinuousArchiving success, loaded plugin `0.15.1`, and a
running owned primary with the profile PostgreSQL image and the pinned Barman sidecar
from the recorded `0.15.1` manifest. S3 configuration must use an explicit endpoint and
`accessKeyId`/`secretAccessKey` name/key Secret selectors. Optional compression is gzip;
positive WAL `maxParallel`, data `jobs` and boolean `immediateCheckpoint` are supported.
Automatic retention, other providers/credential modes, endpoint CA overrides, custom
sidecar env/args/resources and nondefault sidecar settings fail closed rather than
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

Tests use HTTP Kubernetes mocks and a synthetic PostgreSQL wire server with real
TLS/password exchange; they do not prove native Barman/MinIO uploads or real SQL
execution. No native fixture was created for this verifier change. Provisioning,
explicit administrator backfill, restored-source backups and archive deletion remain
separate follow-ups.

The isolated local spike remains behind `LOCAL=true` and
`CNPG_DISPOSABLE_SPIKE=true` with its separate explicit profile.

Local checks use an HTTP mock, not a cluster. Disable the disposable integration
opt-in when running them:

```sh
env -u CNPG_TEST_PASSWORD go test ./...
```
