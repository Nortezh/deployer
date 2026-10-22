package k8s

import (
	"reflect"
	"testing"
)

func TestPodLabels(t *testing.T) {
	base := map[string]string{"id": "web-1", "projectId": "1"}

	if got := podLabels(base, nil); !reflect.DeepEqual(got, base) {
		t.Fatalf("no extra: got %v", got)
	}

	got := podLabels(base, map[string]string{"id": "other", "seer.io/app_id": "web"})
	want := map[string]string{"id": "web-1", "projectId": "1", "seer.io/app_id": "web"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(base) != 2 {
		t.Fatalf("base was modified: %v", base)
	}
}
