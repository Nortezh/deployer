package k8s

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestSecretKeyRevisionAndOwnership(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	c := &Client{client: cs, namespace: "app"}
	obj := SecretKey{ID: "secrets-2", ProjectID: "2", Key: "secret-3", Value: []byte("fixture-password"), Revision: 2}
	if err := c.UpsertSecretKey(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertSecretKey(ctx, obj); err != nil {
		t.Fatal(err)
	}
	peer := SecretKey{ID: obj.ID, ProjectID: obj.ProjectID, Key: "secret-4", Value: []byte("peer"), Revision: 1}
	if err := c.UpsertSecretKey(ctx, peer); err != nil {
		t.Fatal(err)
	}
	get := func() *v1.Secret {
		t.Helper()
		s, err := cs.CoreV1().Secrets("app").Get(ctx, obj.ID, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := get()
	if before.Annotations["nortezh.io/secret-revision-3"] != "2" || before.Annotations["nortezh.io/secret-revision-4"] != "1" {
		t.Fatal("revision not stamped atomically per key")
	}
	for _, bad := range []SecretKey{
		{ID: obj.ID, ProjectID: "2", Key: obj.Key, Value: []byte("stale"), Revision: 1},
		{ID: obj.ID, ProjectID: "2", Key: obj.Key, Value: []byte("unversioned"), Revision: 0},
		{ID: obj.ID, ProjectID: "2", Key: obj.Key, Value: []byte("different"), Revision: 2},
		{ID: obj.ID, ProjectID: "1", Key: obj.Key, Value: obj.Value, Revision: 2},
		{ID: obj.ID, ProjectID: "02", Key: obj.Key, Value: obj.Value, Revision: 2},
		{ID: obj.ID, ProjectID: "2", Key: "secret-03", Value: obj.Value, Revision: 2},
		{ID: obj.ID, ProjectID: "2", Key: obj.Key, Value: obj.Value, Revision: -1},
	} {
		if err := c.UpsertSecretKey(ctx, bad); err == nil || strings.Contains(err.Error(), string(obj.Value)) {
			t.Fatal("invalid overwrite accepted or credential leaked")
		}
		if !reflect.DeepEqual(before, get()) {
			t.Fatal("rejected write changed aggregate")
		}
	}
	// A resourceVersion conflict rereads the entire object, including concurrent peer metadata/data.
	conflicted := false
	cs.PrependReactor("update", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		current, err := cs.Tracker().Get(v1.SchemeGroupVersion.WithResource("secrets"), "app", obj.ID)
		if err != nil {
			t.Fatal(err)
		}
		s := current.(*v1.Secret).DeepCopy()
		s.Data["secret-5"] = []byte("concurrent")
		s.Annotations["other.example/keep"] = "yes"
		s.Labels["other"] = "keep"
		if err := cs.Tracker().Update(v1.SchemeGroupVersion.WithResource("secrets"), s, "app"); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.ID, nil)
	})
	obj.Revision = 3
	obj.Value = []byte("new-fixture-password")
	if err := c.UpsertSecretKey(ctx, obj); err != nil {
		t.Fatal(err)
	}
	s := get()
	if !conflicted || s.Annotations["nortezh.io/secret-revision-3"] != "3" || s.Annotations["other.example/keep"] != "yes" || s.Labels["other"] != "keep" || !bytes.Equal(s.Data["secret-4"], peer.Value) || string(s.Data["secret-5"]) != "concurrent" {
		t.Fatal("retry lost revision/peer data or metadata")
	}
	value, err := c.ReadSyncedSecretKey(ctx, 2, 3, 3)
	if err != nil || !bytes.Equal(value, obj.Value) {
		t.Fatal("exact synced key not resolved")
	}
	value[0] = 'X'
	if !bytes.Equal(get().Data[obj.Key], obj.Value) {
		t.Fatal("returned value aliases source")
	}
	if value, err := c.ReadSyncedSecretKey(ctx, 2, 3, 2); err == nil || value != nil {
		t.Fatal("wrong revision resolved")
	}
	if err := c.DeleteSecretKey(ctx, obj.ID, obj.Key); err != nil {
		t.Fatal(err)
	}
	s = get()
	if _, marked := s.Annotations["nortezh.io/secret-revision-3"]; marked || s.Annotations["nortezh.io/secret-revision-4"] != "1" || !bytes.Equal(s.Data["secret-4"], peer.Value) {
		t.Fatal("key deletion left a stale marker or changed its peer")
	}
	if value, err := c.ReadSyncedSecretKey(ctx, 2, 3, 3); err == nil || value != nil {
		t.Fatal("deleted key remained eligible")
	}
}

func TestSecretKeyCreateRaceDoesNotOverwriteNewerRevision(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &Client{client: cs, namespace: "app"}
	winner := &v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "secrets-2", Namespace: "app", Labels: map[string]string{"id": "secrets-2", "projectId": "2"}, Annotations: map[string]string{"nortezh.io/secret-revision-3": "3"}}, Type: v1.SecretTypeOpaque, Data: map[string][]byte{"secret-3": []byte("newer-fixture")}}
	cs.PrependReactor("create", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		if err := cs.Tracker().Add(winner); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, winner.Name)
	})
	if err := c.UpsertSecretKey(context.Background(), SecretKey{ID: winner.Name, ProjectID: "2", Key: "secret-3", Value: []byte("older-fixture"), Revision: 2}); err == nil {
		t.Fatal("newer race winner overwritten")
	}
	actual, err := cs.CoreV1().Secrets("app").Get(context.Background(), winner.Name, metav1.GetOptions{})
	if err != nil || !reflect.DeepEqual(winner, actual) {
		t.Fatal("race winner changed")
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" || a.GetVerb() == "delete" {
			t.Fatal("stale writer mutated race winner")
		}
	}
}

func TestSecretKeyRevisionFailsClosedOnAggregateState(t *testing.T) {
	for _, mutation := range []string{"foreign", "unowned", "type", "deleting", "bad marker", "leading zero marker"} {
		t.Run(mutation, func(t *testing.T) {
			s := &v1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "secrets-2", Namespace: "app", Labels: map[string]string{"id": "secrets-2", "projectId": "2"}, Annotations: map[string]string{"nortezh.io/secret-revision-3": "1"}}, Type: v1.SecretTypeOpaque, Data: map[string][]byte{"secret-3": []byte("fixture-password")}}
			switch mutation {
			case "foreign":
				s.Labels["projectId"] = "9"
			case "unowned":
				s.Labels = nil
			case "type":
				s.Type = v1.SecretTypeBasicAuth
			case "deleting":
				now := metav1.Now()
				s.DeletionTimestamp = &now
			case "bad marker":
				s.Annotations["nortezh.io/secret-revision-3"] = "overflow-9223372036854775808"
			case "leading zero marker":
				s.Annotations["nortezh.io/secret-revision-3"] = "01"
			}
			cs := fake.NewSimpleClientset(s)
			c := &Client{client: cs, namespace: "app"}
			if err := c.UpsertSecretKey(context.Background(), SecretKey{ID: "secrets-2", ProjectID: "2", Key: "secret-3", Value: []byte("fixture-password"), Revision: 1}); err == nil {
				t.Fatal("invalid aggregate adopted")
			}
			if value, err := c.ReadSyncedSecretKey(context.Background(), 2, 3, 1); err == nil || value != nil {
				t.Fatal("invalid aggregate read")
			}
			for _, a := range cs.Actions() {
				if a.GetVerb() != "get" {
					t.Fatal("invalid aggregate mutated")
				}
			}
		})
	}
}
