//go:build windows

// Package winpath validates paths from the perspective of the LocalSystem
// service, which is not the same as the user's perspective.
//
// The differences bite in practice: a mapped drive letter belongs to a logon
// session the service does not have, and a UNC path authenticates as the machine
// account rather than the user. Both fail in ways that look like a typo unless
// they are checked for explicitly.
package winpath

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// Drive types returned by GetDriveType. x/sys/windows does not export these.
const (
	driveNoRootDir = 1
	driveRemote    = 4

	deleteAccess            = 0x00010000
	fileRenameInformation   = 10
	fileRenameInformationEx = 65
	renameReplaceIfExist    = 1
	renamePOSIXSemantics    = 2
)

type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
}

// Mode says what the caller intends to do with the path.
type Mode int

const (
	// Read requires the path to exist and be readable.
	Read Mode = iota
	// Write requires the path to exist and be writable.
	Write
	// Create requires the parent directory to exist and be writable.
	Create
)

// FileDestination is a caller-writable staging file and its final destination.
// PrepareFileDestination and Commit must run while impersonating the pipe
// client, so Windows enforces that user's token rather than LocalSystem's.
type FileDestination struct {
	Path        string
	StagingPath string
	overwrite   bool
	stagingFile *os.File
}

// Validate checks that a path is usable from the service and returns it cleaned.
//
// createParents applies to Create only, and makes missing parent directories.
func Validate(path string, mode Mode, createParents bool) (string, error) {
	if path == "" {
		return "", hverr.New(hverr.InvalidArgument, "path is required")
	}
	if !filepath.IsAbs(path) {
		// A relative path resolves against the service's working directory,
		// which is %SystemRoot%\System32 — never what the caller meant.
		return "", hverr.New(hverr.InvalidArgument,
			"%q must be an absolute path", path)
	}

	clean := filepath.Clean(path)

	if err := checkVolume(clean); err != nil {
		return "", err
	}

	switch mode {
	case Read:
		if _, err := os.Stat(clean); err != nil {
			return "", hverr.Wrap(hverr.PathNotFound, err, "%s is not readable by the service", clean)
		}
	case Write:
		if _, err := os.Stat(clean); err != nil {
			return "", hverr.Wrap(hverr.PathNotFound, err, "%s does not exist", clean)
		}
		if err := checkWritable(filepath.Dir(clean)); err != nil {
			return "", err
		}
	case Create:
		parent := filepath.Dir(clean)
		if _, err := os.Stat(parent); err != nil {
			if !createParents {
				return "", hverr.New(hverr.PathNotFound,
					"%s does not exist", parent).
					WithDetail("pass create_parents to create it")
			}
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return "", hverr.Wrap(hverr.PathNotAccessible, err, "could not create %s", parent)
			}
		}
		if err := checkWritable(parent); err != nil {
			return "", err
		}
	}

	return clean, nil
}

