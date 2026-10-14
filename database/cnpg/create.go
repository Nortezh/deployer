package cnpg

import (
	"context"
	"strconv"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
)

// Dedicated creates use immutable database IDs, never reuse a deleted business-name slot.
func createName(it *api.DeployerCommandCNPGCreate) string {
	return k8s.ResourceID(it.ProjectID, "cnpg-"+strconv.FormatInt(it.ID, 10))
}

func ApplyCreate(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGCreate, p Profile) (string, int, bool, error) {
	if c == nil || it == nil || it.Valid() != nil || p.AllocationNamespace == "" || !p.Enabled() {
		return "", 0, false, errBootstrapUnavailable
	}
	if _, err := EnsureBootstrapSecret(ctx, c, it); err != nil {
		return "", 0, false, err
	}
	password, err := c.ReadSyncedSecretKey(ctx, it.ProjectID, it.SecretID, it.SecretRevision)
	if err != nil {
		return "", 0, false, errBootstrapUnavailable
	}
	// The legacy-shaped value is in-memory only: never persist or send plaintext CNPG wire.
	return Apply(ctx, c, &api.DeployerCommandDatabaseCreate{
		ID: it.ID, ProjectID: it.ProjectID, Name: "cnpg-" + strconv.FormatInt(it.ID, 10),
		Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG, StorageSize: it.StorageSize, Resources: it.Resources,
		PostgresConfig: &api.DatabaseConfigPostgres{User: it.Username, Password: string(password), Database: it.Database},
	}, p)
}
