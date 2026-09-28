//go:build windows

package winpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

func TestValidateFileDestinationCreatesParents(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "one", "two", "file.txt")

	if _, err := ValidateFileDestination(dest, false); !hverr.Is(err, hverr.PathNotFound) {
		t.Fatalf("without createParents got %v, want PATH_NOT_FOUND", err)
	}
	got, err := ValidateFileDestination(dest, true)
	if err != nil {
		t.Fatalf("with createParents: %v", err)
	}
	if got != filepath.Clean(dest) {
		t.Fatalf("got %q, want %q", got, filepath.Clean(dest))
	}
}

func TestValidateFileDestinationRejectsDevicePaths(t *testing.T) {
	for _, path := range []string{
		`\\?\C:\Windows\Temp\probe.txt`,
		`\\.\C:\Windows\Temp\probe.txt`,
		`\??\C:\Windows\Temp\probe.txt`,
	} {
		t.Run(path[:4], func(t *testing.T) {
			if _, err := ValidateFileDestination(path, false); !hverr.Is(err, hverr.InvalidArgument) {
				t.Fatalf("got %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}

func TestValidateFileDestinationRejectsReparseParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("creating a directory symlink is unavailable: %v", err)
	}

	_, err := ValidateFileDestination(filepath.Join(link, "file.txt"), false)
	if !hverr.Is(err, hverr.PathNotAccessible) {
		t.Fatalf("got %v, want PATH_NOT_ACCESSIBLE", err)
	}
}

func TestPrepareAndCommitFileDestination(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "new", "file.txt")
	target, err := PrepareFileDestination(dest, true, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Cleanup(func() { os.Remove(target.StagingPath) })
	if target.Existed {
		t.Fatal("a new destination reported that it existed")
	}
	if err := target.CopyFrom(strings.NewReader("first")); err != nil {
		t.Fatalf("copy first: %v", err)
	}
	if err := target.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "first" {
		t.Fatalf("read committed file: got %q err=%v", got, err)
	}

	if _, err := PrepareFileDestination(dest, false, false); !hverr.Is(err, hverr.VMAlreadyExists) {
		t.Fatalf("without overwrite got %v, want VM_ALREADY_EXISTS", err)
	}
	replacement, err := PrepareFileDestination(dest, false, true)
	if err != nil {
		t.Fatalf("prepare overwrite: %v", err)
	}
	t.Cleanup(func() { os.Remove(replacement.StagingPath) })
	if !replacement.Existed {
		t.Fatal("overwrite destination did not report that it existed")
	}
	if err := replacement.CopyFrom(strings.NewReader("second")); err != nil {
		t.Fatalf("copy replacement: %v", err)
	}
	if err := replacement.Commit(); err != nil {
		t.Fatalf("commit overwrite: %v", err)
	}
	got, err = os.ReadFile(dest)
	if err != nil || string(got) != "second" {
		t.Fatalf("read overwritten file: got %q err=%v", got, err)
	}
}
