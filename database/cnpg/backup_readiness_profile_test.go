package cnpg

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBackupReadinessProfileFingerprint(t *testing.T) {
	f, c, p := newReadinessFixture(t, time.Now().Add(-time.Minute).UnixMicro())
	hash, _, _, ok := readinessStore(context.Background(), c, p, f.store)
	if !ok || hash != "31d1f4968adf114a706f5bfd2e07083248082ad6bd76cf2f198531e10e91db73" {
		t.Fatal("v1 fingerprint changed", hash)
	}
	// Metadata identity changes invalidate the digest, but values never enter it.
	f.credentials.Data["ACCESS_SECRET_KEY"] = []byte("different-private-value")
	same, _, _, ok := readinessStore(context.Background(), c, p, f.store)
	if !ok || same != hash {
		t.Fatal("credential value entered fingerprint")
	}
	f.credentials.ResourceVersion = "8"
	changed, _, _, ok := readinessStore(context.Background(), c, p, f.store)
	if !ok || changed == hash {
		t.Fatal("credential revision did not invalidate fingerprint")
	}
}

func TestBackupReadinessConfigurationTimestamps(t *testing.T) {
	f, c, p := newReadinessFixture(t, time.Now().Add(-time.Minute).UnixMicro())
	updated := metav1.NewTime(time.Now().Add(-30 * time.Second).UTC().Truncate(time.Second))
	f.credentials.Immutable = nil
	f.credentials.ManagedFields = []metav1.ManagedFieldsEntry{{Time: &updated, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:data":{}}`)}}}
	_, _, since, ok := readinessStore(context.Background(), c, p, f.store)
	if !ok || !since.Equal(updated.Time) {
		t.Fatal("credential update was not included in archive lower bound")
	}
	r, err := ObserveBackupReadiness(context.Background(), c, readinessCommand(), p)
	if err != nil || r == nil || r.Available {
		t.Fatal("pre-credential-update WAL was reused", err)
	}
	f.credentials.ManagedFields[0].Time = nil
	if _, _, _, ok := readinessStore(context.Background(), c, p, f.store); ok {
		t.Fatal("unknown update time accepted")
	}
}
