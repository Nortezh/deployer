package cnpg

import (
	"context"
	"strings"
	"testing"
)

func longhornDeleteProfile(t *testing.T, f *deleteFixture) Profile {
	p := deleteProfile()
	p.StorageClass = "reviewed-storage"
	f.objects["/apis/postgresql.cnpg.io/v1/namespaces/app-database/clusters/cnpg-1-2"]["spec"].(map[string]any)["storage"].(map[string]any)["storageClass"] = p.StorageClass
	f.objects["/apis/storage.k8s.io/v1/storageclasses/"+p.StorageClass] = map[string]any{
		"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]any{"name": p.StorageClass, "uid": "csi-class"},
		"provisioner": longhornDriver, "reclaimPolicy": "Delete", "volumeBindingMode": "Immediate",
	}
	delete(f.objects, "/apis/apps/v1/namespaces/kube-system/deployments/local-path-provisioner")
	for _, suffix := range []string{"1", "2"} {
		f.objects["/api/v1/namespaces/app-database/persistentvolumeclaims/cnpg-1-2-"+suffix]["spec"].(map[string]any)["storageClassName"] = p.StorageClass
		pv := f.objects["/api/v1/persistentvolumes/pv-"+suffix]
		pv["metadata"].(map[string]any)["finalizers"] = []any{csiDeleteFinalizer}
		spec := pv["spec"].(map[string]any)
		delete(spec, "local")
		delete(spec, "nodeAffinity")
		spec["storageClassName"] = p.StorageClass
		spec["csi"] = map[string]any{"driver": longhornDriver, "volumeHandle": "volume-" + suffix}
	}
	bindDeleteFixture(t, f, p)
	return p
}

func TestCSIDeleteRejectsUnsafeCapture(t *testing.T) {
	for _, kind := range []string{"driver", "local-pv", "missing-finalizer", "empty-handle", "duplicate-handle", "retained", "class-driver", "binding-mode", "class-denied", "foreign-pv"} {
		t.Run(kind, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			p := longhornDeleteProfile(t, f)
			pv := f.objects["/api/v1/persistentvolumes/pv-1"]
			spec := pv["spec"].(map[string]any)
			class := f.objects["/apis/storage.k8s.io/v1/storageclasses/"+p.StorageClass]
			switch kind {
			case "driver":
				spec["csi"].(map[string]any)["driver"] = "unknown"
			case "local-pv":
				delete(spec, "csi")
				spec["local"] = map[string]any{"path": "/wrong"}
			case "missing-finalizer":
				delete(pv["metadata"].(map[string]any), "finalizers")
			case "empty-handle":
				spec["csi"].(map[string]any)["volumeHandle"] = ""
			case "duplicate-handle":
				f.objects["/api/v1/persistentvolumes/pv-2"]["spec"].(map[string]any)["csi"].(map[string]any)["volumeHandle"] = "volume-1"
			case "retained":
				spec["persistentVolumeReclaimPolicy"] = "Retain"
			case "class-driver":
				class["provisioner"] = "unknown"
			case "binding-mode":
				class["volumeBindingMode"] = "WaitForFirstConsumer"
			case "class-denied":
				f.denied = "/apis/storage.k8s.io/v1/storageclasses/" + p.StorageClass
			case "foreign-pv":
				f.objects["/api/v1/persistentvolumes/foreign"] = map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": "foreign", "uid": "foreign"}, "spec": map[string]any{"csi": map[string]any{"driver": longhornDriver, "volumeHandle": "volume-1"}}}
			}
			if done, err := ApplyDelete(context.Background(), c, deleteCommand(), p); done || err == nil {
				t.Fatal("unsafe capture accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.deletes) != 0 || f.objects["/api/v1/namespaces/app-database/configmaps/cnpg-1-2-retirement"] != nil {
				t.Fatal("wrote before validating storage")
			}
		})
	}
}

func TestCSIDeleteBlocksChangedPVOnRetry(t *testing.T) {
	for _, kind := range []string{"uid", "binding", "handle", "driver", "finalizer", "denied", "duplicate-handle"} {
		t.Run(kind, func(t *testing.T) {
			c, f := newDeleteFixture(t)
			p := longhornDeleteProfile(t, f)
			it := deleteCommand()
			if done, err := ApplyDelete(context.Background(), c, it, p); done || err != nil {
				t.Fatal("capture failed", err)
			}
			f.mu.Lock()
			pv := f.objects["/api/v1/persistentvolumes/pv-1"]
			spec := pv["spec"].(map[string]any)
			switch kind {
			case "uid":
				pv["metadata"].(map[string]any)["uid"] = "replacement"
			case "binding":
				spec["claimRef"].(map[string]any)["name"] = "cnpg-1-2-2"
				spec["claimRef"].(map[string]any)["uid"] = "pvc-2"
			case "handle":
				spec["csi"].(map[string]any)["volumeHandle"] = "replacement"
			case "driver":
				spec["csi"].(map[string]any)["driver"] = "unknown"
			case "finalizer":
				delete(pv["metadata"].(map[string]any), "finalizers")
			case "denied":
				f.denied = "/api/v1/persistentvolumes"
			case "duplicate-handle":
				f.objects["/api/v1/persistentvolumes/foreign"] = map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": "foreign", "uid": "foreign"}, "spec": map[string]any{"csi": map[string]any{"driver": longhornDriver, "volumeHandle": "volume-1"}}}
			}
			f.mu.Unlock()
			if done, err := ApplyDelete(context.Background(), c, it, p); done || err == nil {
				t.Fatal("changed PV accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.deletes) != 1 || f.objects["/api/v1/namespaces/allocator/configmaps/"+allocationMap]["data"].(map[string]any)["lb_6109"] == nil {
				t.Fatal("unexpected delete or allocation release")
			}
			for _, request := range f.requests {
				if strings.Contains(request, "longhorn.io/") || strings.Contains(request, "/deployments/") {
					t.Fatal("must rely on stock CSI, not installation internals", request)
				}
			}
		})
	}
}
