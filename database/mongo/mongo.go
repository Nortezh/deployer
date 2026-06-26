package mongo

import (
	"github.com/Nortezh/api"

	"github.com/deploys-app/deployer/database"
)

type Engine struct{}

func (Engine) Kind() string           { return "Mongo" }
func (Engine) Type() api.DatabaseType { return api.DatabaseTypeMongo }

func (Engine) Spec(it *api.DeployerCommandDatabaseCreate, name string) map[string]any {
	cfg := it.MongoConfig
	if cfg == nil {
		cfg = &api.DatabaseConfigMongo{}
	}
	spec := map[string]any{
		"user":     cfg.User,
		"password": cfg.Password, // mongo: no database field
		"storage":  database.StorageBlock(name, it.StorageSize, it.StorageClass),
	}
	if it.Image != "" {
		spec["image"] = it.Image
	}
	if r := database.ResourcesBlock(it.Resources); r != nil {
		spec["resources"] = r
	}
	return spec
}
