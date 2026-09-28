# hypervm-mcp

**Drive Hyper-V from an AI coding agent, without a UAC prompt every time.**

[![CI](https://github.com/heavycaffeiner/hypervm-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/heavycaffeiner/hypervm-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**English** | [한국어](README.ko.md)

Every Hyper-V cmdlet needs administrator rights. Ask an agent to spin up a test
VM and you get an elevation prompt, then another, and another. So you either
click all day or run your agent elevated, which is worse.

This moves the privilege instead of asking for it. A small Windows service holds
the Hyper-V rights; your agent talks to it through a named pipe that only your
account can open. **One UAC prompt at install, and never again.**

```
create a Rocky Linux VM, install nginx, and show me its page
```

That is a thing you can now say, and the agent has the 50-odd tools to do it:
create the VM, install unattended, run commands inside it, forward a port out,
and take a picture of its screen if something goes wrong.

---

## What this is

New here? Two minutes of vocabulary, then you can skip the rest of this section
forever.

**Hyper-V** is the virtual machine engine built into Windows Pro and Windows
Server. It runs a whole other operating system (Linux, another Windows) in a box
on your machine.

**MCP**, the Model Context Protocol, is the standard way an AI agent such as
Claude Code is given tools it can call. An MCP server publishes a list of tools;
the agent decides when to use them and what to pass.

**hypervm-mcp** is an MCP server for Hyper-V. After installing it once, you ask
your agent for what you want in ordinary language and it does the Hyper-V work:

| You say | The agent calls |
|---|---|
| "make me an Ubuntu box with 8 GB of RAM" | `create_vm`, `create_seed_disk`, `start_vm` |
| "is it up yet?" | `wait_for_guest_ip`, `get_vm` |
| "install nginx on it" | `ssh_exec` |
| "let me open it in my browser" | `open_tunnel` |
| "it's stuck, what's on screen?" | `capture_vm_screen` |
| "snapshot it before I break something" | `create_checkpoint` |

You never call these yourself. They are listed throughout this README so you can
see what your agent is doing and tell it to do something more specific.

<details>
<summary><b>Words you will run into</b></summary>

| | |
|---|---|
| **Host** | Your physical Windows machine, the one running Hyper-V. |
| **Guest** | The operating system running inside a VM. |
| **Generation 1 / 2** | Hyper-V's two VM shapes. Gen 2 is UEFI and modern; Gen 1 is legacy BIOS, for old operating systems. Use Gen 2 unless something forces you not to. |
| **VHD / VHDX** | A virtual hard disk: one file on your host that the guest sees as a drive. |
| **Checkpoint** | A snapshot of a VM you can revert to. Hyper-V's name for it. |
| **Virtual switch** | A virtual network cable. *Default Switch* comes with Windows and just works; *Internal* connects host and guests only; *Private* connects guests only; *External* puts the guest on your real LAN. |
| **NAT** | Address translation, the thing that lets a guest on a private network reach the internet. |
| **UAC** | The Windows "do you want to allow this app to make changes" prompt. |
| **Windows service** | A background program Windows starts on boot, able to run with more rights than you have. |
| **Named pipe** | A local channel between two programs on the same machine, with Windows deciding who is allowed to open it. |
| **VMBus** | Hyper-V's private channel between host and guest. It does not use the network, so it works even when the guest has no network at all. |
| **PowerShell Direct** | Running PowerShell inside a Windows guest over the VMBus. No network, no SSH, no open ports. |
| **Integration services** | Small drivers and daemons inside the guest that let the host see and do more. Windows ships them; Linux needs `hyperv-daemons`. |
| **Unattended install / answer file** | A file the operating system installer reads so nobody has to click through setup. `autounattend.xml` on Windows, a kickstart file on Rocky or RHEL. |
| **Tunnel** | A port on your host forwarded into a guest, so `localhost:8080` reaches the guest's web server. |
| **Tailscale / tailnet** | A private network across your own machines. Optional here, used for reaching a VM from your laptop. |

</details>

---

## Contents

- [Install](#install)
- [Your first VM](#your-first-vm)
- [The three ways to reach a service](#the-three-ways-to-reach-a-service)
- [Things that will bite you](#things-that-will-bite-you)
- [Tools](#tools)
- [Testing Windows software in a VM](#testing-windows-software-in-a-vm)
- [Operating it](#operating-it)
- [How it works](#how-it-works)
- [What is verified](#what-is-verified)

---

## Install

You need, before anything else:

- **Windows 10/11 Pro, Enterprise or Education, or Windows Server.** Home
  editions have no Hyper-V.
- **Hyper-V turned on.** Not sure? Run
  `Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V` in
  PowerShell. If it says `Disabled`, enable Hyper-V from *Turn Windows features
  on or off* and reboot.
- **PowerShell 5.1**, which every supported Windows already has.
- **An MCP client**, such as Claude Code.

Then, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/heavycaffeiner/hypervm-mcp/main/install.ps1 | iex
```

This downloads the latest release, verifies its checksum, installs the service
(one UAC prompt, here), and registers itself with Claude Code if it finds it.

Check it worked:

```powershell
hypervm-mcp doctor
```

`doctor` inspects Hyper-V, the service, the pipe, storage paths, switches,
credentials and tunnels, and tells you what to fix if anything is off. Restart
your MCP client afterwards so it picks up the new server, then ask your agent to
list your VMs.

To upgrade later:

```powershell
hypervm-mcp update
```

Checks the latest release, verifies its checksum, and installs it over the
current one. Stored credentials, pinned host keys, tunnels and anything you
edited in `config.json` are kept; only `service uninstall --purge` deletes those.

<details>
<summary>Other ways to install</summary>

With Go:

```powershell
go install github.com/heavycaffeiner/hypervm-mcp/cmd/hypervm-mcp@latest
hypervm-mcp service install
claude mcp add hypervm-mcp -s user -- "$env:ProgramData\hypervm-mcp\bin\hypervm-mcp.exe" bridge
```

By hand: download `hypervm-mcp.exe` from a release, check it against
`hypervm-mcp.exe.sha256`, run `hypervm-mcp.exe service install`.

Per-workspace instead of globally, put this in `.mcp.json`:

```json
{
  "mcpServers": {
    "hypervm-mcp": {
      "command": "C:\\ProgramData\\hypervm-mcp\\bin\\hypervm-mcp.exe",
      "args": ["bridge"]
    }
  }
}
```
</details>

---

## Your first VM

A worked example, in the order the tools are meant to be used.

> [!NOTE]
> **You do not type the boxes below.** You ask in plain language, and the agent
> works out the call. The boxes show what your request turns into, so you can
> recognise it and ask for something more specific. Blocks that start with
> `hypervm-mcp` are the exception: those are real commands you run yourself in
> PowerShell.

**1. Make it.** `create_vm` takes separate paths for the configuration, the disk,
the checkpoints and the paging file, so nothing has to land on your system drive.

```
create_vm  name=dev-box  generation=2  memory_mb=4096  vhd_size_mb=32768
           iso_path=D:\ISO\Rocky-10.iso  switch_name="Default Switch"
```

**2. Install it without watching.** `create_seed_disk` writes an answer file to a
small disk the installer finds by itself: a kickstart on an `OEMDRV`-labelled
disk for Linux, or `autounattend.xml` on an ISO for Windows. No console, no
keyboard.

**3. Get in.** Two ways, and they are good at different things:

- `ssh_exec` runs on any guest OS, once it has an address and a running sshd.
- `guest_invoke_command` is Windows guests only, but it goes over the VMBus, so
  it works with **no guest network at all**. This is how you bootstrap a guest
  that cannot be reached yet.

Store the credentials once and neither needs them again:

```powershell
hypervm-mcp cred set --vm dev-box --user dev --ssh-key ~\.ssh\id_ed25519
```

**4. Reach a service inside it.** `open_tunnel` forwards a host port into the
guest. Use `mode=ssh` when the service is bound to the guest's own `127.0.0.1`,
because nothing else can reach that.

```
open_tunnel  vm_name=dev-box  guest_port=80  mode=ssh  bind_scope=loopback
```

Tunnels live in the service, not your conversation. They survive it ending, are
restored when the service restarts, and find the guest again when it reboots onto
a new address.

**5. When something goes wrong, look.** `capture_vm_screen` photographs the
console from the host side. It needs no agent, no network and no operating
system, so it works on a firmware prompt, a boot menu or a stop error, exactly
when nothing else can tell you anything.

---

## The three ways to reach a service

Your VM is running something (a web server, a database, a remote desktop) and you
want to use it. There are three routes in, and picking the wrong one wastes an
afternoon, so the tools say which applies.

| | When |
|---|---|
| **Connect directly** to the guest's address | Binds no host port, so what the host is already listening on is irrelevant. Guest SMB on 445 works this way over the Default Switch. |
| **A tunnel** forwards a host port in | Anything whose client can be pointed at a port. `mode=ssh` reaches services on the guest's own loopback; `bind_scope` decides who may connect: this host, your tailnet, or everything. |
| **An External switch** gives the guest its own LAN address | Only when *other machines* must reach it, or the host already holds the port (SMB, RDP, WinRM), or the protocol cares about host identity. |

> [!CAUTION]
> **Creating an External switch is the one thing here that can break your
> machine.** Hyper-V rebinds the physical adapter, dropping host networking for
> several seconds, and a static address that fails to migrate leaves the host
> unreachable except from the console.
>
> `create_switch` refuses until `confirm_disruption` is set, and the refusal
> reports which risks apply to *your* host: whether that adapter is your only
> uplink, whether its address is static, whether it is wireless and so will not
> truly bridge anyway, and what happens to Tailscale.
> `preflight_external_switch` shows the same report and changes nothing.
>
> **This path is unverified.** See [What is verified](#what-is-verified).

---

## Things that will bite you

**A VM's name is an identity here, not a label.** Credentials, the pinned SSH
host key and every tunnel are filed under it. Rename a VM in Hyper-V Manager and
this server stops recognising it: no credentials, no pin (so the next connection
looks like a first sighting and a changed key is trusted in silence), and tunnels
that fail the next time they look the guest up. Use `rename_vm`, which carries all
three across. It does not rename files on disk: the folder, the disks and the
checkpoints keep the old name.

**Guest IPs need an agent inside the guest.** Hyper-V does not discover them, it
reads what an agent reports. A minimal Linux install ships without one, so a
perfectly healthy VM reports nothing and `wait_for_guest_ip` times out. Install
`hyperv-daemons` and reboot. Until then `ssh_exec`, `open_tunnel` and
`diagnose_vm_network` all take the address directly.

**An Internal switch has no DHCP.** The Default Switch arrives with an address,
NAT and DHCP already arranged, which hides that all three are normally your job.
One you create has none: `set_switch_host_address` for the host,
`set_guest_static_ip` per guest, `set_switch_nat` if they need the internet.

**A template must stay powered off while its clones run.** A running VM holds its
disk exclusively and a differencing disk cannot read a parent held that way. The
clone fails with a file-in-use error that never mentions differencing disks.

**Nested virtualization only takes on a stopped VM, and Hyper-V will not tell
you.** `Set-VMProcessor -ExposeVirtualizationExtensions` on a running VM reports
success and changes nothing: the setting reads back unchanged, and still
unchanged once the VM stops. `set_vm_nested_virtualization` refuses a VM that is
not Off rather than pass that success on. It also turns dynamic memory off, which
Hyper-V does not require but a guest hypervisor does; the returned detail shows
the new value. And nested guests reach the network only if the outer VM's adapter
allows MAC spoofing, which is `set_vm_network`'s `mac_spoofing`.

**Paths are opened by the service, not by you.** It runs as LocalSystem with its
own logon session, so your mapped drive letters do not exist for it. Use UNC and
grant `HOSTNAME$` access to the share. Every path is checked before anything is
created.

---

## Tools

Around fifty of them. You do not need to memorise any: your agent picks. Skim the
groups so you know what is possible, and open one when you want to ask for
something by name.

<details>
<summary><b>Lifecycle and checkpoints</b></summary>

| Tool | |
|---|---|
| `list_vms` `get_vm` | Inventory and full detail |
| `start_vm` `stop_vm` `restart_vm` | Power control; graceful unless `force` |
| `suspend_vm` `resume_vm` | Save to disk or pause in memory |
| `wait_for_guest_ip` | Wait until the guest is actually reachable |
| `set_vm_nested_virtualization` | Let the guest run its own hypervisor: Hyper-V, WSL2, Docker Desktop, Windows Sandbox |
| `create_checkpoint` `list_checkpoints` | Snapshot and inspect |
| `apply_checkpoint` | Revert, discarding everything since |
| `delete_checkpoint` | Remove one and wait for its disk to merge |
</details>

<details>
<summary><b>VM settings</b></summary>

`get_vm_settings` reads the whole configuration in one call, in the same vocabulary every setter accepts, so what it reports can be sent straight back.

| Tool | |
|---|---|
| `get_vm_settings` | Everything on every property page, in one read |
| `set_vm_memory` | Startup, dynamic range, buffer, priority, applied together |
| `set_vm_processor` | Count, reserve, cap, weight, SMT visibility, migration compatibility |
| `set_vm_firmware` | Boot order, secure boot template, console mode; Generation 1 BIOS too |
| `set_vm_options` | Automatic start and stop, checkpoint policy, file locations, enhanced session |
| `set_vm_integration_services` | The VMBus services other tools here depend on |
| `set_vm_security` | A virtual TPM, which Windows 11 will not install without |
| `set_vm_video` | Pin the console resolution `capture_vm_screen` photographs |
| `set_vm_com_port` | A serial console over a named pipe, needing no guest network |
| `set_vm_disk_settings` | Storage QoS in normalized IOPS, and controller placement |
</details>

<details>
<summary><b>Provisioning and storage</b></summary>

| Tool | |
|---|---|
| `create_vm` `delete_vm` | Independent paths for config, disk, checkpoints, paging |
| `rename_vm` | Renames the VM and moves the credentials, host key and tunnels filed under it |
| `create_vhd` `attach_vhd` `detach_vhd` | Disks sized in MB, on a controller port you choose |
| `add_scsi_controller` | A second controller, past 64 disks |
| `attach_iso` `eject_dvd` | Installation media in and out |
| `create_seed_disk` | An answer-file disk for an unattended install |
| `create_vm_from_template` | Clone from a golden image in seconds |
| `export_vm` `import_vm` | Produce an image, or move a VM's fixed config path |
| `get_vhd_info` `resize_vhd` `convert_vhd` | Inspect and reshape |
| `get_host_storage_paths` `set_host_storage_paths` | Where things land by default |
</details>

<details>
<summary><b>Networking</b></summary>

| Tool | |
|---|---|
| `list_switches` `create_switch` `delete_switch` | Virtual switches |
| `set_switch_host_address` `set_switch_nat` | The host's side of an Internal switch |
| `set_vm_network` | Switch, static MAC, VLAN, MAC spoofing, extra adapters |
| `set_vm_network_advanced` | DHCP and router guard, bandwidth, offloads, mirroring, VLAN trunk |
| `remove_vm_network_adapter` | Take an adapter away again |
| `set_guest_static_ip` | A fixed address inside the guest |
| `list_physical_adapters` `preflight_external_switch` | Before touching a physical NIC |
| `diagnose_vm_network` | What can reach this VM, and what cannot |
</details>

<details>
<summary><b>Guest access</b></summary>

| Tool | |
|---|---|
| `ssh_exec` `ssh_info` `ssh_forget_host_key` | Commands over SSH, host keys pinned per VM |
| `guest_invoke_command` `guest_copy_file` `guest_copy_from` | Over the VMBus, with no guest network at all |
| `guest_run_in_session` | On the guest's desktop, elevated, where windows are drawn |
| `capture_vm_screen` | A picture of the console, needing nothing inside the guest |
| `send_vm_key` `send_vm_mouse` | The console's own keyboard and pointer |
| `get_guest_network` | Adapters and reported addresses |
| `open_tunnel` `list_tunnels` `close_tunnel` | Forward a host port into a VM |
| `tailscale_serve` `tailnet_status` | HTTPS at your MagicDNS name |
| `doctor` | Check everything and say what to fix |
</details>

---

## Testing Windows software in a VM

<details>
<summary><b>Programs that need administrator</b></summary>

There is no prompt to click, because there is no UAC on this path.
`guest_invoke_command` reaches the guest over the VMBus, where the session is
created by a service running as SYSTEM and gets an unfiltered administrator
token. Installing features, writing under `HKLM`, changing services and setting
ACLs all just work.

```
guest_invoke_command  vm_name=win-test
                      command="Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0"
```

**Use the built-in Administrator.** It is the one local account never subject to
token filtering. Another local admin may come back with a filtered token, which
looks like a member of the group right up until something needs the privilege. If
you must use one, set `LocalAccountTokenFilterPolicy` to 1 in the guest, or go
through `guest_run_in_session`, which asks for the highest run level explicitly.

**Elevation is not the hard part, the session is.** `guest_invoke_command` runs
in session 0, which has no desktop. A console program does not care. A program
that opens a window will be drawn nowhere at all.
</details>

<details>
<summary><b>Verifying a GUI</b></summary>

Four separate problems:

**Somewhere to draw.** Desktop Experience, not Server Core, and automatic logon so
an interactive session exists and survives reboots. Turn off the lock screen,
screen saver and display power-down: a blanked screen captures as black and takes
no clicks, which reads as a broken test rather than a sleeping desktop. The answer
file in `internal/e2e` does all of this.

**A way to run there.** `guest_run_in_session` registers a scheduled task for the
logged-on user with an interactive logon type, at the highest run level. It is the
only way here to drive a program that needs administrator *and* shows a window.

**A way to see.** `capture_vm_screen` reads the console frame buffer from the host.
The image comes back inline, so a client that can look at pictures does not have to
open a file.

**A way to act.** `send_vm_key` and `send_vm_mouse` drive the console's own
keyboard and pointer. Give mouse coordinates in pixels along with the size of the
capture they came from; the scaling to the guest's real resolution is done for you.

> [!IMPORTANT]
> **Read screens, do not compare them.** The capture is a scaled thumbnail and
> pixels move with resolution, theme, DPI and font. Decide whether a GUI is
> correct by querying its automation tree from `guest_run_in_session`
> (`UIAutomationClient`, or FlaUI); use screenshots to find out what went wrong.

For a human to watch, forward RDP instead: `open_tunnel` to guest port 3389,
`bind_scope=tailnet` to watch from another machine.
</details>

<details>
<summary><b>Which tools work on which guest</b></summary>

The console tools drive Hyper-V's own devices rather than anything inside the VM,
so how far they generalise depends on how much of the guest each one needs.

| | Needs from the guest | Run against |
|---|---|---|
| `capture_vm_screen` | Nothing at all | Windows, Linux, firmware with no OS |
| `send_vm_key` | Something listening, firmware counts | Windows, Linux, firmware with no OS |
| `send_vm_mouse` | A bound pointer, which firmware has not | Windows, Linux |
| `guest_invoke_command` | PowerShell Direct | Windows only; use `ssh_exec` on Linux |
| `guest_run_in_session` | PowerShell Direct and a desktop | Windows only |
| `guest_copy_file` | The Guest Service Interface | Both; Linux needs `hypervfcopyd` |
| `guest_copy_from` | PowerShell Direct | Windows only; one file at a time |

Give no `width`/`height` to `capture_vm_screen` and it uses the console's own
resolution, the only size always accepted. A Generation 1 firmware screen is
640x480-ish, nowhere near a plausible default.

The pointer is a synthetic device on Generation 2 VMs and an emulated PS/2 one on
Generation 1; both are handled. Keys are Windows virtual-key codes that Hyper-V
turns into scancodes, so letters and navigation keys are safe everywhere while
symbols follow the guest's keyboard layout.

A guest with no Hyper-V integration drivers still captures, and will likely still
take keys at the firmware level, but its pointer will not bind. That case is
untested here.
</details>

---

## Operating it

<details>
<summary><b>Credentials and host keys</b></summary>

Guest credentials are stored by the service, encrypted with machine-scope DPAPI,
so they never travel through a conversation:

```powershell
hypervm-mcp cred set --vm dev-box --user dev --ssh-key ~\.ssh\id_ed25519
hypervm-mcp cred list     # what is on file, never the secrets
```

The CLI hands them over the pipe rather than writing the file, because the two
ends run as different accounts: the CLI is you, and only the service can write its
own data directory.

SSH host keys are pinned per VM name on first connect. A later mismatch fails
until you pass `trust_new_key`, the expected path after rebuilding a VM. Keying
on the name rather than the address is deliberate: an address-keyed store would
see a new host after every reboot and pin nothing.
</details>

<details>
<summary><b>CLI, file locations, troubleshooting</b></summary>

```
hypervm-mcp bridge                  Relay MCP traffic (run by MCP clients)
hypervm-mcp service install         Install and start (one UAC prompt)
hypervm-mcp service uninstall       Remove; --purge also deletes stored data
hypervm-mcp service start | stop | status
hypervm-mcp update                  Install the latest release over this one
hypervm-mcp cred set | list | delete
hypervm-mcp tunnel list
hypervm-mcp doctor                  Check the setup and report what to fix
hypervm-mcp version
```

```
%ProgramData%\hypervm-mcp\
  bin\hypervm-mcp.exe    the binary the service runs
  config.json            pipe name, allowed SID, PowerShell path, limits
  credentials.dat        guest credentials, DPAPI machine scope
  known_hosts.json       pinned SSH host keys, by VM name
  tunnels.json           tunnel definitions, reopened on restart
  logs\service.log       warnings and errors also go to the event log
```

The directory's ACL is explicit and non-inherited: LocalSystem and Administrators
get full control, the installing user read-only. The binary lives here rather than
in your build tree because anything writable by a non-admin that a LocalSystem
service executes is a privilege escalation.

Run `hypervm-mcp doctor` first. It checks Hyper-V, the pipe, storage paths,
switches, credentials, Tailscale and every open tunnel, and says what to do.

| Symptom | |
|---|---|
| *"the hypervm-mcp service is not running"* | `hypervm-mcp service start` |
| *"access to \\\\.\\pipe\\hypervm-mcp was denied"* | The pipe accepts only the account it was installed for. Reinstall as that account. |
| `ACCESS_DENIED` *"Hyper-V refused the caller"* | PowerShell ran without Hyper-V rights; check the service runs as LocalSystem. |
| `GUEST_IP_UNAVAILABLE` | Usually the missing guest agent above. |
| `SSH_HOST_KEY_MISMATCH` | Expected after rebuilding a VM. Pass `trust_new_key`, or `ssh_forget_host_key`. |
| `PORT_IN_USE` on 445, 3389, 5985 | Windows holds those; they cannot be tunnelled. |
</details>

---

## How it works

An unprivileged bridge relays MCP over a named pipe to a LocalSystem service that
holds the Hyper-V rights. The pipe's DACL is protected and names your account
alone, so the boundary is enforced by Windows rather than by this code being
careful.

<details>
<summary><b>Design notes</b></summary>

**Arguments never enter script text.** Every value is sent as JSON on stdin and
read back as `$P` inside the PowerShell script. A VM named
`Dev"; Remove-Item C:\ -Recurse` is a string, not code. There is a test for
exactly that.

**Success is a field, not an exit code.** PowerShell leaves the exit code at 0 for
non-terminating errors, so scripts are wrapped in an envelope reporting `ok`
explicitly, with `$ErrorActionPreference = 'Stop'` promoting everything to
terminating.

**Projections build hashtables, never `Select-Object` calculated properties.**
Select-Object flattens its expression's output: an array of one becomes a scalar
and an empty one becomes `{}`. Both decode wrongly, and the one-element case is
the common one, since a VM usually has exactly one IP address.

**Nothing depends on the host's display language.** Hyper-V localizes its
messages, so "VM not found" arrives in Korean on a Korean Windows. Outcomes are
decided by error *category* and by cmdlet id, both locale-independent, with an
`en-US` pin so anything reaching the text-matching fallback is English.
</details>

<details>
<summary><b>Developing against an installed copy</b></summary>

If you already use hypervm-mcp for real work, you do not want a development build
restarting that service, reading its credentials, or answering on its pipe. Build
with an instance name and it takes a separate identity throughout:

```powershell
go build -ldflags "-X github.com/heavycaffeiner/hypervm-mcp/internal/config.instance=dev" `
  -o bin\hypervm-mcp-dev.exe .\cmd\hypervm-mcp
.\bin\hypervm-mcp-dev.exe service install
```

| | release build | `instance=dev` |
|---|---|---|
| Service | `hypervm-mcp` | `hypervm-mcp-dev` |
| Pipe | `\\.\pipe\hypervm-mcp` | `\\.\pipe\hypervm-mcp-dev` |
| Data directory | `%ProgramData%\hypervm-mcp` | `%ProgramData%\hypervm-mcp-dev` |
| Event log source | `hypervm-mcp` | `hypervm-mcp-dev` |
| Firewall rules and NATs | `hypervm-mcp-*` | `hypervm-mcp-dev-*` |
| MCP server name | `hypervm-mcp` | `hypervm-mcp-dev` |

Release builds pass no flag, so their names are unchanged. Credentials and pinned
host keys do not carry over: the dev instance starts empty, which is the point.

**Hyper-V itself is not isolated.** Both instances drive the same hypervisor and
see the same virtual machines. Keeping their VM names apart is still up to you.
</details>

<details>
<summary><b>Tests</b></summary>

```powershell
go test ./...
```

Everything touching real infrastructure is opt-in, because it costs a VM.

```powershell
$env:HYPERVM_E2E = "$env:ProgramData\hypervm-mcp\bin\hypervm-mcp.exe"
go test ./internal/e2e -count=1 -v          # against the installed service

$env:HYPERVM_E2E_ROCKY = "1"
$env:HYPERVM_E2E_INSTALL = "1"              # rebuilds the VM; separately gated
go test ./internal/e2e -run Rocky -count=1 -v -timeout 90m
```

`TestRockyProvision` installs Rocky Linux 10 unattended from a kickstart on an
`OEMDRV` seed disk, which Anaconda finds on its own with no boot parameter and no
rebuilt ISO. The rest build on that guest: nginx behind an SSH tunnel, SMB from
the host, a RAID array across attached disks, a private three-way network.

Testing a dev instance needs the same `-ldflags` on the test binary, because it
resolves the pipe name the same way the server does.
</details>

---

## What is verified

Everything here is implemented. This says which parts have been run against real
hardware, because on a public server that is a different claim.

<details open>
<summary><b>Exercised end to end</b></summary>

Against **Rocky Linux 10** on Windows 11:

| | |
|---|---|
| Unattended install | Kickstart on an `OEMDRV` seed disk, 3.5 minutes, no console input |
| SSH tunnel | nginx bound to the guest's `127.0.0.1`, with a `direct` tunnel proven to fail first |
| Tailnet tunnel | Both tailnet addresses; firewall rule created and removed, checked against Windows |
| `tailscale serve` | Served at the MagicDNS name over HTTPS |
| Guest SMB from the host | Read and write over the Default Switch, with the 445 tunnel refused alongside |
| Checkpoints | Snapshot, change the guest, revert, confirm the change is gone, merge |
| Golden-image clone | A 32 GB VM in 6.9 seconds; deleting the clone leaves the image intact |
| Disks and RAID | Four 512 MB fixed disks at chosen ports, assembled into RAID5 by the guest |
| ISO mount | Attached to a running VM, mounted and read by the guest |
| Private network | Internal switch, static IPs, host and two guests all reaching each other, plus NAT |
| Network diagnosis | Port probes, and telling "no address reported" apart from "unreachable" |
| Console capture | The text console, and a Wayland session, both read from the host |
| Console keyboard | Keys reach a Linux text console; the frame changes in response |
| Console pointer | Four positions across the screen, each confirmed at the guest's own input device |

Against **Windows Server 2022** (Desktop Experience):

| | |
|---|---|
| Unattended install | `autounattend.xml` on its own small ISO, no console input |
| `guest_invoke_command` | PowerShell Direct over the VMBus, before the guest had any network service |
| SSH bootstrap | OpenSSH installed, started, keyed and firewalled entirely over the VMBus, then reached over TCP |
| `guest_copy_file` | Host to guest over the VMBus |
| `guest_copy_from` | Guest to host over the VMBus, with size and SHA256 verified |
| `set_guest_static_ip` | The Windows branch, on a second adapter named `Ethernet 2` |
| Session bridge | The same query answered as session 0 over PowerShell Direct and session 1 through `guest_run_in_session` |
| Elevation | An unfiltered administrator token in that session, proven by writing under `HKLM` |
| Console capture and pointer | A 1024x768 desktop; the cursor confirmed at the pixel it was sent to |

With **no guest at all**:

| | |
|---|---|
| Generation 1 and 2 | Both firmware screens captured, both took keys, both refused the pointer with a reason |
| Nested virtualization | Enabled on a stopped VM with dynamic memory turned off alongside, read back through `get_vm`, then a running VM refused and left untouched |
| Every VM setting | Each one written and read back through the same vocabulary: memory, processor, boot order, secure boot, automatic actions, checkpoint policy, integration services, vTPM, console resolution, serial port, disk QoS and placement, adapter guards, bandwidth and VLAN trunk |
| Both firmware families | UEFI boot order set by device token on Generation 2; the BIOS permutation completed from one named class on Generation 1, with secure boot and a vTPM refused there for the right reason |

And `rename_vm`: credentials and the pinned host key both confirmed under the new
name, the key by comparing fingerprints, because a lost pin is silent and would
otherwise look identical to a carried one.

The answer file deliberately does not install OpenSSH. Putting sshd there would
have made the bootstrap test prove nothing. The GUI tests assert on the automation
tree, never on pixels; a screenshot is saved on the way past either way, because
when something does go wrong it is the only record of what the guest was showing.
</details>

<details>
<summary><b>Implemented but never run</b></summary>

- **Creating or deleting an External switch.** Running it means disconnecting a
  working machine, so it is left for a deliberate session with console access. Its
  guards and preflight are tested; the creation is not.
- **`export_vm` / `import_vm` / `resize_vhd` / `convert_vhd`.**
- **A hypervisor actually running inside a guest.** What is measured is the
  setting and its guards, not WSL2 or Hyper-V booting in there. That also needs a
  host CPU that can do it, and a Hyper-V host reports its own virtualization
  flags as false, so there is no preflight worth trusting to offer.
- **Guests other than the two above.** Other Windows versions and Linux
  distributions should work by construction, since the guest-side requirement is
  only a driver both families ship in-tree, but that is an expectation, not a
  measurement.
</details>

<details>
<summary><b>Known environment limits, not defects</b></summary>

- `guest_copy_file` cannot work on Linux 6.10 or later, which removed the
  `/dev/vmbus/hv_fcopy` device `hypervfcopyd` attaches to. The daemon still
  reports itself active. The error says so and points at `ssh_exec`.
- Rocky 10 ships no X server, since RHEL 10 dropped it, so the Linux GUI tests run
  a Wayland compositor, and read the pointer from the kernel input device because
  Wayland will not tell a client where it is.
</details>

---

## License

MIT. See [LICENSE](LICENSE).
