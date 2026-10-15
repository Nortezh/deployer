package main

import (
	"context"
	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/database/cnpg"
	"reflect"
)

func (w *Worker) cnpgDelete(ctx context.Context, it *api.DeployerCommandCNPGDelete) {
	if it == nil || it.Valid() != nil || w.Client == nil {
		return
	}
	// Snapshot of current authenticated intent, not a late-writer OS fence.
	current, err := w.Deployer.GetCommands(ctx, &api.Empty{})
	if err != nil || current == nil {
		return
	}
	found := false
	for _, cmd := range *current {
		if cmd != nil && cmd.CNPGDelete != nil && cmd.ValidCNPG() == nil && reflect.DeepEqual(cmd.CNPGDelete, it) {
			found = true
			break
		}
	}
	if !found {
		return
	}
	absent, err := cnpg.ApplyDelete(ctx, w.Client, it, w.CNPG)
	if err == nil && !absent {
		return
	}
	result := &api.DeployerSetResultCNPGDelete{DeployerCommandCNPGIdentity: it.DeployerCommandCNPGIdentity, Phase: it.Phase, ResourceAbsent: absent}
	if err != nil {
		result.ResourceAbsent = false
		result.FailureCode = api.CNPGFailureCleanup
	}
	w.results = append(w.results, &api.DeployerSetResultItem{CNPGDelete: result})
}
