package cnpg

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type r2FixtureTransport struct {
	original http.RoundTripper
	endpoint *url.URL
}

func (r r2FixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Hostname() != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.r2.cloudflarestorage.com" {
		return r.original.RoundTrip(request)
	}
	clone := request.Clone(request.Context())
	u := *request.URL
	u.Scheme = r.endpoint.Scheme
	u.Host = r.endpoint.Host
	clone.URL = &u
	return r.original.RoundTrip(clone)
}
func newR2DeleteFixture(t *testing.T) (*deleteFixture, Profile) {
	t.Helper()
	_, f := newDeleteFixture(t)
	store := f.objects["/apis/barmancloud.cnpg.io/v1/namespaces/app-database/objectstores/backup-store"]
	old, _, err := unstructured.NestedString(store, "spec", "configuration", "endpointURL")
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(old)
	if err != nil || endpoint.Scheme != "http" || !strings.HasPrefix(endpoint.Host, "127.0.0.1:") {
		t.Fatal("unexpected fake S3 endpoint")
	}
	original := http.DefaultTransport
	http.DefaultTransport = r2FixtureTransport{original, endpoint}
	t.Cleanup(func() { http.DefaultTransport = original })
	if err = unstructured.SetNestedField(store, "https://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.r2.cloudflarestorage.com", "spec", "configuration", "endpointURL"); err != nil {
		t.Fatal(err)
	}
	p := deleteProfile()
	p.ArchiveDeleteProvider = deleteR2Profile
	bindDeleteFixture(t, f, p)
	return f, p
}
func TestR2DeleteProviderEndpoint(t *testing.T) {
	p := deleteProfile()
	p.ArchiveDeleteProvider = deleteR2Profile
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.r2.cloudflarestorage.com", true},
		{"https://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.eu.r2.cloudflarestorage.com", true},
		{"http://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.r2.cloudflarestorage.com", false},
		{"https://attacker.example", false},
		{"https://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.r2.cloudflarestorage.com.attacker.example", false},
		{"https://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.r2.cloudflarestorage.com:444", false},
	} {
		if got := supportedDeleteProvider(p, tc.url); got != tc.ok {
			t.Errorf("endpoint check %q got %v", tc.url, got)
		}
	}
}
func TestR2DeleteIsDisabledWithoutExplicitProfile(t *testing.T) {
	for _, profile := range []string{"", deleteRustFSProfile} {
		t.Run(profile, func(t *testing.T) {
			f, p := newR2DeleteFixture(t)
			p.ArchiveDeleteProvider = profile
			if done, err := ApplyDelete(context.Background(), f.client, deleteCommand(), p); done || err == nil || f.stops != 0 || f.archive.deletes != 0 {
				t.Fatal("unselected or misidentified R2 profile advanced deletion")
			}
		})
	}
}

func TestR2DeletePartialSameOperation(t *testing.T) {
	f, p := newR2DeleteFixture(t)
	f.archive.pageSize = 1
	f.archive.failBatch = 2
	if done, err := ApplyDelete(context.Background(), f.client, deleteCommand(), p); done || err == nil || f.stops != 1 || len(f.deletes) != 0 {
		t.Fatal("partial R2 delete not blocked")
	}
	f.archive.failBatch = 0
	if done, err := ApplyDelete(context.Background(), f.client, deleteCommand(), p); done || err != nil || f.stops != 1 {
		t.Fatal("same R2 operation did not resume", err)
	}
	if f.archive.lifecycleReads != 0 {
		t.Fatal("R2 retry required lifecycle-read permission")
	}
	if f.archive.objects["staging/source/wals/control"] != "source" || f.archive.objects["staging/cnpg-1-2-neighbor/wals/control"] != "neighbor" {
		t.Fatal("unrelated data changed")
	}
}

func TestR2DeleteCalledPathWithoutLifecyclePermission(t *testing.T) {
	for _, mode := range []string{"allowed", "per-key", "list"} {
		t.Run(mode, func(t *testing.T) {
			f, p := newR2DeleteFixture(t)
			if mode != "allowed" {
				f.archive.failure = mode
			}
			done, err := ApplyDelete(context.Background(), f.client, deleteCommand(), p)
			if mode == "allowed" {
				if err != nil || done || f.stops != 1 || f.archive.deletes == 0 {
					t.Fatalf("R2 called path not reached: %v", err)
				}
			} else if err == nil || done || len(f.deletes) != 0 {
				t.Fatal("archive error advanced native retirement")
			}
			if f.archive.lifecycleReads != 0 {
				t.Fatal("R2 Delete required bucket lifecycle-read permission")
			}
			if f.archive.objects["staging/source/wals/control"] != "source" || f.archive.objects["staging/cnpg-1-2-neighbor/wals/control"] != "neighbor" {
				t.Fatal("source or neighbor changed")
			}
		})
	}
}
