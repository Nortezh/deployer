package cnpg

import (
	"context"
	"crypto/subtle"
	"errors"
	"reflect"
	"strconv"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Recovery never falls back to initdb, latest backup, a replacement source or profile.
func ApplyRestore(ctx context.Context, c *k8s.Client, it *api.DeployerCommandCNPGRestore, p Profile) (string, int, bool, error) {
	if c == nil || it == nil || it.Valid() != nil || p.AllocationNamespace == "" || !p.Enabled() || it.Material.Image != p.Image || it.Material.StorageClass != p.StorageClass {
		return "", 0, false, errBootstrapUnavailable
	}
	// A checkpointed backup observation cannot create a missing Backup CR.
	observed, err := ApplyBackup(ctx, c, &it.Backup, p)
	if err != nil || observed == nil || observed.Status != api.CNPGBackupCompleted || !reflect.DeepEqual(observed.Material, &it.Material) {
		return "", 0, false, errors.New("CNPG selected backup unavailable or mismatched")
	}
	sourceName := createName(&api.DeployerCommandCNPGCreate{DeployerCommandCNPGIdentity: it.Backup.DeployerCommandCNPGIdentity})
	source, err := c.Core().CoreV1().Secrets(c.DBNamespace()).Get(ctx, sourceName+"-app", metav1.GetOptions{})
	if err != nil || string(source.Data["username"]) != it.Username {
		return "", 0, false, errBootstrapUnavailable
	}
	password, err := c.ReadSyncedSecretKey(ctx, it.ProjectID, it.SecretID, it.SecretRevision)
	if err != nil || subtle.ConstantTimeCompare(password, source.Data["password"]) == 1 {
		return "", 0, false, errBootstrapUnavailable
	}
	if _, err = EnsureBootstrapSecret(ctx, c, &it.DeployerCommandCNPGCreate); err != nil {
		return "", 0, false, err
	}
	// CNPG reconciles this recovery owner to the fresh target Secret after promotion.
	return apply(ctx, c, &api.DeployerCommandDatabaseCreate{
		ID: it.ID, ProjectID: it.ProjectID, Name: "cnpg-" + strconv.FormatInt(it.ID, 10), Type: api.DatabaseTypePostgres, Provider: api.DatabaseProviderCNPG,
		StorageSize: it.StorageSize, Resources: it.Resources, PostgresConfig: &api.DatabaseConfigPostgres{User: it.Username, Password: string(password), Database: it.Database},
	}, p, it)
}