// ValidateFileDestination validates a path that a caller will create or replace.
//
// The service runs as LocalSystem, so following a reparse point supplied by an
// unprivileged caller could redirect a write into a more privileged location.
// Reject every existing reparse point from the volume root through the leaf.
func ValidateFileDestination(path string, createParents bool) (string, error) {
	if path == "" {
		return Validate(path, Create, createParents)
	}
	if isDevicePath(path) {
		return "", hverr.New(hverr.InvalidArgument,
			"%q is a device path, not a filesystem path", path)
	}
	if !filepath.IsAbs(path) {
		return Validate(path, Create, createParents)
	}
	clean := filepath.Clean(path)
	if err := checkVolume(clean); err != nil {
		return "", err
	}
	// Check before Validate performs its write-access probe, then again after
	// it creates any requested parents.
	if err := rejectReparsePoints(clean); err != nil {
		return "", err
	}
	clean, err := Validate(clean, Create, createParents)
	if err != nil {
		return "", err
	}
	if err := rejectReparsePoints(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// PrepareFileDestination validates a final path and creates an empty staging
// file beside it. Keeping the staging file in the same directory makes the
// final move stay on one volume.
func PrepareFileDestination(path string, createParents, overwrite bool) (*FileDestination, error) {
	clean, err := ValidateFileDestination(path, createParents)
	if err != nil {
		return nil, err
	}

	if err := inspectDestination(clean, overwrite); err != nil {
		return nil, err
	}
	f, err := createRenameableTemp(filepath.Dir(clean))
	if err != nil {
		return nil, hverr.Wrap(hverr.PathNotAccessible, err,
			"could not create a staging file in %s", filepath.Dir(clean))
	}
	return &FileDestination{
		Path:        clean,
		StagingPath: f.Name(),
		overwrite:   overwrite,
		stagingFile: f,
	}, nil
}

// CopyFrom fills the caller-created staging file through its retained handle.
// It must run while impersonating the pipe client.
func (d *FileDestination) CopyFrom(ctx context.Context, src io.Reader, expectedSize int64, expectedSHA256 string) error {
	if d.stagingFile == nil {
		return hverr.New(hverr.Internal, "destination staging file is not open")
	}
	stopCancel := make(chan struct{})
	go func(f *os.File) {
		select {
		case <-ctx.Done():
			_ = f.Close()
		case <-stopCancel:
		}
	}(d.stagingFile)
	defer close(stopCancel)

	written, err := copyWithContext(ctx, d.stagingFile, src)
	if err != nil {
		d.stagingFile.Close()
		d.stagingFile = nil
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not write staging file for %s", d.Path)
	}
	if written != expectedSize {
		d.stagingFile.Close()
		d.stagingFile = nil
		return hverr.New(hverr.Internal,
			"the final host staging file did not match the guest size")
	}
	if err := d.stagingFile.Sync(); err != nil {
		d.stagingFile.Close()
		d.stagingFile = nil
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not flush staging file for %s", d.Path)
	}
	if _, err := d.stagingFile.Seek(0, io.SeekStart); err != nil {
		d.stagingFile.Close()
		d.stagingFile = nil
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not verify staging file for %s", d.Path)
	}
	hash := sha256.New()
	verifiedSize, err := copyWithContext(ctx, hash, d.stagingFile)
	if err != nil {
		d.stagingFile.Close()
		d.stagingFile = nil
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not verify staging file for %s", d.Path)
	}
	if verifiedSize != expectedSize || hex.EncodeToString(hash.Sum(nil)) != expectedSHA256 {
		d.stagingFile.Close()
		d.stagingFile = nil
		return hverr.New(hverr.Internal,
			"the final host staging file did not match the guest size and SHA256")
	}
	if err := ctx.Err(); err != nil {
		d.stagingFile.Close()
		d.stagingFile = nil
		return err
	}
	return nil
}

// Cleanup closes and removes an uncommitted staging file. It must run while
// impersonating the pipe client.
func (d *FileDestination) Cleanup() error {
	if d.stagingFile != nil {
		_ = d.stagingFile.Close()
		d.stagingFile = nil
	}
	err := os.Remove(d.StagingPath)
	if err != nil && !os.IsNotExist(err) {
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not remove staging file for %s", d.Path)
	}
	return nil
}

// Commit revalidates the destination and moves the staged file into place.
func (d *FileDestination) Commit(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.stagingFile == nil {
		return hverr.New(hverr.Internal, "destination staging file is not open")
	}
	if _, err := ValidateFileDestination(d.Path, false); err != nil {
		return err
	}
	if err := inspectDestination(d.Path, d.overwrite); err != nil {
		return err
	}

	if err := renameOpenFile(d.stagingFile, d.Path, d.overwrite); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return hverr.New(hverr.VMAlreadyExists,
				"%s already exists; pass overwrite to replace it", d.Path)
		}
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not move the copied file to %s", d.Path)
	}
	if err := d.stagingFile.Close(); err != nil {
		d.stagingFile = nil
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not close the copied file at %s", d.Path)
	}
	d.stagingFile = nil
	return nil
}

func inspectDestination(path string, overwrite bool) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return hverr.New(hverr.InvalidArgument,
				"%s is a directory; destination_path must name a file", path)
		}

		if !overwrite {
			return hverr.New(hverr.VMAlreadyExists,
				"%s already exists; pass overwrite to replace it", path)
		}
		return nil
	case os.IsNotExist(err):
		return nil
	default:
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not inspect %s", path)
	}
}

func createRenameableTemp(dir string) (*os.File, error) {
	for range 100 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, fmt.Sprintf(".hypervm-mcp-copy-%x.tmp", random))
		ptr, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		handle, err := windows.CreateFile(
			ptr,
			windows.GENERIC_READ|windows.GENERIC_WRITE|deleteAccess,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE,
			nil,
			windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(handle), path), nil
	}
	return nil, windows.ERROR_FILE_EXISTS
}

func renameOpenFile(file *os.File, destination string, overwrite bool) error {
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()

	name, err := windows.UTF16FromString(filepath.Base(destination))
	if err != nil {
		return err
	}
	name = name[:len(name)-1]

	var layout fileRenameInfo
	nameOffset := unsafe.Offsetof(layout.FileNameLength) + unsafe.Sizeof(layout.FileNameLength)
	buf := make([]byte, int(nameOffset)+len(name)*2)
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.RootDirectory = windows.Handle(dir.Fd())
	if overwrite {
		info.Flags = renameReplaceIfExist | renamePOSIXSemantics
	}
	info.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[nameOffset])), len(name)), name)

	var status windows.IO_STATUS_BLOCK
	err = windows.NtSetInformationFile(
		windows.Handle(file.Fd()),
		&status,
		&buf[0],
		uint32(len(buf)),
		fileRenameInformationEx,
	)
	if err == nil {
		return nil
	}

	if overwrite {
		info.Flags = renameReplaceIfExist
	} else {
		info.Flags = 0
	}
	err = windows.NtSetInformationFile(
		windows.Handle(file.Fd()),
		&status,
		&buf[0],
		uint32(len(buf)),
		fileRenameInformation,
	)
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}

