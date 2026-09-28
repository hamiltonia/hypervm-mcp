package mcpsrv

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
)

// pathRules is repeated in the tools that take paths, because getting this wrong
// is the most common way these calls fail.
const pathRules = "\n\nPaths are opened by the service, which runs as LocalSystem. " +
	"Mapped drive letters (Z:\\) do not exist in its logon session and are rejected; " +
	"use a UNC path and grant the computer account access to the share. " +
	"Paths are checked before anything is created, so a bad one fails cleanly."

type createVMInput struct {
	Name            string `json:"name" jsonschema:"Name for the new VM. Must not already exist."`
	Generation      int    `json:"generation,omitempty" jsonschema:"1 for legacy BIOS guests, 2 for UEFI. Default 2."`
	MemoryMB        int    `json:"memory_mb,omitempty" jsonschema:"Startup memory in MB. Default 4096."`
	DynamicMemory   bool   `json:"dynamic_memory,omitempty" jsonschema:"Let Hyper-V grow and shrink memory. Default off; some OS installers misbehave under it."`
	CPUCount        int    `json:"cpu_count,omitempty" jsonschema:"Virtual processors. Default 2."`
	VMPath          string `json:"vm_path,omitempty" jsonschema:"Directory for the VM configuration files. FIXED AT CREATION - the only way to move it later is export and re-import. Defaults to the host setting."`
	VHDPath         string `json:"vhd_path,omitempty" jsonschema:"Full path of the boot disk. An existing file is attached as-is; otherwise a new dynamic VHDX of vhd_size_mb is created. Defaults to the host setting."`
	VHDSizeMB       int    `json:"vhd_size_mb,omitempty" jsonschema:"Size of the new boot disk in MB. Default 65536 (64 GB). Ignored when vhd_path names an existing file."`
	CheckpointPath  string `json:"checkpoint_path,omitempty" jsonschema:"Directory for checkpoint files. Can be changed later."`
	SmartPagingPath string `json:"smart_paging_path,omitempty" jsonschema:"Directory for the smart paging file. Can be changed later."`
	SwitchName      string `json:"switch_name,omitempty" jsonschema:"Virtual switch to connect to. Empty leaves the VM with no network. See list_switches."`
	ISOPath         string `json:"iso_path,omitempty" jsonschema:"ISO to attach as a DVD and boot from first."`
	SecureBoot      string `json:"secure_boot,omitempty" jsonschema:"Generation 2 only. \"windows\" (default), \"linux\" for distributions signed by the third-party UEFI CA, or \"off\"."`
	CreateParents   bool   `json:"create_parents,omitempty" jsonschema:"Create missing directories for the paths above."`
}

type deleteVMInput struct {
	Name        string `json:"name" jsonschema:"Exact name of the VM."`
	DeleteDisks bool   `json:"delete_disks,omitempty" jsonschema:"Also delete the VM's disk files. Disks shared with another VM are never deleted."`
	Force       bool   `json:"force,omitempty" jsonschema:"Power the VM off first if it is running."`
}

type renameVMInput struct {
	Name    string `json:"name" jsonschema:"Exact current name of the VM."`
	NewName string `json:"new_name" jsonschema:"Name to give it. Must not already belong to another VM."`
}

// renameVMResult says what moved with the name, so a caller can tell a complete
// rename from one that left something behind.
type renameVMResult struct {
	VM               *hyperv.VMDetail `json:"vm"`
	CredentialsMoved bool             `json:"credentials_moved"`
	HostKeyMoved     bool             `json:"host_key_moved"`
	TunnelsMoved     []string         `json:"tunnels_moved,omitempty"`
	Note             string           `json:"note,omitempty"`
	Warnings         []string         `json:"warnings,omitempty"`
}

type createVHDInput struct {
	Path          string `json:"path" jsonschema:"Full path of the new .vhdx file."`
	SizeMB        int    `json:"size_mb,omitempty" jsonschema:"Virtual size in MB. Default 65536 (64 GB). Ignored for a differencing disk."`
	DiskType      string `json:"disk_type,omitempty" jsonschema:"\"dynamic\" (default), \"fixed\", or \"differencing\"."`
	ParentPath    string `json:"parent_path,omitempty" jsonschema:"Parent image for a differencing disk. Required when disk_type is differencing."`
	CreateParents bool   `json:"create_parents,omitempty" jsonschema:"Create the parent directory if it is missing."`
}

