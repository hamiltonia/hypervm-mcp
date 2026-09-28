package mcpsrv

import (
	"context"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winpath"
)

type guestInvokeInput struct {
	VMName         string `json:"vm_name" jsonschema:"Exact name of the VM."`
	Command        string `json:"command" jsonschema:"PowerShell command to run inside the guest."`
	Username       string `json:"username,omitempty" jsonschema:"Overrides the stored username."`
	Password       string `json:"password,omitempty" jsonschema:"Overrides the stored password."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Default 120."`
}

type sendKeyInput struct {
	VMName     string   `json:"vm_name" jsonschema:"Exact name of the VM. It must be running."`
	Keys       []string `json:"keys" jsonschema:"Keys to press in order: names such as \"space\", \"enter\", \"esc\", \"f8\", \"up\", or virtual-key codes such as \"0x20\"."`
	Repeat     int      `json:"repeat,omitempty" jsonschema:"How many times to send the sequence. Default 1. Use ~20 to cover a boot prompt whose timing is unknown."`
	IntervalMS int      `json:"interval_ms,omitempty" jsonschema:"Milliseconds between repeats. Default 1000."`
}

type sendMouseInput struct {
	VMName       string `json:"vm_name" jsonschema:"Exact name of the VM. It must be running."`
	X            int    `json:"x" jsonschema:"Horizontal pixel position to move to."`
	Y            int    `json:"y" jsonschema:"Vertical pixel position to move to."`
	ScreenWidth  int    `json:"screen_width" jsonschema:"Width of the image x and y were read from — normally capture_vm_screen's width."`
	ScreenHeight int    `json:"screen_height" jsonschema:"Height of that same image."`
	Button       string `json:"button,omitempty" jsonschema:"Click after moving: \"left\", \"right\" or \"middle\". Omit to only move."`
	Scroll       int    `json:"scroll,omitempty" jsonschema:"Wheel movement; positive scrolls up. Omit for none."`
}

type captureScreenInput struct {
	VMName     string `json:"vm_name" jsonschema:"Exact name of the VM. It must be running."`
	Width      int    `json:"width,omitempty" jsonschema:"Capture width in pixels. Default 1024. Ask for something near the guest's own resolution."`
	Height     int    `json:"height,omitempty" jsonschema:"Capture height in pixels. Default 768."`
	OutputPath string `json:"output_path,omitempty" jsonschema:"Host path to also write the PNG to. The image is returned either way."`
}

type guestSessionInput struct {
	VMName         string `json:"vm_name" jsonschema:"Exact name of the VM."`
	Command        string `json:"command" jsonschema:"PowerShell command to run on the guest's desktop."`
	Username       string `json:"username,omitempty" jsonschema:"Overrides the stored username."`
	Password       string `json:"password,omitempty" jsonschema:"Overrides the stored password."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Default 300."`
}

type guestCopyInput struct {
	VMName          string `json:"vm_name" jsonschema:"Exact name of the VM."`
	SourcePath      string `json:"source_path" jsonschema:"File on the host to copy."`
	DestinationPath string `json:"destination_path" jsonschema:"Where to put it inside the guest."`
	CreateFullPath  bool   `json:"create_full_path,omitempty" jsonschema:"Create missing directories in the guest."`
	Overwrite       bool   `json:"overwrite,omitempty" jsonschema:"Replace an existing file."`
}

type guestCopyFromInput struct {
	VMName          string `json:"vm_name" jsonschema:"Exact name of the Windows VM."`
	SourcePath      string `json:"source_path" jsonschema:"One file inside the guest to copy."`
	DestinationPath string `json:"destination_path" jsonschema:"Absolute host file path to create."`
	CreateParents   bool   `json:"create_parents,omitempty" jsonschema:"Create missing host parent directories."`
	Overwrite       bool   `json:"overwrite,omitempty" jsonschema:"Replace an existing host file. Default false."`
	Username        string `json:"username,omitempty" jsonschema:"Overrides the stored username."`
	Password        string `json:"password,omitempty" jsonschema:"Overrides the stored password."`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty" jsonschema:"Default 300."`
}

