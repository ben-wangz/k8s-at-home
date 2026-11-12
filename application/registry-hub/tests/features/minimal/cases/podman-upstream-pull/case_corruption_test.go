package podmanpull

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestPodmanRejectsValidlyEncodedBlobWithWrongDigest(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := verifyPodmanRejectsCorruptLayer(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
