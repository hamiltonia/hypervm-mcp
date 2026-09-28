//go:build windows

package winpath

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"golang.org/x/sys/windows"
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

func TestSharedValidateDoesNotClassifyDevicePaths(t *testing.T) {
	_, err := Validate(`\\?\C:\does-not-exist`, Read, false)
	if hverr.Is(err, hverr.InvalidArgument) {
		t.Fatalf("shared path validation applied guest_copy_from device-path policy: %v", err)
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
	t.Cleanup(func() { _ = target.Cleanup() })
	if err := copyText(target, "first"); err != nil {
		t.Fatalf("copy first: %v", err)
	}
	if err := target.Commit(context.Background()); err != nil {
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
	t.Cleanup(func() { _ = replacement.Cleanup() })
	if err := copyText(replacement, "second"); err != nil {
		t.Fatalf("copy replacement: %v", err)
	}
	if err := replacement.Commit(context.Background()); err != nil {
		t.Fatalf("commit overwrite: %v", err)
	}
	got, err = os.ReadFile(dest)
	if err != nil || string(got) != "second" {
		t.Fatalf("read overwritten file: got %q err=%v", got, err)
	}
}

func TestCopyFromHonorsCancellation(t *testing.T) {
	target, err := PrepareFileDestination(filepath.Join(t.TempDir(), "file.txt"), false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Cleanup() })

	ctx, cancel := context.WithCancel(context.Background())
	src := io.MultiReader(strings.NewReader("first"), cancelingReader{cancel: cancel})
	err = target.CopyFrom(ctx, src, 6,
		fmt.Sprintf("%x", sha256.Sum256([]byte("firstx"))))
	if err != context.Canceled {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestCopyFromVerifiesFinalStage(t *testing.T) {
	target, err := PrepareFileDestination(filepath.Join(t.TempDir(), "file.txt"), false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Cleanup() })

	err = target.CopyFrom(context.Background(), strings.NewReader("content"), 7,
		strings.Repeat("0", sha256.Size*2))
	if !hverr.Is(err, hverr.Internal) {
		t.Fatalf("got %v, want INTERNAL integrity error", err)
	}
}

func TestCopyFromLargeFile(t *testing.T) {
	content := bytes.Repeat([]byte("0123456789abcdef"), 128*1024)
	dest := filepath.Join(t.TempDir(), "large.bin")
	target, err := PrepareFileDestination(dest, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Cleanup() })

	sum := sha256.Sum256(content)
	if err := target.CopyFrom(context.Background(), bytes.NewReader(content),
		int64(len(content)), fmt.Sprintf("%x", sum)); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := target.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("large copy differs: got %d bytes, want %d", len(got), len(content))
	}
}

func TestCommitOverwritesFileCreatedAfterPrepare(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "file.txt")
	target, err := PrepareFileDestination(dest, false, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Cleanup() })
	if err := copyText(target, "guest"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("racer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := target.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "guest" {
		t.Fatalf("got %q err=%v, want guest", got, err)
	}
}

func TestCommitRenamesVerifiedHandleAfterStagingPathReplacement(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "destination.txt")
	target, err := PrepareFileDestination(dest, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Cleanup() })
	if err := copyText(target, "verified"); err != nil {
		t.Fatal(err)
	}

	displaced := filepath.Join(root, "displaced.tmp")
	if err := os.Rename(target.StagingPath, displaced); err != nil {
		t.Fatalf("displace staging path: %v", err)
	}
	if err := os.WriteFile(target.StagingPath, []byte("replacement"), 0o644); err != nil {
		t.Fatalf("replace staging path: %v", err)
	}

	if err := target.Commit(context.Background()); err != nil {
		t.Fatalf("commit verified handle: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "verified" {
		t.Fatalf("destination got %q err=%v, want verified", got, err)
	}
	replacement, err := os.ReadFile(target.StagingPath)
	if err != nil || string(replacement) != "replacement" {
		t.Fatalf("replacement path got %q err=%v", replacement, err)
	}
}

func TestStagingFileRejectsSecondWritableHandle(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "destination.txt")
	target, err := PrepareFileDestination(dest, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Cleanup() })

	path, err := windows.UTF16PtrFromString(target.StagingPath)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("second writable handle unexpectedly opened")
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("second writable open got %v, want sharing violation", err)
	}
}

func copyText(target *FileDestination, content string) error {
	sum := sha256.Sum256([]byte(content))
	return target.CopyFrom(context.Background(), strings.NewReader(content),
		int64(len(content)), fmt.Sprintf("%x", sum))
}

type cancelingReader struct {
	cancel context.CancelFunc
}

func (r cancelingReader) Read(p []byte) (int, error) {
	r.cancel()
	p[0] = 'x'
	return 1, nil
}
