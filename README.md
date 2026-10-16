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

The isolated local spike remains behind `LOCAL=true` and
`CNPG_DISPOSABLE_SPIKE=true` with its separate explicit profile.

Local checks use an HTTP mock, not a cluster. Disable the disposable integration
opt-in when running them:

```sh
env -u CNPG_TEST_PASSWORD go test ./...
```