// guestCredentials fills in whatever the caller left out from the store.
//
// PowerShell Direct authenticates with a password, so a key on file is not
// enough and the error has to say which piece is missing.
func guestCredentials(d *Deps, vm, username, password string) (string, string, error) {
	if username != "" && password != "" {
		return username, password, nil
	}
	stored, ok, err := d.Creds.Get(vm)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", hverr.New(hverr.CredentialNotFound,
			"no credentials stored for %q", vm).
			WithDetail("Run: hypervm-mcp cred set --vm " + vm + " --user <name>")
	}
	if username == "" {
		username = stored.Username
	}
	if password == "" {
		password = stored.Password
	}
	return username, password, nil
}

func registerGuestTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "guest_invoke_command",
		Title: "Run a command in a Windows guest without networking",
		Description: "Run a PowerShell command inside a Windows guest over PowerShell Direct.\n\n" +
			"This travels over the VMBus rather than the network, so it works on a VM with no " +
			"address, no switch, or a Private switch. That makes it the way to bootstrap a guest " +
			"before anything else can reach it — installing sshd, or changing network settings that " +
			"would cut an SSH session mid-command.\n\n" +
			"Windows guests only; Linux has no PowerShell Direct endpoint, so use ssh_exec there. " +
			"It also needs a password: a key is not enough for this transport.\n\n" +
			"The session it gets is elevated — an unfiltered administrator token, so installing " +
			"features and writing under HKLM work with nothing to confirm.\n\n" +
			"But it is session 0, which has no desktop. Starting a graphical program here appears " +
			"to succeed and shows nothing, a screen capture taken here is blank, and UI automation " +
			"finds no elements — none of which reports an error. For anything with a window, use " +
			"guest_run_in_session.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in guestInvokeInput) (*mcp.CallToolResult, *hyperv.GuestResult, error) {
		user, pass, err := guestCredentials(d, in.VMName, in.Username, in.Password)
		if err != nil {
			return nil, nil, err
		}
		out, err := d.VM.GuestInvokeCommand(ctx, in.VMName, in.Command, user, pass,
			time.Duration(in.TimeoutSeconds)*time.Second)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "send_vm_key",
		Title: "Press keys at a VM's console",
		Description: "Type into a VM's console keyboard, before it has an operating system.\n\n" +
			"This is for prompts nothing inside the guest can answer. Microsoft's installation " +
			"media asks \"Press any key to boot from CD or DVD\" and gives up after a few seconds, " +
			"so an unattended install from a stock ISO never starts unless something presses a " +
			"key — and guest_invoke_command, ssh_exec and guest_copy_file all need a booted " +
			"guest, which is exactly what does not exist yet.\n\n" +
			"The prompt is a race: it opens a few seconds after power-on and closes again. Rather " +
			"than time it, set repeat and interval_ms to press steadily across the window — " +
			"20 presses a second apart covers a normal boot.\n\n" +
			"Keys are names (\"space\", \"enter\", \"esc\", \"f8\", arrows) or virtual-key codes " +
			"(\"0x20\"). This types single keys; it is not a way to drive an installer by hand.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendKeyInput) (*mcp.CallToolResult, *hyperv.KeyResult, error) {
		if len(in.Keys) == 0 {
			return nil, nil, hverr.New(hverr.InvalidArgument, "keys is required")
		}
		codes := make([]int, 0, len(in.Keys))
		for _, k := range in.Keys {
			code, err := hyperv.ParseKey(k)
			if err != nil {
				return nil, nil, err
			}
			codes = append(codes, code)
		}
		out, err := d.VM.SendKeys(ctx, in.VMName, codes, in.Repeat,
			time.Duration(in.IntervalMS)*time.Millisecond)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "capture_vm_screen",
		Title: "Photograph a VM's console",
		Description: "Return a picture of what the VM is displaying, read from the host side.\n\n" +
			"This needs nothing running inside the guest: no agent, no network, no operating " +
			"system. It is the only way to see a firmware prompt, a boot menu, a stop error or an " +
			"installer stuck on a dialog — every other route in needs something in the guest to " +
			"answer, and in those situations nothing is.\n\n" +
			"What comes back is a scaled thumbnail of the console, so read it, do not compare it. " +
			"For deciding whether a GUI is correct, drive its automation tree from " +
			"guest_run_in_session instead; pixels shift with resolution, theme and DPI.\n\n" +
			"Give no width or height and it uses the console's own resolution, which is the only " +
			"size always accepted — a Generation 1 firmware screen is 640x480-ish, nowhere near a " +
			"sensible-looking default.\n\n" +
			"If the result says blank, one colour covers the whole frame: usually a display that " +
			"has gone to sleep, which send_vm_key wakes without typing anything.\n\n" +
			"This changes nothing in the VM. It writes a file on the host only when output_path " +
			"is given; otherwise the picture comes back in the reply and nowhere else.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in captureScreenInput) (*mcp.CallToolResult, *hyperv.ScreenCapture, error) {
		out, err := d.VM.CaptureScreen(ctx, in.VMName, in.Width, in.Height, in.OutputPath)
		if err != nil {
			return nil, nil, err
		}
		// Returned as image content as well as a summary, so a client that can
		// look at pictures does not have to go and open a file.
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.ImageContent{Data: out.Image(), MIMEType: "image/png"}},
		}, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "send_vm_mouse",
		Title: "Move and click the VM's pointer",
		Description: "Move the console pointer, and optionally click or scroll.\n\n" +
			"Give x and y in pixels together with the size of the image they came from — normally " +
			"a capture_vm_screen result. Hyper-V positions the pointer as a fraction of the " +
			"screen, so a point read off a small thumbnail lands correctly on a large desktop.\n\n" +
			"This drives the console itself, so it works on anything on screen. It does need the " +
			"guest's integration services to have bound the pointer, which happens during boot: a " +
			"guest still in firmware has a keyboard but no mouse, so use send_vm_key there.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sendMouseInput) (*mcp.CallToolResult, *hyperv.MouseResult, error) {
		out, err := d.VM.SendMouse(ctx, in.VMName, in.X, in.Y, in.ScreenWidth, in.ScreenHeight,
			in.Button, in.Scroll)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "guest_run_in_session",
		Title: "Run a command on a Windows guest's desktop",
		Description: "Windows guests only. Run a PowerShell command in the guest's interactive " +
			"session, elevated.\n\n" +
			"There is no Linux equivalent of this and there cannot be one here: it is built on " +
			"PowerShell Direct and the Windows task scheduler, and Hyper-V gives a Linux guest no " +
			"command channel at all. To reach a Linux desktop, use ssh_exec with DISPLAY and " +
			"XDG_RUNTIME_DIR set for the logged-in seat.\n\n" +
			"guest_invoke_command lands in session 0, which Windows keeps for services and which " +
			"has no desktop: a window opened there is drawn nowhere, a screen capture taken there " +
			"is blank, and UI automation finds nothing. Anything involving a graphical program " +
			"has to run where a user is logged on, and this puts it there.\n\n" +
			"It also runs at the highest run level, so the process gets an unfiltered " +
			"administrator token — this is the way to drive a program that both needs elevation " +
			"and shows a window.\n\n" +
			"Somebody must be logged on to a desktop; arrange automatic logon on a guest meant " +
			"for this. Output is whatever the command writes, errors included.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in guestSessionInput) (*mcp.CallToolResult, *hyperv.SessionResult, error) {
		user, pass, err := guestCredentials(d, in.VMName, in.Username, in.Password)
		if err != nil {
			return nil, nil, err
		}
		out, err := d.VM.GuestRunInSession(ctx, in.VMName, in.Command, user, pass,
			time.Duration(in.TimeoutSeconds)*time.Second)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "guest_copy_file",
		Title: "Copy a file into a guest without networking",
		Description: "Copy a file from the host into a running guest over the VMBus, so it needs no " +
			"guest network.\n\n" +
			"It does need the Guest Service Interface integration component, which this enables if it " +
			"is switched off — on Linux that component is hypervfcopyd from the hyperv-daemons " +
			"package.\n\n" +
			"For the reverse direction from a Windows guest, use guest_copy_from." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in guestCopyInput) (*mcp.CallToolResult, map[string]any, error) {
		err := d.VM.GuestCopyFile(ctx, in.VMName, in.SourcePath, in.DestinationPath,
			in.CreateFullPath, in.Overwrite)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{
			"copied":      true,
			"vm_name":     in.VMName,
			"destination": in.DestinationPath,
		}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "guest_copy_from",
		Title: "Copy a file out of a Windows guest without networking",
		Description: "Copy one file from a running Windows guest to the host over PowerShell Direct " +
			"and the VMBus, so it needs no guest network.\n\n" +
			"Windows guests only. This uses the stored PowerShell Direct credentials unless username " +
			"or password overrides are supplied. Directories are not copied recursively; archive one " +
			"inside the guest first, then retrieve the archive.\n\n" +
			"overwrite defaults to false. create_parents defaults to false. The result includes the " +
			"guest file size and SHA256, both verified against the final host staging file. The host " +
			"destination is created as the connected user, not as LocalSystem; device paths and " +
			"existing reparse points are rejected." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in guestCopyFromInput) (*mcp.CallToolResult, *hyperv.GuestCopyFromResult, error) {
		user, pass, err := guestCredentials(d, in.VMName, in.Username, in.Password)
		if err != nil {
			return nil, nil, err
		}
		if d.Impersonate == nil {
			return nil, nil, hverr.New(hverr.Internal,
				"the service did not provide a client impersonation context")
		}

		timeout := time.Duration(in.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		copyCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		var target *winpath.FileDestination
		err = d.Impersonate(func() error {
			var prepareErr error
			target, prepareErr = winpath.PrepareFileDestination(
				in.DestinationPath, in.CreateParents, in.Overwrite)
			return prepareErr
		})
		if err != nil {
			return nil, nil, err
		}
		defer func() {
			if cleanupErr := d.Impersonate(target.Cleanup); cleanupErr != nil && d.Log != nil {
				d.Log.Warnf("guest_copy_from staging cleanup failed: %v", cleanupErr)
			}
		}()

		serviceStage, err := os.CreateTemp(config.DataDir(), ".guest-copy-from-*.tmp")
		if err != nil {
			return nil, nil, hverr.Wrap(hverr.PathNotAccessible, err,
				"could not create protected guest-copy staging file")
		}
		serviceStagePath := serviceStage.Name()
		if err := serviceStage.Close(); err != nil {
			os.Remove(serviceStagePath)
			return nil, nil, hverr.Wrap(hverr.PathNotAccessible, err,
				"could not close protected guest-copy staging file")
		}
		defer func() {
			if cleanupErr := os.Remove(serviceStagePath); cleanupErr != nil &&
				!os.IsNotExist(cleanupErr) && d.Log != nil {
				d.Log.Warnf("guest_copy_from protected staging cleanup failed: %v", cleanupErr)
			}
		}()

		out, err := d.VM.GuestCopyFrom(copyCtx, in.VMName, in.SourcePath, serviceStagePath,
			user, pass, timeout)
		if err != nil {
			return nil, nil, err
		}
		serviceStage, err = os.Open(serviceStagePath)
		if err != nil {
			return nil, nil, hverr.Wrap(hverr.PathNotAccessible, err,
				"could not open protected guest-copy staging file")
		}
		defer serviceStage.Close()

		if err := d.Impersonate(func() error {
			if err := target.CopyFrom(copyCtx, serviceStage, out.SizeBytes, out.SHA256); err != nil {
				return err
			}
			return target.Commit(copyCtx)
		}); err != nil {
			return nil, nil, err
		}
		out.Destination = target.Path
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "get_guest_network",
		Title: "Guest network adapters",
		Description: "Report a VM's virtual network adapters with the switch each is on and the " +
			"addresses the guest reports. An empty address list is normal shortly after boot, and " +
			"permanent on a guest without the reporting agent — it does not mean the guest is " +
			"unreachable. Use diagnose_vm_network to tell those apart.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameOnlyInput) (*mcp.CallToolResult, *listOf[hyperv.NetworkAdapter], error) {
		return list(d.VM.GetNetworkAdapters(ctx, in.VMName))
	})
}
