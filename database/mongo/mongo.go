package mongo

import (
	"github.com/Nortezh/api"

	"github.com/deploys-app/deployer/database"
)

type Engine struct{}

func (Engine) Kind() string           { return "Mongo" }
func (Engine) Type() api.DatabaseType { return api.DatabaseTypeMongo }

func (Engine) Spec(it *api.DeployerCommandDatabaseCreate, name string) map[string]any {
	spec := map[string]any{
		"user":     it.User,
		"password": it.Password, // mongo: no database field
		"storage":  database.StorageBlock(name, it.StorageSize, it.StorageClass),
	}
	if it.Image != "" {
		spec["image"] = it.Image
	}
	return spec
}
