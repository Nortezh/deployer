package k8s

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestSecretKeyReconciliation(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()
	client := &Client{client: clientset, namespace: "app"}
	projectSecretID := "secrets-2"

	upsert := func(key, value string) {
		t.Helper()
		if err := client.UpsertSecretKey(ctx, SecretKey{ID: projectSecretID, ProjectID: "2", Key: key, Value: []byte(value)}); err != nil {
			t.Fatal(err)
		}
	}
	get := func() *v1.Secret {
		t.Helper()
		secret, err := clientset.CoreV1().Secrets("app").Get(ctx, projectSecretID, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if secret.Type != v1.SecretTypeOpaque || secret.Labels["id"] != projectSecretID || secret.Labels["projectId"] != "2" {
			t.Fatalf("unexpected metadata: type=%q labels=%v", secret.Type, secret.Labels)
		}
		return secret
	}

	upsert("secret-1", "first")
	upsert("secret-2", "second")
	upsert("secret-1", "updated")
	upsert("secret-1", "updated") // retry
	secret := get()
	if len(secret.Data) != 2 || !bytes.Equal(secret.Data["secret-1"], []byte("updated")) || !bytes.Equal(secret.Data["secret-2"], []byte("second")) {
		t.Fatalf("unexpected data: %v", secret.Data)
	}

	conflicts := 0
	clientset.PrependReactor("update", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			return false, nil, nil
		}
		conflicts++
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, projectSecretID, fmt.Errorf("concurrent update"))
	})
	upsert("secret-2", "after-conflict")
	if conflicts != 1 || !bytes.Equal(get().Data["secret-2"], []byte("after-conflict")) {
		t.Fatal("conflict was not retried")
	}

	if err := client.DeleteSecretKey(ctx, projectSecretID, "secret-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteSecretKey(ctx, projectSecretID, "secret-1"); err != nil { // retry
		t.Fatal(err)
	}
	secret = get()
	if len(secret.Data) != 1 || !bytes.Equal(secret.Data["secret-2"], []byte("after-conflict")) {
		t.Fatalf("key delete changed other data: %v", secret.Data)
	}

	if err := client.DeleteSecretKey(ctx, projectSecretID, "secret-2"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteSecretKey(ctx, projectSecretID, "secret-2"); err != nil { // retry
		t.Fatal(err)
	}
	_, err := clientset.CoreV1().Secrets("app").Get(ctx, projectSecretID, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("project secret still exists: %v", err)
	}
}
