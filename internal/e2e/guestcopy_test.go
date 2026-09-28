//go:build windows

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGuestCopyFile copies a file into the guest over the VMBus rather than the
// network.
//
// This path matters because it needs no guest address at all — it is how you get
// something into a VM that has no network yet. It does need the Guest Service
// Interface component running inside the guest, which on Linux is hypervfcopyd
// from hyperv-daemons; without it the test skips rather than reporting a failure
// that is really about the guest's configuration.
func TestGuestCopyFile(t *testing.T) {
	requireRocky(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host := os.Getenv(guestHostEnv)

	// The daemon is udev-activated and static, so a guest that gained the package
	// after its last boot will not have it running yet.
	sshRun(t, session, ctx, host, "sudo systemctl start hypervfcopyd 2>&1 || true")
	state := sshRun(t, session, ctx, host, "systemctl is-active hypervfcopyd || true")
	if !strings.Contains(state, "active") {
		t.Skipf("hypervfcopyd is %q in the guest, so the Guest Service Interface is unavailable", state)
	}

	// Write a file on the host for the service to pick up. It reads as
	// LocalSystem, so it goes somewhere LocalSystem can certainly read.
	hostDir := rockyArtifact
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatalf("create %s: %v", hostDir, err)
	}
	src := filepath.Join(hostDir, "guest-copy-probe.txt")
	content := "copied over the VMBus at " + time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", src, err)
	}

	const dest = "/var/tmp/hypervm-mcp/guest-copy-probe.txt"
	sshRun(t, session, ctx, host, "sudo rm -rf /var/tmp/hypervm-mcp")

	if err := tryCall(t, session, ctx, "guest_copy_file", map[string]any{
		"vm_name":          rockyVM,
		"source_path":      src,
		"destination_path": dest,
		"create_full_path": true,
		"overwrite":        true,
	}); err != nil {
		// A kernel that dropped the fcopy device is an environment limit, not a
		// defect here — but the error has to say which, and this checks it does.
		if strings.Contains(err.Error(), "GUEST_SERVICE_UNAVAILABLE") {
			if !strings.Contains(err.Error(), "hv_fcopy") {
				t.Fatalf("the error does not explain the cause: %v", err)
			}
			t.Skipf("this guest's kernel has no file-copy device: %v", err)
		}
		t.Fatalf("guest_copy_file: %v", err)
	}

	// Read it back over SSH because guest_copy_from is Windows-only.
	got := sshRun(t, session, ctx, host, "cat "+dest)
	if got != content {
		t.Fatalf("the guest has %q, want %q", got, content)
	}
	t.Logf("file arrived intact at %s", dest)

	sshRun(t, session, ctx, host, "sudo rm -rf /var/tmp/hypervm-mcp")
}
