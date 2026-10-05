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

The isolated local spike remains behind `LOCAL=true` and
`CNPG_DISPOSABLE_SPIKE=true` with its separate explicit profile.

Local checks use an HTTP mock, not a cluster. Disable the disposable integration
opt-in when running them:

```sh
env -u CNPG_TEST_PASSWORD go test ./...
```