type vmDiskInput struct {
	VMName string `json:"vm_name" jsonschema:"Exact name of the VM."`
	Path   string `json:"path" jsonschema:"Full path of the .vhdx file."`
}

type attachVHDInput struct {
	VMName             string `json:"vm_name" jsonschema:"Exact name of the VM."`
	Path               string `json:"path" jsonschema:"Full path of the .vhdx file."`
	ControllerType     string `json:"controller_type,omitempty" jsonschema:"\"SCSI\" (default) or \"IDE\"."`
	ControllerNumber   int    `json:"controller_number,omitempty" jsonschema:"Which controller to attach to. Default 0."`
	ControllerLocation *int   `json:"controller_location,omitempty" jsonschema:"Which port on that controller. Omit to take the next free one."`
}

type createSeedDiskInput struct {
	Path          string            `json:"path" jsonschema:"Full path of the .vhdx to create."`
	Label         string            `json:"label" jsonschema:"Volume label the installer looks for: OEMDRV for Anaconda kickstart, CIDATA for cloud-init. Maximum 11 characters."`
	SizeMB        int               `json:"size_mb,omitempty" jsonschema:"Disk size in MB. Default 256."`
	Files         []hyperv.SeedFile `json:"files" jsonschema:"Files to write, with paths relative to the volume root."`
	Overwrite     bool              `json:"overwrite,omitempty" jsonschema:"Replace an existing file at path."`
	CreateParents bool              `json:"create_parents,omitempty" jsonschema:"Create the parent directory if it is missing."`
}

