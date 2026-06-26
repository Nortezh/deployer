package postgres

import (
	"github.com/Nortezh/api"

	"github.com/deploys-app/deployer/database"
)

type Engine struct{}

func (Engine) Kind() string           { return "Postgres" }
func (Engine) Type() api.DatabaseType { return api.DatabaseTypePostgres }

func (Engine) Spec(it *api.DeployerCommandDatabaseCreate, name string) map[string]any {
	cfg := it.PostgresConfig
	if cfg == nil {
		cfg = &api.DatabaseConfigPostgres{}
	}
	spec := map[string]any{
		"user":     cfg.User,
		"password": cfg.Password,
		"storage":  database.StorageBlock(name, it.StorageSize, it.StorageClass),
	}
	if cfg.Database != "" {
		spec["database"] = cfg.Database // POSTGRES_DB; postgres-only
	}
	if it.Image != "" {
		spec["image"] = it.Image
	}
	if r := database.ResourcesBlock(it.Resources); r != nil {
		spec["resources"] = r
	}
	return spec
}
