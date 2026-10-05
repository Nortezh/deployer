package cnpg

import (
	"context"
	"errors"
	"testing"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestSharedAllocator(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset(
		&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "lb", Labels: map[string]string{"kdb/role": "lb"}, Annotations: map[string]string{"kdb.io/host": "lb.example"}}},
		&v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationMap, Namespace: "allocator"}, Data: map[string]string{"lb_6200": "legacy/db"}},
	)
	conflict := true
	client.PrependReactor("update", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		if !conflict {
			return false, nil, nil
		}
		conflict = false
		cm := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: allocationMap, Namespace: "allocator"}, Data: map[string]string{"lb_6200": "legacy/db", "lb_6201": "legacy/other"}}
		if err := client.Tracker().Update(v1.SchemeGroupVersion.WithResource("configmaps"), cm, "allocator"); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, allocationMap, errors.New("concurrent KDB write"))
	})
	owner := allocationOwner("db", "orders", 9)
	ep, err := reserve(ctx, client, "allocator", "6200-6202", owner)
	if err != nil || ep.Port != "6202" || ep.Host != "lb.example" {
		t.Fatalf("reservation: %+v %v", ep, err)
	}
	again, err := reserve(ctx, client, "allocator", "6200-6202", owner)
	if err != nil || again != ep {
		t.Fatal("reservation not idempotent", err)
	}
	if _, err := reserve(ctx, client, "allocator", "6200-6202", "cnpg:other"); err == nil {
		t.Fatal("exhausted range accepted")
	}
	if err := release(ctx, client, "allocator", "cnpg:wrong-owner"); err != nil {
		t.Fatal(err)
	}
	cm, _ := client.CoreV1().ConfigMaps("allocator").Get(ctx, allocationMap, metav1.GetOptions{})
	if cm.Data["lb_6202"] != owner {
		t.Fatal("wrong owner released reservation")
	}
	for i := 0; i < 2; i++ {
		if err := release(ctx, client, "allocator", owner); err != nil {
			t.Fatal(err)
		}
	}
	cm, _ = client.CoreV1().ConfigMaps("allocator").Get(ctx, allocationMap, metav1.GetOptions{})
	if len(cm.Data) != 2 || cm.Data["lb_6200"] != "legacy/db" || cm.Data["lb_6201"] != "legacy/other" {
		t.Fatal("legacy reservations modified")
	}
	if _, err := reserve(ctx, client, "missing", "6200-6202", owner); err == nil {
		t.Fatal("missing map accepted")
	}
}

func TestOperationalProfileFailsClosed(t *testing.T) {
	good := Profile{AllocationNamespace: "allocator", PortRange: "6200-6202", PVCRetention: "delete", StorageClass: "test", Image: "pinned"}
	if !good.Enabled() {
		t.Fatal("explicit profile rejected")
	}
	for _, ports := range []string{"", "0-2", "2-1", "1-65536", "1-2,3-4", "abc"} {
		p := good
		p.PortRange = ports
		if p.Enabled() {
			t.Fatalf("accepted range %q", ports)
		}
	}
	for _, policy := range []string{"", "retain", "unknown"} {
		p := good
		p.PVCRetention = policy
		if p.Enabled() {
			t.Fatalf("accepted unimplemented retention %q", policy)
		}
	}
}
