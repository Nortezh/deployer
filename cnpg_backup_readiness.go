package main

import (
	"context"
	"reflect"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
)

func (w *Worker) cnpgBackupReadiness(ctx context.Context, it *api.DeployerCommandCNPGBackupReadiness) {
	if it == nil || it.Valid() != nil || w.Client == nil || w.CNPG.AllocationNamespace == "" || !w.CNPG.Enabled() {
		return
	}
	current, err := w.Deployer.GetCommands(ctx, &api.Empty{})
	if err != nil || current == nil {
		return
	}
	for _, command := range *current {
		if command == nil || command.CNPGBackupReadiness == nil || command.ValidCNPG() != nil || !reflect.DeepEqual(command.CNPGBackupReadiness, it) {
			continue
		}
		result, err := cnpg.ObserveBackupReadiness(ctx, w.Client, it, w.CNPG)
		if err == nil && result != nil {
			w.results = append(w.results, &api.DeployerSetResultItem{CNPGBackupReadiness: result})
		}
		return
	}
}
