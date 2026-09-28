// Package mcpsrv builds the MCP server the service exposes over the named pipe.
//
// A fresh server is constructed per connection so protocol state stays isolated
// between MCP sessions, while the dependencies it closes over (the PowerShell
// runner, and later the tunnel manager) are process-wide singletons.
package mcpsrv

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/creds"
	"github.com/heavycaffeiner/hypervm-mcp/internal/guest"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
	"github.com/heavycaffeiner/hypervm-mcp/internal/logx"
	"github.com/heavycaffeiner/hypervm-mcp/internal/sshx"
	"github.com/heavycaffeiner/hypervm-mcp/internal/tailnet"
	"github.com/heavycaffeiner/hypervm-mcp/internal/tunnel"
)

// Deps are the shared services every tool handler draws on. They are
// process-wide singletons, unlike the per-connection server that closes over them.
type Deps struct {
	VM       *hyperv.Client
	Guest    *guest.Dialer
	Creds    *creds.Store
	SSH      *sshx.Pool
	HostKeys *sshx.HostKeys
	Tunnels  *tunnel.Manager
	Tailnet  *tailnet.Client
	Log      *logx.Logger
	// Impersonate runs filesystem work as the connected pipe client. It is set
	// per MCP connection rather than on the process-wide dependency template.
	Impersonate func(func() error) error
}

const instructions = `Controls Hyper-V virtual machines on this Windows host.

Two things are worth knowing up front.

Hyper-V reports a VM as Running well before its guest OS has finished booting.
After start_vm, call wait_for_guest_ip before assuming the VM is reachable.

There are two ways to reach a service inside a VM, and they solve different
problems. A tunnel forwards a host port and works for anything whose client can
be pointed at a port; use mode "ssh" when the service is bound to the guest's own
127.0.0.1, because no direct route reaches that. An External virtual switch gives
the guest its own address on the physical LAN, which is the only option when the
host already holds the port (SMB, RDP, WinRM), when the protocol is sensitive to
host identity, or when other machines must reach the guest directly.

A VM's name is an identity here, not a label. Stored credentials, the pinned SSH
host key and every open tunnel are filed under it. Renaming a VM any other way
leaves all three behind and this server stops recognising it — no credentials, no
pin, so the next connection is treated as a first sighting and a changed host key
is trusted in silence. Use rename_vm, which carries them across.

get_vm answers what a VM is; get_vm_settings answers how it is configured, and
reads every property page in one call. Its output is the vocabulary every
set_vm_* tool accepts, so what it reports can be sent straight back rather than
guessed at — the boot order in particular reports a token per device that
reorders it.

Guest-facing tools split by where their mechanism lives, and the split decides
which guests they work on. capture_vm_screen, send_vm_key and send_vm_mouse
drive Hyper-V's own console devices, so they work whatever the guest runs — or
even when nothing is running yet. guest_invoke_command and guest_run_in_session
go through PowerShell Direct and are Windows-only; the Linux equivalent is
ssh_exec. guest_copy_file works on both, but a Linux guest needs hypervfcopyd
from hyperv-daemons. guest_copy_from retrieves one file from a Windows guest
over PowerShell Direct; archive directories first.

A Windows guest can be driven before it has any network at all.
guest_invoke_command runs a command over the VMBus and gets an unfiltered
administrator token, so installing features and writing under HKLM work with no
prompt to answer. It needs a password; an SSH key is not enough for that
transport.

Anything graphical needs one more step, and skipping it fails quietly.
guest_invoke_command lands in session 0, which has no desktop: a window opened
there is drawn nowhere, a screen capture taken there is blank, and UI automation
finds no elements — all without an error. Use guest_run_in_session to run in the
logged-on user's session instead. It is elevated too, so it is also the way to
drive a program that needs administrator and shows a window. It needs somebody
logged on, which means the guest needs automatic logon arranged.

To see a VM at all, capture_vm_screen reads the console from the host and needs
nothing running inside the guest — it is the only way to look at a firmware
prompt, a boot menu, a stop error, or an installer waiting on a dialog.
send_vm_key and send_vm_mouse drive the console's keyboard and pointer, which
likewise works before any guest software exists to do it.

Judge a GUI by its automation tree, queried through guest_run_in_session, not by
its pixels: pixels move with resolution, theme and DPI, and the capture is a
scaled thumbnail. Screenshots are for finding out what went wrong.`

// titleSuffix renders the build's instance name for display, empty on a release.
func titleSuffix() string {
	if config.Instance() == "" {
		return ""
	}
	return " (" + config.Instance() + ")"
}

// New returns a server with every tool registered, ready to run over a transport.
func New(version string, d *Deps) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		// Suffixed on a development build so a client connected to both can tell
		// which one answered.
		Name:        config.ServiceName(),
		Title:       "Hyper-V Manager" + titleSuffix(),
		Description: "Manage Hyper-V virtual machines without per-session privilege elevation.",
		Version:     version,
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	registerVMTools(s, d)
	registerSettingsTools(s, d)
	registerCheckpointTools(s, d)
	registerProvisionTools(s, d)
	registerStorageTools(s, d)
	registerDiskTools(s, d)
	registerNetworkTools(s, d)
	registerGuestTools(s, d)
	registerSSHTools(s, d)
	registerTunnelTools(s, d)
	registerDoctorTool(s, d)
	return s
}

// ptr is a helper for the SDK's optional-bool annotation fields.
func ptr[T any](v T) *T { return &v }

// listOf wraps a slice so a listing tool's output schema is an object.
//
// The Go SDK will happily infer an array schema for a slice return, but clients
// reject a tool whose outputSchema is not an object — Claude Code refuses to
// load the entire tool list over it, not just the offending tool. Wrapping costs
// one field and keeps every listing usable.
type listOf[T any] struct {
	Items []T `json:"items"`
	Count int `json:"count"`
}

func list[T any](items []T, err error) (*mcp.CallToolResult, *listOf[T], error) {
	if err != nil {
		return nil, nil, err
	}
	if items == nil {
		items = []T{}
	}
	return nil, &listOf[T]{Items: items, Count: len(items)}, nil
}