func registerProvisionTools(s *mcp.Server, d *Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "list_switches",
		Title: "List virtual switches",
		Description: "List the host's virtual switches with their type and the VMs attached to each. " +
			"Switch type decides what a guest can reach: Private sees only other VMs, Internal adds " +
			"the host, the Default Switch adds outbound internet through NAT, and External puts the " +
			"guest on the physical LAN with its own address.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *listOf[hyperv.VMSwitch], error) {
		return list(d.VM.ListSwitches(ctx))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "create_vm",
		Title: "Create VM",
		Description: "Create a virtual machine. Storage locations for the configuration, boot disk, " +
			"checkpoints and paging file can each be set independently.\n\n" +
			"Generation 2 (the default) is UEFI. Linux guests need secure_boot \"linux\", because " +
			"distributions are signed by the third-party UEFI CA rather than the Windows one and " +
			"will not boot under the default template.\n\n" +
			"vm_path cannot be changed after creation." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createVMInput) (*mcp.CallToolResult, *hyperv.VMDetail, error) {
		out, err := d.VM.CreateVM(ctx, hyperv.CreateVMOptions{
			Name:            in.Name,
			Generation:      in.Generation,
			MemoryMB:        in.MemoryMB,
			DynamicMemory:   in.DynamicMemory,
			CPUCount:        in.CPUCount,
			VMPath:          in.VMPath,
			VHDPath:         in.VHDPath,
			VHDSizeMB:       in.VHDSizeMB,
			CheckpointPath:  in.CheckpointPath,
			SmartPagingPath: in.SmartPagingPath,
			SwitchName:      in.SwitchName,
			ISOPath:         in.ISOPath,
			SecureBoot:      in.SecureBoot,
			CreateParents:   in.CreateParents,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "delete_vm",
		Title: "Delete VM",
		Description: "Remove a VM. With delete_disks its disk files go too, except any that another VM " +
			"still uses or that another VM's differencing disk descends from — the result lists what " +
			"was kept and why.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteVMInput) (*mcp.CallToolResult, *hyperv.DeleteVMResult, error) {
		out, err := d.VM.DeleteVM(ctx, in.Name, in.DeleteDisks, in.Force)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "rename_vm",
		Title: "Rename a VM",
		Description: "Rename a VM, and move everything this server files under its name with it.\n\n" +
			"Stored credentials, the pinned SSH host key and any open tunnels are all keyed by VM " +
			"name. A plain Hyper-V rename leaves them behind, and the VM then looks unknown: no " +
			"credentials, no pin — so the next connection is treated as a first sighting and a " +
			"changed key is trusted silently — and tunnels that fail the next time they need to " +
			"find the guest. This moves all three and reports what moved.\n\n" +
			"Only the name changes. The configuration folder, the virtual disks and the " +
			"checkpoint files keep their old names on disk; renaming those means exporting and " +
			"reimporting.\n\n" +
			"Refused if another VM already has the new name. Hyper-V would allow it, but every " +
			"tool here addresses a VM by name and two would be indistinguishable.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in renameVMInput) (*mcp.CallToolResult, *renameVMResult, error) {
		// Hyper-V first: it is the only step that can legitimately refuse, and
		// moving the stored state before knowing the rename took would leave it
		// filed under a name no VM has.
		vm, err := d.VM.RenameVM(ctx, in.Name, in.NewName)
		if err != nil {
			return nil, nil, err
		}

		out := &renameVMResult{VM: vm}
		if moved, err := d.Creds.Rename(in.Name, in.NewName); err != nil {
			out.Warnings = append(out.Warnings,
				"the VM was renamed but its credentials could not be moved: "+err.Error()+
					". Store them again with `hypervm-mcp cred set --vm "+in.NewName+"`.")
		} else {
			out.CredentialsMoved = moved
		}

		if moved, err := d.HostKeys.Rename(in.Name, in.NewName); err != nil {
			out.Warnings = append(out.Warnings,
				"the VM was renamed but its pinned host key could not be moved: "+err.Error()+
					". The next SSH connection will look like a first sighting.")
		} else {
			out.HostKeyMoved = moved
		}

		// Pooled connections are filed under the old name too, and one left
		// there would answer for a VM that no longer has that name.
		d.SSH.Drop(in.Name)

		out.TunnelsMoved = d.Tunnels.RenameVM(in.Name, in.NewName)
		out.Note = "Files on disk keep the old name: the configuration folder, the disks and any " +
			"checkpoints. Only the VM's name changed."
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "create_vhd",
		Title: "Create virtual disk",
		Description: "Create a .vhdx without attaching it. Size is in megabytes, so a disk can be " +
			"given the exact capacity a test needs rather than the nearest gigabyte.\n\n" +
			"A dynamic disk shares the host volume's free space and grows on write, which distorts " +
			"storage benchmarks and hides how a guest behaves when its disks actually fill up; use " +
			"fixed for those. A differencing disk provisions in seconds from a golden image, at the " +
			"cost of depending on that image never changing." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createVHDInput) (*mcp.CallToolResult, *hyperv.VHDInfo, error) {
		out, err := d.VM.CreateVHD(ctx, in.Path, in.SizeMB, in.DiskType, in.ParentPath, in.CreateParents)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "attach_vhd",
		Title: "Attach virtual disk",
		Description: "Attach an existing .vhdx to a VM.\n\n" +
			"Controller placement is yours to choose because the guest names its devices by " +
			"controller location. Attaching a set of disks at consecutive locations is what makes " +
			"them appear as consecutive devices in the guest — which matters when they are meant to " +
			"form one array, and is left to chance if you do not say.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in attachVHDInput) (*mcp.CallToolResult, *hyperv.VMDetail, error) {
		out, err := d.VM.AttachVHD(ctx, hyperv.AttachOptions{
			VMName:             in.VMName,
			Path:               in.Path,
			ControllerType:     in.ControllerType,
			ControllerNumber:   in.ControllerNumber,
			ControllerLocation: in.ControllerLocation,
		})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "detach_vhd",
		Title:       "Detach virtual disk",
		Description: "Remove a disk from a VM without deleting the file.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmDiskInput) (*mcp.CallToolResult, *hyperv.VMDetail, error) {
		out, err := d.VM.DetachVHD(ctx, in.VMName, in.Path)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "create_seed_disk",
		Title: "Create unattended-install seed disk",
		Description: "Build a small disk holding an installer's answer file, then attach it with " +
			"attach_vhd to install a guest with no console input.\n\n" +
			"Installers find these by volume label, not by path: Anaconda reads /ks.cfg from a volume " +
			"labelled OEMDRV with no boot parameter at all, and cloud-init reads user-data and " +
			"meta-data from CIDATA. That makes this enough to automate an install without rebuilding " +
			"the installation ISO.\n\n" +
			"Files are written as UTF-8 with LF endings, since Linux tooling parses them." + pathRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createSeedDiskInput) (*mcp.CallToolResult, *hyperv.SeedDiskResult, error) {
		out, err := d.VM.CreateSeedDisk(ctx, in.Path, in.Label, in.SizeMB, in.Files, in.Overwrite, in.CreateParents)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "eject_dvd",
		Title:       "Eject DVD drives",
		Description: "Remove every DVD drive from a VM. Do this once an unattended install finishes, so the VM boots from disk instead of running the installer again.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in vmNameInput) (*mcp.CallToolResult, *hyperv.VMDetail, error) {
		out, err := d.VM.EjectDVD(ctx, in.Name)
		return nil, out, err
	})
}
