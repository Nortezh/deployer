package cnpg

import (
	"context"
	"testing"
)

func TestBootstrapRetirementMarkerBlocksRecreation(t *testing.T) {
	c, state := newBootstrapFixture(t)
	state.retired = true
	if _, err := EnsureBootstrapSecret(context.Background(), c, bootstrapCommand()); err == nil {
		t.Fatal("retired identity recreated")
	}
	if state.calls != 1 || state.writes != 0 {
		t.Fatal("read credentials or wrote after retirement")
	}
}
