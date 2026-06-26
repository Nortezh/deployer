package redis

import (
	"github.com/Nortezh/api"

	"github.com/deploys-app/deployer/database"
)

type Engine struct{}

func (Engine) Kind() string           { return "Redis" }
func (Engine) Type() api.DatabaseType { return api.DatabaseTypeRedis }

func (Engine) Spec(it *api.DeployerCommandDatabaseCreate, name string) map[string]any {
	cfg := it.RedisConfig
	if cfg == nil {
		cfg = &api.DatabaseConfigRedis{}
	}
	spec := map[string]any{
		"password": cfg.Password, // redis: no user, no database
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
