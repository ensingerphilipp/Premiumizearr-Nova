package service

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestContainerStartupPermissions(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container entrypoint requires Linux and bash")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join("..", "..", "scripts", "test-container-permissions"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("container permission regression: %v\n%s", err, output)
	}
}
