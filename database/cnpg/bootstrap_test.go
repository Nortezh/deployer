package cnpg

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const bootstrapPassword = "fixture-bootstrap-password-never-log"

func bootstrapCommand() *api.DeployerCommandCNPGCreate {
	return &api.DeployerCommandCNPGCreate{
		DeployerCommandCNPGIdentity: api.DeployerCommandCNPGIdentity{Version: 1, ID: 1, ProjectID: 2, OperationID: "a1111111-1111-4111-8111-111111111111"},
		Name:                        "orders", Username: "app", StorageSize: 1024, SecretID: 3, SecretRevision: 1,
	}
}

type bootstrapServer struct {
	mu                     sync.Mutex
	source, target         *v1.Secret
	calls, writes          int
	denySource, denyTarget bool
	retired                bool
	race                   func(*v1.Secret) *v1.Secret
}

func newBootstrapFixture(t *testing.T) (*k8s.Client, *bootstrapServer) {
	t.Helper()
	state := &bootstrapServer{source: &v1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: "secrets-2", Namespace: "app", Labels: map[string]string{"id": "secrets-2", "projectId": "2"}, Annotations: map[string]string{"nortezh.io/secret-revision-3": "1"}},
		Type:       v1.SecretTypeOpaque, Data: map[string][]byte{"secret-3": []byte(bootstrapPassword), "secret-4": []byte("untouched-peer")},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		state.calls++
		w.Header().Set("Content-Type", "application/json")
		status := func(code int, reason string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReason(reason), Code: int32(code), Message: bootstrapPassword})
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/app-database/configmaps/cnpg-1-2-retirement":
			if state.retired {
				_ = json.NewEncoder(w).Encode(v1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}})
			} else {
				status(404, "NotFound")
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/app/secrets/secrets-2":
			if state.denySource {
				status(403, "Forbidden")
				return
			}
			if state.source == nil {
				status(404, "NotFound")
				return
			}
			_ = json.NewEncoder(w).Encode(state.source)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/app-database/secrets/cnpg-1-2-app":
			if state.denyTarget {
				status(403, "Forbidden")
				return
			}
			if state.target == nil {
				status(404, "NotFound")
				return
			}
			_ = json.NewEncoder(w).Encode(state.target)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/app-database/secrets":
			state.writes++
			var obj v1.Secret
			if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
				t.Error("invalid create payload")
				status(400, "BadRequest")
				return
			}
			obj.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
			if state.race != nil {
				state.target = state.race(&obj)
				state.race = nil
				status(409, "AlreadyExists")
				return
			}
			state.target = &obj
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(state.target)
		default:
			t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
			status(403, "Forbidden")
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("KUBE_PROXY_URL", server.URL)
	c, err := k8s.NewLocalClient("app")
	if err != nil {
		t.Fatal(err)
	}
	return c, state
}

