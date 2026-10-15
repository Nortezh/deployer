package cnpg

import (
	"context"
	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"strconv"
	"strings"
)

func splitDeleteAllocation(key string) (string, string, bool) {
	i := strings.LastIndex(key, "_")
	if i <= 0 {
		return "", "", false
	}
	port, err := strconv.Atoi(key[i+1:])
	if err != nil || port < 1 || port > 65535 {
		return "", "", false
	}
	return key[:i], strconv.Itoa(port), true
}

func releaseDeleteAllocation(ctx context.Context, c *k8s.Client, p Profile, name string, it *api.DeployerCommandCNPGDelete, inv *deleteInventory) error {
	owner := allocationOwner(c.DBNamespace(), name, it.ID)
	maps := c.Core().CoreV1().ConfigMaps(p.AllocationNamespace)
	records := c.Core().CoreV1().ConfigMaps(c.DBNamespace())
	// A completed receipt is idempotent and never removes a later port owner's entry.
	record, err := records.Get(ctx, retirementName(name), metav1.GetOptions{})
	if err != nil {
		return errDeleteBlocked
	}
	released := record.Annotations["nortezh.io/endpoint-released"] == it.OperationID
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := maps.Get(ctx, allocationMap, metav1.GetOptions{})
		if err != nil || cm.UID != inv.AllocationMapUID {
			return errDeleteBlocked
		}
		for key, value := range cm.Data {
			if value == owner && key != inv.AllocationKey {
				return errDeleteBlocked
			}
		}
		value, occupied := cm.Data[inv.AllocationKey]
		if released {
			if value == owner {
				return errDeleteBlocked
			}
			return nil
		}
		if occupied && value != owner {
			return errDeleteBlocked
		}
		if !occupied {
			return nil
		}
		delete(cm.Data, inv.AllocationKey)
		_, err = maps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}
	if released {
		return nil
	}
	// shortcut: a lost receipt plus port reuse retains the name; recover only with verified ownership.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := records.Get(ctx, record.Name, metav1.GetOptions{})
		if err != nil || current.UID != record.UID {
			return errDeleteBlocked
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations["nortezh.io/endpoint-released"] = it.OperationID
		_, err = records.Update(ctx, current, metav1.UpdateOptions{})
		return err
	})
}
