//go:build windows

// Package winsec holds the Windows security primitives this service depends on:
// identifying the calling user, deciding who may open the pipe, and locking down
// the data directory.
package winsec

import (
	"fmt"
	"net"
	"runtime"
	"syscall"

	"golang.org/x/sys/windows"
)

var impersonateNamedPipeClient = windows.NewLazySystemDLL("advapi32.dll").
	NewProc("ImpersonateNamedPipeClient")

// CurrentUserSID returns the SID of the account running this process.
func CurrentUserSID() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read process token user: %w", err)
	}
	return user.User.Sid.String(), nil
}

// IsElevated reports whether this process is running with an elevated token.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// WithNamedPipeClient runs fn under the token of the client connected to conn.
//
// Impersonation is thread-local, so the goroutine is pinned until RevertToSelf
// completes. This lets filesystem operations enforce the caller's permissions
// even though the service process itself runs as LocalSystem.
func WithNamedPipeClient(conn net.Conn, fn func() error) error {
	hc, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return fmt.Errorf("named pipe connection does not expose its Windows handle")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var impersonateErr error
	if r1, _, callErr := impersonateNamedPipeClient.Call(hc.Fd()); r1 == 0 {
		if callErr != syscall.Errno(0) {
			impersonateErr = callErr
		} else {
			impersonateErr = windows.ERROR_ACCESS_DENIED
		}
	}
	if impersonateErr != nil {
		return fmt.Errorf("impersonate named pipe client: %w", impersonateErr)
	}

	var fnErr, revertErr error
	func() {
		defer func() { revertErr = windows.RevertToSelf() }()
		fnErr = fn()
	}()
	if revertErr != nil {
		return fmt.Errorf("revert named pipe client impersonation: %w", revertErr)
	}
	return fnErr
}

// ValidateSID checks that s parses as a SID, so a bad --allowed-sid fails at
// install time rather than producing a pipe nobody can open.
func ValidateSID(s string) error {
	if _, err := windows.StringToSid(s); err != nil {
		return fmt.Errorf("%q is not a valid SID: %w", s, err)
	}
	return nil
}

// PipeSDDL builds the security descriptor for the service's named pipe.
//
// The "P" in "D:P" protects the DACL from inheritance, so no Everyone or
// Authenticated Users ACE can leak in from a parent object. Only the three
// listed trustees can open the pipe at all.
//
// The user gets 0x12019b (file read + file write + SYNCHRONIZE) rather than
// GENERIC_ALL: withholding WRITE_DAC stops a client from rewriting the very ACL
// that restricts it.
func PipeSDDL(allowedSID string) string {
	return "D:P" +
		"(A;;GA;;;SY)" + // LocalSystem
		"(A;;GA;;;BA)" + // BUILTIN\Administrators
		"(A;;0x12019b;;;" + allowedSID + ")"
}

// dataDirSDDL grants the service and administrators full control of the data
// directory, and the installing user read-only access so they can inspect
// config and logs without being able to tamper with them.
//
// OICI makes both ACEs inheritable by files and subdirectories.
func dataDirSDDL(allowedSID string) string {
	return "D:P" +
		"(A;OICI;FA;;;SY)" +
		"(A;OICI;FA;;;BA)" +
		"(A;OICI;FRFX;;;" + allowedSID + ")"
}

// SecureDataDir replaces path's DACL with an explicit, non-inherited one.
//
// This matters because the service binary lives under this directory: if a
// non-admin could write there, they could swap the binary that LocalSystem runs.
func SecureDataDir(path, allowedSID string) error {
	sd, err := windows.SecurityDescriptorFromString(dataDirSDDL(allowedSID))
	if err != nil {
		return fmt.Errorf("build security descriptor: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("extract DACL: %w", err)
	}
	err = windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
	if err != nil {
		return fmt.Errorf("apply DACL to %s: %w", path, err)
	}
	return nil
}
