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
	result := &api.DeployerSetResultCNPGDelete{DeployerCommandCNPGIdentity: it.DeployerCommandCNPGIdentity, Phase: it.Phase, ResourceAbsent: absent,
		CreateOperationID: it.CreateOperationID, SecretID: it.SecretID, SecretRevision: it.SecretRevision}
	if absent {
		result.Receipt, err = cnpg.DeleteReceipt(ctx, w.Client, it)
	}
	if it.Phase == api.CNPGDeleteFinalize {
		result.Receipt = it.Receipt
	}
	if err != nil {
		result.ResourceAbsent = false
		result.FailureCode = api.CNPGFailureCleanup
		result.FailureStage = cnpg.DeleteFailureStage(err)
		if it.Phase == api.CNPGDeleteFinalize {
			result.FailureStage = api.CNPGDeleteFinalize
		}
	}
	if result.Valid() != nil {
		return
	}
	w.results = append(w.results, &api.DeployerSetResultItem{CNPGDelete: result})
}
