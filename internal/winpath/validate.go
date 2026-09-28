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
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
)

// Drive types returned by GetDriveType. x/sys/windows does not export these.
const (
	driveNoRootDir = 1
	driveRemote    = 4
)

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
	Existed     bool
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
	if isDevicePath(path) {
		return "", hverr.New(hverr.InvalidArgument,
			"%q is a device path, not a filesystem path", path)
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
	if path == "" || isDevicePath(path) || !filepath.IsAbs(path) {
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

	existed, err := inspectDestination(clean, overwrite)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(clean), ".hypervm-mcp-copy-*.tmp")
	if err != nil {
		return nil, hverr.Wrap(hverr.PathNotAccessible, err,
			"could not create a staging file in %s", filepath.Dir(clean))
	}
	return &FileDestination{
		Path:        clean,
		StagingPath: f.Name(),
		Existed:     existed,
		overwrite:   overwrite,
		stagingFile: f,
	}, nil
}

// CopyFrom fills the caller-created staging file through its retained handle.
// It must run while impersonating the pipe client.
func (d *FileDestination) CopyFrom(src io.Reader) error {
	if d.stagingFile == nil {
		return hverr.New(hverr.Internal, "destination staging file is not open")
	}
	if _, err := io.Copy(d.stagingFile, src); err != nil {
		d.stagingFile.Close()
		d.stagingFile = nil
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not write staging file for %s", d.Path)
	}
	if err := d.stagingFile.Sync(); err != nil {
		d.stagingFile.Close()
		d.stagingFile = nil
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not flush staging file for %s", d.Path)
	}
	if err := d.stagingFile.Close(); err != nil {
		d.stagingFile = nil
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not close staging file for %s", d.Path)
	}
	d.stagingFile = nil
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
func (d *FileDestination) Commit() error {
	if d.stagingFile != nil {
		return hverr.New(hverr.Internal, "destination staging file is still open")
	}
	if _, err := ValidateFileDestination(d.Path, false); err != nil {
		return err
	}
	if _, err := inspectDestination(d.Path, d.overwrite); err != nil {
		return err
	}
	if err := rejectReparsePoints(d.StagingPath); err != nil {
		return err
	}

	from, err := windows.UTF16PtrFromString(d.StagingPath)
	if err != nil {
		return hverr.Wrap(hverr.InvalidArgument, err, "invalid staging path")
	}
	to, err := windows.UTF16PtrFromString(d.Path)
	if err != nil {
		return hverr.Wrap(hverr.InvalidArgument, err, "invalid destination path")
	}
	if d.overwrite {
		err = windows.MoveFileEx(from, to,
			windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	} else {
		err = windows.MoveFile(from, to)
	}
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
			errors.Is(err, windows.ERROR_FILE_EXISTS) {
			return hverr.New(hverr.VMAlreadyExists,
				"%s already exists; pass overwrite to replace it", d.Path)
		}
		return hverr.Wrap(hverr.PathNotAccessible, err,
			"could not move the copied file to %s", d.Path)
	}
	return nil
}

func inspectDestination(path string, overwrite bool) (bool, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.IsDir() {
			return false, hverr.New(hverr.InvalidArgument,
				"%s is a directory; destination_path must name a file", path)
		}
		if !overwrite {
			return false, hverr.New(hverr.VMAlreadyExists,
				"%s already exists; pass overwrite to replace it", path)
		}
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, hverr.Wrap(hverr.PathNotAccessible, err,
			"could not inspect %s", path)
	}
}

// ValidateDir is Validate for a directory the caller wants to write into.
func ValidateDir(path string, createParents bool) (string, error) {
	if path == "" {
		return "", hverr.New(hverr.InvalidArgument, "path is required")
	}
	if isDevicePath(path) {
		return "", hverr.New(hverr.InvalidArgument,
			"%q is a device path, not a filesystem path", path)
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