// ValidateDir is Validate for a directory the caller wants to write into.
func ValidateDir(path string, createParents bool) (string, error) {
	if path == "" {
		return "", hverr.New(hverr.InvalidArgument, "path is required")
	}
	if !filepath.IsAbs(path) {
		return "", hverr.New(hverr.InvalidArgument, "%q must be an absolute path", path)
	}

	clean := filepath.Clean(path)

	if err := checkVolume(clean); err != nil {
		return "", err
	}
	if _, err := os.Stat(clean); err != nil {
		if !createParents {
			return "", hverr.New(hverr.PathNotFound, "%s does not exist", clean).
				WithDetail("pass create_parents to create it")
		}
		if err := os.MkdirAll(clean, 0o755); err != nil {
			return "", hverr.Wrap(hverr.PathNotAccessible, err, "could not create %s", clean)
		}
	}
	if err := checkWritable(clean); err != nil {
		return "", err
	}
	return clean, nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 128*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		nr, readErr := src.Read(buf)
		if nr > 0 {
			if err := ctx.Err(); err != nil {
				return written, err
			}
			nw, writeErr := dst.Write(buf[:nr])
			written += int64(nw)
			if writeErr != nil {
				return written, writeErr
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func isDevicePath(path string) bool {
	p := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	return strings.HasPrefix(p, `\\?\`) ||
		strings.HasPrefix(p, `\\.\`) ||
		strings.HasPrefix(p, `\??\`)
}

func rejectReparsePoints(path string) error {
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	current := volume + string(os.PathSeparator)

	for _, part := range strings.FieldsFunc(rest, func(r rune) bool {
		return r == '\\' || r == '/'
	}) {
		current = filepath.Join(current, part)
		ptr, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return hverr.Wrap(hverr.InvalidArgument, err, "invalid path %q", path)
		}
		attrs, err := windows.GetFileAttributes(ptr)
		if err != nil {
			if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
				errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
				continue
			}
			return hverr.Wrap(hverr.PathNotAccessible, err,
				"could not inspect %s", current)
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return hverr.New(hverr.PathNotAccessible,
				"%s is a reparse point and cannot be used as a service write destination", current)
		}
	}
	return nil
}

// checkVolume rejects the two path shapes that fail for reasons specific to
// running as a service, with an explanation of what to use instead.
func checkVolume(path string) error {
	if strings.HasPrefix(path, `\\`) {
		// Reaching a share at all proves the machine account has access; a
		// permission failure here is the whole point of checking.
		root := uncShareRoot(path)
		if _, err := os.Stat(root); err != nil {
			return hverr.Wrap(hverr.PathNotAccessible, err,
				"the service cannot reach %s", root).
				WithDetail("A service running as LocalSystem authenticates to network " +
					"shares as the computer account. Grant this machine's account " +
					"(HOSTNAME$) access to the share.")
		}
		return nil
	}

	if len(path) >= 2 && path[1] == ':' {
		root := path[:2] + `\`
		ptr, err := windows.UTF16PtrFromString(root)
		if err != nil {
			return hverr.Wrap(hverr.InvalidArgument, err, "invalid path %q", path)
		}
		switch windows.GetDriveType(ptr) {
		case driveRemote:
			return hverr.New(hverr.PathNotAccessible,
				"%s is a mapped network drive, which the service cannot see", root).
				WithDetail("Drive mappings belong to a logon session, and the service " +
					"has its own. Use the UNC path (\\\\server\\share\\...) instead.")
		case driveNoRootDir:
			return hverr.New(hverr.PathNotFound, "drive %s does not exist", root)
		}
	}
	return nil
}

// checkWritable proves write access rather than inferring it from an ACL, which
// is both simpler and accounts for read-only media and full volumes.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".hypervm-mcp-write-check-*")
	if err != nil {
		return hverr.Wrap(hverr.PathNotAccessible, err, "the service cannot write to %s", dir)
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return nil
}

// uncShareRoot trims \\server\share\a\b down to \\server\share.
func uncShareRoot(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, `\\`), `\`)
	if len(parts) >= 2 {
		return `\\` + parts[0] + `\` + parts[1]
	}
	return path
}
