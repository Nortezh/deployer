package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Nortezh/api"
	"github.com/deploys-app/deployer/k8s"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWorkerSecretUpsertRevisionAndRedaction(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "denied redaction"}[denied], func(t *testing.T) {
			const password = "fixture-worker-secret-never-log"
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: "NotFound", Code: 404})
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/app/secrets" {
					t.Error("unexpected request")
					w.WriteHeader(403)
					return
				}
				if denied {
					w.WriteHeader(403)
					_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: "Forbidden", Code: 403, Message: password})
					return
				}
				var s v1.Secret
				if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
					t.Error("bad request")
					return
				}
				if s.Name != "secrets-2" || s.Annotations["nortezh.io/secret-revision-3"] != "7" || string(s.Data["secret-3"]) != password {
					t.Error("wire revision/value not passed to key sync")
				}
				s.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
				w.WriteHeader(201)
				_ = json.NewEncoder(w).Encode(s)
			}))
			defer server.Close()
			t.Setenv("KUBE_PROXY_URL", server.URL)
			c, err := k8s.NewLocalClient("app")
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(old)
			worker := Worker{Client: c}
			worker.secretUpsert(context.Background(), &api.DeployerCommandSecretUpsert{SecretID: 3, ProjectID: 2, Name: "secret-3", Revision: 7, Value: password})
			if calls.Load() != 2 || len(worker.results) != 1 || worker.results[0].SecretUpsert.Success == denied {
				t.Fatal("unexpected sync result")
			}
			result, err := json.Marshal(worker.results)
			if err != nil || strings.Contains(string(result), password) || strings.Contains(logs.String(), password) {
				t.Fatal("credential leaked in result/logs")
			}
		})
	}
}

func TestWorkerSecretUpsertInvalidIdentityDoesNotCallKubernetes(t *testing.T) {
	for _, kind := range []string{"secret", "project", "revision", "key"} {
		it := &api.DeployerCommandSecretUpsert{SecretID: 3, ProjectID: 2, Name: "secret-3", Revision: 1, Value: "fixture"}
		switch kind {
		case "secret":
			it.SecretID = 0
		case "project":
			it.ProjectID = 0
		case "revision":
			it.Revision = 0
		case "key":
			it.Name = "secret-4"
		}
		// Nil client would panic if an invalid command reached Kubernetes.
		worker := Worker{}
		worker.secretUpsert(context.Background(), it)
		if len(worker.results) != 1 || worker.results[0].SecretUpsert.Success {
			t.Fatal("invalid sync acknowledged")
		}
	}
}
