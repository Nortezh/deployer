# deployer

Deployer agent operates within Kubernetes cluster,
retrieves commands from API server then transforms commands to Kubernetes configurations,
and applies those configurations to the cluster.

## CNPG local development

CNPG remains disposable-only: `LOCAL=true`, `CNPG_DISPOSABLE_SPIKE=true`, and a
complete explicit profile are required. There is no staging/prod runtime path.
Endpoint allocation, TLS trust, operator installation, RBAC and PVC retention must
be confirmed before implementing an operational path; do not use local mode to
bypass these prerequisites on staging.

Local checks use an HTTP mock, not a cluster. Disable the disposable integration
opt-in when running them:

```sh
env -u CNPG_TEST_PASSWORD go test ./...
```
