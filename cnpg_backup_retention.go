package main

import (
	"context"
	"reflect"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
)

func (w *Worker) cnpgBackupRetention(ctx context.Context, it *api.DeployerCommandCNPGBackupRetention) {
	if it == nil || it.Valid() != nil || w.Client == nil || w.CNPG.AllocationNamespace == "" || !w.CNPG.Enabled() {
		return
	}
	current, err := w.Deployer.GetCommands(ctx, &api.Empty{})
	if err != nil || current == nil {
		return
	}
	for _, command := range *current {
		if command == nil || command.CNPGBackupRetention == nil || command.ValidCNPG() != nil || !reflect.DeepEqual(command.CNPGBackupRetention, it) {
			continue
		}
		result, err := cnpg.ApplyBackupRetention(ctx, w.Client, it, w.CNPG)
		if err == nil && result != nil && result.Valid() == nil {
			w.results = append(w.results, &api.DeployerSetResultItem{CNPGBackupRetention: result})
		}
		return
	}
}