func TestBootstrapCreateAndRetry(t *testing.T) {
	c, state := newBootstrapFixture(t)
	it := bootstrapCommand()
	for i := 0; i < 2; i++ {
		name, err := EnsureBootstrapSecret(context.Background(), c, it)
		if err != nil || name != "cnpg-1-2-app" {
			t.Fatalf("bootstrap failed: name=%q err=%v", name, err)
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.writes != 1 || state.target.Namespace != "app-database" || state.target.Type != v1.SecretTypeBasicAuth || state.target.Immutable == nil || !*state.target.Immutable || len(state.target.Data) != 2 || string(state.target.Data[v1.BasicAuthUsernameKey]) != "app" || string(state.target.Data[v1.BasicAuthPasswordKey]) != bootstrapPassword {
		t.Fatal("bootstrap shape/namespace/idempotency incorrect")
	}
	if state.target.Labels["nortezh.io/database-id"] != "1" || state.target.Labels["nortezh.io/project-id"] != "2" || state.target.Labels["nortezh.io/secret-id"] != "3" || state.target.Labels["nortezh.io/secret-revision"] != "1" || state.target.Labels["nortezh.io/operation-id"] != it.OperationID {
		t.Fatal("bootstrap ownership missing")
	}
	if string(state.source.Data["secret-4"]) != "untouched-peer" || string(state.source.Data["secret-3"]) != bootstrapPassword {
		t.Fatal("aggregate changed")
	}
	metadata, err := json.Marshal(state.target.ObjectMeta)
	if err != nil || strings.Contains(string(metadata), bootstrapPassword) {
		t.Fatal("credential stored in metadata")
	}
	wire, err := json.Marshal(it)
	if err != nil || strings.Contains(string(wire), bootstrapPassword) {
		t.Fatal("credential stored in dedicated command")
	}
}

func TestBootstrapRejectsSourceBeforeWrite(t *testing.T) {
	for _, kind := range []string{"missing", "no marker", "wrong revision", "foreign project", "unowned", "wrong type", "no key", "empty key", "deleting", "controller owner", "denied"} {
		t.Run(kind, func(t *testing.T) {
			c, state := newBootstrapFixture(t)
			state.mu.Lock()
			switch kind {
			case "missing":
				state.source = nil
			case "no marker":
				state.source.Annotations = nil
			case "wrong revision":
				state.source.Annotations["nortezh.io/secret-revision-3"] = "2"
			case "foreign project":
				state.source.Labels["projectId"] = "9"
			case "unowned":
				state.source.Labels = nil
			case "wrong type":
				state.source.Type = v1.SecretTypeBasicAuth
			case "no key":
				delete(state.source.Data, "secret-3")
			case "empty key":
				state.source.Data["secret-3"] = nil
			case "deleting":
				now := metav1.Now()
				state.source.DeletionTimestamp = &now
			case "controller owner":
				state.source.OwnerReferences = []metav1.OwnerReference{{UID: "other"}}
			case "denied":
				state.denySource = true
			}
			state.mu.Unlock()
			name, err := EnsureBootstrapSecret(context.Background(), c, bootstrapCommand())
			if err == nil || name != "" || strings.Contains(err.Error(), bootstrapPassword) {
				t.Fatal("source accepted or raw credential error leaked")
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.writes != 0 || state.calls != 2 {
				t.Fatal("invalid source reached destination/write")
			}
		})
	}
}

func TestBootstrapRejectsExistingMismatchWithoutAdoption(t *testing.T) {
	for _, kind := range []string{"database", "project", "operation", "secret", "revision", "unowned", "type", "mutable", "username", "password", "extra data", "owner", "deleting", "denied"} {
		t.Run(kind, func(t *testing.T) {
			c, state := newBootstrapFixture(t)
			if _, err := EnsureBootstrapSecret(context.Background(), c, bootstrapCommand()); err != nil {
				t.Fatal(err)
			}
			state.mu.Lock()
			switch kind {
			case "database":
				state.target.Labels["nortezh.io/database-id"] = "9"
			case "project":
				state.target.Labels["nortezh.io/project-id"] = "9"
			case "operation":
				state.target.Labels["nortezh.io/operation-id"] = "b1111111-1111-4111-8111-111111111111"
			case "secret":
				state.target.Labels["nortezh.io/secret-id"] = "9"
			case "revision":
				state.target.Labels["nortezh.io/secret-revision"] = "2"
			case "unowned":
				state.target.Labels = nil
			case "type":
				state.target.Type = v1.SecretTypeOpaque
			case "mutable":
				state.target.Immutable = nil
			case "username":
				state.target.Data[v1.BasicAuthUsernameKey] = []byte("other")
			case "password":
				state.target.Data[v1.BasicAuthPasswordKey] = []byte("other")
			case "extra data":
				state.target.Data["unexpected"] = []byte("other")
			case "owner":
				state.target.OwnerReferences = []metav1.OwnerReference{{UID: "other"}}
			case "deleting":
				now := metav1.Now()
				state.target.DeletionTimestamp = &now
			case "denied":
				state.denyTarget = true
			}
			before := state.target.DeepCopy()
			state.writes = 0
			state.mu.Unlock()
			name, err := EnsureBootstrapSecret(context.Background(), c, bootstrapCommand())
			if err == nil || name != "" || strings.Contains(err.Error(), bootstrapPassword) {
				t.Fatal("mismatch adopted or credential leaked")
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.writes != 0 || !reflect.DeepEqual(before, state.target) {
				t.Fatal("existing object changed")
			}
		})
	}
}

func TestBootstrapAlreadyExistsRaceRechecksOwnership(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "same owner", true: "foreign owner"}[foreign], func(t *testing.T) {
			c, state := newBootstrapFixture(t)
			state.mu.Lock()
			state.race = func(s *v1.Secret) *v1.Secret {
				out := s.DeepCopy()
				if foreign {
					out.Labels["nortezh.io/database-id"] = "9"
				}
				return out
			}
			state.mu.Unlock()
			name, err := EnsureBootstrapSecret(context.Background(), c, bootstrapCommand())
			if foreign {
				if err == nil || name != "" {
					t.Fatal("race winner adopted")
				}
			} else if err != nil || name != "cnpg-1-2-app" {
				t.Fatal("owned race not retried")
			}
			state.mu.Lock()
			defer state.mu.Unlock()
			if state.writes != 1 {
				t.Fatal("race performed unexpected writes")
			}
		})
	}
}

func TestBootstrapInvalidCommandDoesNotAccessKubernetes(t *testing.T) {
	c, state := newBootstrapFixture(t)
	for _, kind := range []string{"nil", "version", "resource", "project", "operation", "secret", "revision", "name", "username"} {
		it := bootstrapCommand()
		switch kind {
		case "nil":
			it = nil
		case "version":
			it.Version = 2
		case "resource":
			it.ID = 0
		case "project":
			it.ProjectID = 0
		case "operation":
			it.OperationID = "bad"
		case "secret":
			it.SecretID = 0
		case "revision":
			it.SecretRevision = 0
		case "name":
			it.Name = "../foreign"
		case "username":
			it.Username = ""
		}
		if name, err := EnsureBootstrapSecret(context.Background(), c, it); err == nil || name != "" {
			t.Fatal("invalid command accepted")
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.calls != 0 || state.writes != 0 {
		t.Fatal("invalid command called Kubernetes")
	}
}
