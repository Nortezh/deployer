package postgres

import (
	"github.com/Nortezh/api"

	"github.com/deploys-app/deployer/database"
)

type Engine struct{}

func (Engine) Kind() string           { return "Postgres" }
func (Engine) Type() api.DatabaseType { return api.DatabaseTypePostgres }

func (Engine) Spec(it *api.DeployerCommandDatabaseCreate, name string) map[string]any {
	spec := map[string]any{
		"user":     it.User,
		"password": it.Password,
		"storage":  database.StorageBlock(name, it.StorageSize, it.StorageClass),
	}
	if it.Database != "" {
		spec["database"] = it.Database // POSTGRES_DB; postgres-only
	}
	if it.Image != "" {
		spec["image"] = it.Image
	}
	return spec
}
