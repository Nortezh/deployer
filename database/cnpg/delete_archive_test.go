package cnpg

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

type archiveFixture struct {
	mu                                     sync.Mutex
	objects                                map[string]string
	deletes, lists                         int
	versioning, denied, malformed, failure string
	lifecycle                              string
	lock, replication                      bool
	failBatch                              int
	pageSize                               int
	injectLate                             bool
}

func newArchiveFixture(t *testing.T) (*archiveFixture, string) {
	t.Helper()
	f := &archiveFixture{objects: map[string]string{"staging/cnpg-1-2/wals/one": "wal1", "staging/cnpg-1-2/base/two": "backup", "staging/cnpg-1-2-neighbor/wals/control": "neighbor", "staging/source/wals/control": "source"}, pageSize: 2}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		errReply := func(code, message string) {
			w.WriteHeader(400)
			_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, message)
		}
		query := r.URL.Query()
		if query.Has("lifecycle") {
			if f.lifecycle == "missing" {
				_, _ = fmt.Fprint(w, "<LifecycleConfiguration></LifecycleConfiguration>")
				return
			}
			if f.lifecycle == "denied" {
				errReply("AccessDenied", "unavailable")
				return
			}
			if f.lifecycle == "tagged" {
				_, _ = fmt.Fprint(w, "<LifecycleConfiguration><Rule><Filter><Tag><Key>type</Key><Value>private</Value></Tag></Filter><Status>Enabled</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule></LifecycleConfiguration>")
				return
			}
			status, prefix, days := "Enabled", "", "7"
			if f.lifecycle == "disabled" {
				status = "Disabled"
			}
			if f.lifecycle == "foreign" {
				prefix = "other/"
			}
			if f.lifecycle == "invalid" {
				days = "0"
			}
			_, _ = fmt.Fprintf(w, "<LifecycleConfiguration><Rule><ID>incomplete-multipart</ID><Filter><Prefix>%s</Prefix></Filter><Status>%s</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>%s</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule></LifecycleConfiguration>", prefix, status, days)
			return
		}
		for _, feature := range []string{"versioning", "object-lock", "replication"} {
			if !query.Has(feature) {
				continue
			}
			if f.denied == feature {
				errReply("AccessDenied", "private provider details")
				return
			}
			switch feature {
			case "versioning":
				_, _ = fmt.Fprintf(w, "<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>", f.versioning)
			case "object-lock":
				if f.lock {
					_, _ = fmt.Fprint(w, "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>")
				} else {
					errReply("ObjectLockConfigurationNotFoundError", "absent")
				}
			case "replication":
				if f.replication {
					_, _ = fmt.Fprint(w, "<ReplicationConfiguration><Role>foreign</Role></ReplicationConfiguration>")
				} else {
					errReply("ReplicationConfigurationNotFoundError", "absent")
				}
			}
			return
		}
		if query.Get("list-type") == "2" {
			f.lists++
			if f.lifecycle == "drift" {
				f.lifecycle = "denied"
			}
			prefix, token := query.Get("prefix"), query.Get("continuation-token")
			if prefix != "staging/cnpg-1-2/" {
				t.Error("unbound prefix", prefix)
			}
			if f.failure == "list" {
				errReply("AccessDenied", "private listing")
				return
			}
			var keys []string
			for key := range f.objects {
				if strings.HasPrefix(key, prefix) && key > token {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			truncated := len(keys) > f.pageSize
			if truncated {
				keys = keys[:f.pageSize]
			}
			_, _ = fmt.Fprintf(w, "<ListBucketResult><Name>cnpg-backups</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><IsTruncated>%t</IsTruncated>", prefix, len(keys), truncated)
			for _, key := range keys {
				_, _ = fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>1</Size></Contents>", key)
			}
			if f.malformed == "foreign key" {
				_, _ = fmt.Fprint(w, "<Contents><Key>staging/source/wals/control</Key></Contents>")
			}
			if truncated && f.malformed != "missing token" {
				_, _ = fmt.Fprintf(w, "<NextContinuationToken>%s</NextContinuationToken>", keys[len(keys)-1])
			}
			_, _ = fmt.Fprint(w, "</ListBucketResult>")
			return
		}
		if r.Method == http.MethodPost && query.Has("delete") {
			f.deletes++
			var input struct {
				Objects []struct {
					Key string `xml:"Key"`
				} `xml:"Object"`
			}
			if err := xml.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				errReply("InvalidRequest", "bad input")
				return
			}
			_, _ = fmt.Fprint(w, "<DeleteResult>")
			for _, object := range input.Objects {
				if !strings.HasPrefix(object.Key, "staging/cnpg-1-2/") {
					t.Error("foreign object mutation", object.Key)
				}
				if f.failBatch == f.deletes || f.failure == "per-key" {
					_, _ = fmt.Fprintf(w, "<Error><Key>%s</Key><Code>AccessDenied</Code><Message>private details</Message></Error>", object.Key)
				} else {
					delete(f.objects, object.Key)
					_, _ = fmt.Fprintf(w, "<Deleted><Key>%s</Key></Deleted>", object.Key)
				}
			}
			if f.injectLate {
				f.objects["staging/cnpg-1-2/000-late"] = "late"
				f.injectLate = false
			}
			_, _ = fmt.Fprint(w, "</DeleteResult>")
			return
		}
		t.Error("unexpected provider request", r.Method, r.URL.Path, r.URL.RawQuery)
		errReply("InvalidRequest", "unsupported")
	}))
	t.Cleanup(server.Close)
	return f, server.URL
}

func TestDeleteFreshAbsencePassFindsLateCommittedObject(t *testing.T) {
	c, f := newDeleteFixture(t)
	f.archive.injectLate = true
	if done, err := ApplyDelete(context.Background(), c, deleteCommand(), deleteProfile()); done || err == nil || len(f.deletes) != 0 {
		t.Fatal("absence pass missed a committed object", err)
	}
	if f.archive.objects["staging/cnpg-1-2/000-late"] == "" {
		t.Fatal("test did not introduce late object")
	}
	if done, err := ApplyDelete(context.Background(), c, deleteCommand(), deleteProfile()); done || err != nil || f.stops != 1 {
		t.Fatal("same operation did not resume archive cleanup", err)
	}
	if f.archive.objects["staging/cnpg-1-2/000-late"] != "" {
		t.Fatal("retry left committed target object")
	}
}
