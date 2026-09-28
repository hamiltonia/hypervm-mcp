//go:build windows

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file covers the Windows guest, which reaches the same places as the Rocky
// tests by an entirely different road.
//
// A Linux guest is configured by putting a kickstart on a disk and talking to it
// over SSH afterwards. A Windows guest has neither: the answer file has to arrive
// on removable media, and there is no SSH until something installs it. That
// something is PowerShell Direct, which runs over the VMBus and needs no guest
// address at all — so the interesting claim here is that a VM with no network
// service of its own can be given one from the host.
//
// Opt in; the install takes a while:
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp-dev\bin\hypervm-mcp-dev.exe"
//	$env:HYPERVM_E2E_WINDOWS="1"
//	$env:HYPERVM_E2E_INSTALL="1"   # only for TestWindowsProvision, which rebuilds the VM
//	go test ./internal/e2e -run Windows -v -count=1 -timeout 120m `
//	  -ldflags "-X github.com/heavycaffeiner/hypervm-mcp/internal/config.instance=dev"
//
// The -ldflags matter when testing a dev instance: credentials are stored by
// talking to the service's pipe directly, and the test binary resolves that name
// the same way the server does. Without the flag it would write into the
// installed service's store instead.

// readyMarker is written by the answer file's FirstLogonCommands. Setup reports
// itself finished well before the guest is usable, so this is what "ready"
// actually means here.
const readyMarker = `C:\hypervm-mcp-ready.txt`

func requireWindowsGuest(t *testing.T) {
	t.Helper()
	if os.Getenv("HYPERVM_E2E_WINDOWS") == "" {
		t.Skip("set HYPERVM_E2E_WINDOWS=1 to run the Windows guest tests")
	}
}

// TestWindowsProvision installs Windows Server 2022 with no console interaction.
//
// Gated separately from the rest of the Windows group for the same reason the
// Rocky one is: it destroys and rebuilds the VM the others configure.
func TestWindowsProvision(t *testing.T) {
	requireWindowsGuest(t)
	if os.Getenv("HYPERVM_E2E_INSTALL") == "" {
		t.Skip("set HYPERVM_E2E_INSTALL=1 to run; this deletes and reinstalls the VM")
	}
	if _, err := os.Stat(winISO); err != nil {
		t.Fatalf("Windows ISO not found at %s: %v", winISO, err)
	}
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Minute)
	defer cancel()

	// Start from nothing: a rerun over an installed disk would leave the firmware
	// with two bootable devices, and Setup would sit on "press any key to boot
	// from CD" with nobody to press it.
	if err := tryCall(t, session, ctx, "delete_vm",
		map[string]any{"name": winVMName, "delete_disks": true, "force": true}); err == nil {
		t.Log("removed a previous VM")
	}
	_ = tryCall(t, session, ctx, "ssh_forget_host_key", map[string]any{"name": winVMName})

	buildAnswerISO(t)

	var vm map[string]any
	call(t, session, ctx, "create_vm", map[string]any{
		"name":        winVMName,
		"generation":  2,
		"memory_mb":   4096,
		"cpu_count":   2,
		"vm_path":     winVMPath,
		"vhd_path":    winVHD,
		"vhd_size_mb": 40960,
		"switch_name": winSwitch,
		"iso_path":    winISO,
		// Windows Server is signed by the Microsoft UEFI CA, so the default
		// template applies — unlike Rocky, which needs the third-party one.
		"secure_boot":    "windows",
		"create_parents": true,
	}, &vm)
	t.Logf("created %v at %v", vm["name"], vm["configuration_location"])

	// The answer file rides its own drive. Setup scans every removable device for
	// autounattend.xml, so it does not have to be on the installation media —
	// which is the only reason this works without rebuilding a 4.7 GB ISO.
	call(t, session, ctx, "attach_iso", map[string]any{
		"vm_name": winVMName, "iso_path": winAnswerISO,
	}, nil)

	call(t, session, ctx, "start_vm", map[string]any{"name": winVMName}, nil)

	// Insurance, not a required step. Microsoft's media asks "Press any key to
	// boot from CD or DVD"; nothing inside the guest can answer that, because
	// there is no guest yet. Installs here have gone through without it, but one
	// run stalled with Setup never starting and the disk never growing past its
	// header, which is what that prompt timing out looks like from outside.
	//
	// Pressing space across the first half-minute costs nothing when the prompt
	// does not appear, and the presses land long before the reboot that follows
	// the image being applied — so this cannot restart the installer.
	var keys map[string]any
	call(t, session, ctx, "send_vm_key", map[string]any{
		"vm_name": winVMName, "keys": []string{"space"},
		"repeat": 30, "interval_ms": 1000,
	}, &keys)
	t.Logf("pressed space: %v accepted, %v refused before the keyboard attached",
		keys["sent"], keys["rejected"])

	t.Log("installing; Windows reboots several times and never powers itself off, " +
		"so readiness is judged by the marker the answer file writes")

	waitForWindowsReady(t, session, ctx, 90*time.Minute)
	finishProvision(t, session, ctx)
}

// finishProvision is the tidy-up that has to happen once, after the install
// finishes, whichever test happened to be watching when it did.
func finishProvision(t *testing.T, s *mcp.ClientSession, ctx context.Context) {
	t.Helper()

	// Take the media away. The DVD is still the first boot device, so leaving it
	// in means the next reboot offers to install over the system that was just
	// built.
	call(t, s, ctx, "eject_dvd", map[string]any{"name": winVMName}, nil)

	var ip map[string]any
	call(t, s, ctx, "wait_for_guest_ip", map[string]any{
		"name": winVMName, "timeout_seconds": 300,
	}, &ip)
	t.Logf("guest is up at %v (waited %.0fs)", ip["address"], ip["waited_seconds"])
}

// TestWindowsWaitReady waits for an install that is already under way.
//
// Provisioning and waiting are separated because the install outlives any one
// test run: if the harness is interrupted the guest keeps going, and rebuilding
// it from scratch to find out how it got on would throw away the half hour it
// has already spent.
func TestWindowsWaitReady(t *testing.T) {
	requireWindowsGuest(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	waitForWindowsReady(t, session, ctx, 80*time.Minute)
	finishProvision(t, session, ctx)
}

// buildAnswerISO writes autounattend.xml to a small ISO on the host.
//
// This runs PowerShell directly rather than through the server: putting media in
// front of a VM is the server's job, authoring it is the harness's.
func buildAnswerISO(t *testing.T) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(winAnswerISO), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(winAnswerISO), err)
	}
	xml := autounattendXML(winAdmin, winPassword)

	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive",
		"-Command", buildAnswerISOScript(winAnswerISO, xml))
	// Passed through the environment rather than embedded in the script, so a
	// password with quoting-significant characters cannot break it.
	cmd.Env = append(os.Environ(), "HYPERVM_ANSWER="+xml)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build the answer ISO: %v\n%s", err, out)
	}
	t.Logf("answer ISO: %s (%s bytes)", winAnswerISO, strings.TrimSpace(string(out)))
}

// waitForWindowsReady polls PowerShell Direct until the guest reports the marker.
//
// Every call before the guest finishes is expected to fail — the endpoint does
// not exist during Setup, and the account cannot authenticate until it has logged
// on once — so failures are progress, not errors, right up until the deadline.
func waitForWindowsReady(t *testing.T, s *mcp.ClientSession, ctx context.Context, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	started := time.Now()
	lastErr := ""

	for {
		out, err := winExecErr(s, ctx, "if (Test-Path '"+readyMarker+"') { 'ready' } else { 'waiting' }", 60)
		switch {
		case err == nil && strings.Contains(out, "ready"):
			t.Logf("[%s] guest ready after %s", time.Now().Format("15:04:05"),
				time.Since(started).Round(time.Second))
			return
		case err == nil:
			t.Logf("[%s] PowerShell Direct answers; first logon still running",
				time.Now().Format("15:04:05"))
		default:
			// Collapse the repeated identical errors a long install produces.
			if msg := firstLine(err.Error()); msg != lastErr {
				t.Logf("[%s] not reachable yet: %s", time.Now().Format("15:04:05"), msg)
				lastErr = msg
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("the guest never reported ready within %s (last: %s)", timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			t.Fatal("cancelled while waiting for the guest")
		case <-time.After(30 * time.Second):
		}
	}
}

// TestWindowsBootstrapSSH gives a Windows guest an SSH server it did not have.
//
// The answer file deliberately does not install OpenSSH, so everything here goes
// in over the VMBus. That is the point: the host configures a guest it has no
// network service to talk to, and only afterwards is there one.
func TestWindowsBootstrapSSH(t *testing.T) {
	requireWindowsGuest(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	privateKey, authorizedKey := sshKeyPair(t)

	t.Logf("PowerShell Direct reaches %s",
		strings.TrimSpace(winExec(t, session, ctx, "$env:COMPUTERNAME", 60)))

	// OpenSSH Server ships as a Feature on Demand. Server Core has it available
	// but not installed, and it comes from Windows Update, so the guest needs
	// working outbound network for this step alone.
	t.Log("installing OpenSSH.Server (Feature on Demand; this pulls from Windows Update)")
	out := winExec(t, session, ctx, `
        $cap = Get-WindowsCapability -Online -Name 'OpenSSH.Server*'
        if ($cap.State -ne 'Installed') { Add-WindowsCapability -Online -Name $cap.Name | Out-Null }
        (Get-WindowsCapability -Online -Name 'OpenSSH.Server*').State`, 20*60)
	if !strings.Contains(out, "Installed") {
		t.Fatalf("OpenSSH.Server did not install: %s", out)
	}

	t.Log("starting sshd and opening port 22 inside the guest")
	winExec(t, session, ctx, `
        Set-Service -Name sshd -StartupType Automatic
        Start-Service sshd
        if (-not (Get-NetFirewallRule -Name 'sshd-e2e' -ErrorAction SilentlyContinue)) {
            New-NetFirewallRule -Name 'sshd-e2e' -DisplayName 'OpenSSH Server (e2e)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 | Out-Null
        }
        (Get-Service sshd).Status`, 300)

	// An administrator's key does not go in the profile's authorized_keys —
	// sshd on Windows reads a single machine-wide file for every account in the
	// Administrators group, and refuses it outright if anyone else can write it.
	t.Log("authorising the test key")
	winExec(t, session, ctx, fmt.Sprintf(`
        $f = 'C:\ProgramData\ssh\administrators_authorized_keys'
        Set-Content -Path $f -Value %s -Encoding ascii
        # By SID, not by name: the built-in groups are localized, and sshd
        # rejects the file outright if the ACL is not exactly these two.
        icacls $f /inheritance:r /grant '*S-1-5-32-544:F' /grant '*S-1-5-18:F' | Out-Null
        Get-Content $f`, psQuote(authorizedKey)), 300)

	var ip map[string]any
	call(t, session, ctx, "wait_for_guest_ip", map[string]any{
		"name": winVMName, "timeout_seconds": 300,
	}, &ip)
	host, _ := ip["address"].(string)
	if host == "" {
		t.Fatalf("no guest address reported: %v", ip)
	}
	t.Logf("guest address: %s", host)

	storeCredentialsAs(t, winVMName, winAdmin, winPassword, privateKey)
	waitForSSHVM(t, session, ctx, winVMName, host, 5*time.Minute)

	// The round trip that proves it: a command that started life reachable only
	// over the VMBus is now answered over TCP.
	got := sshRunVM(t, session, ctx, winVMName, host, "hostname")
	if !strings.EqualFold(strings.TrimSpace(got), winVMName) {
		t.Fatalf("SSH reached %q, want %q", strings.TrimSpace(got), winVMName)
	}
	t.Logf("SSH into the Windows guest answered: %s", strings.TrimSpace(got))
}

// TestWindowsGuestCopyFile copies a file in over the VMBus.
//
// Windows guests carry the Guest Service Interface in the box, so unlike the
// Rocky case there is no daemon to install first — which makes this the clean
// check that the copy path itself works.
func TestWindowsGuestCopyFile(t *testing.T) {
	requireWindowsGuest(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	hostDir := rockyArtifact
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatalf("create %s: %v", hostDir, err)
	}

	src := filepath.Join(hostDir, "win-copy-probe.txt")
	content := "copied over the VMBus at " + time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", src, err)
	}

	const dest = `C:\hypervm-mcp\win-copy-probe.txt`
	winExec(t, session, ctx, `Remove-Item -Recurse -Force 'C:\hypervm-mcp' -ErrorAction SilentlyContinue; 'cleared'`, 120)

	call(t, session, ctx, "guest_copy_file", map[string]any{
		"vm_name":          winVMName,
		"source_path":      src,
		"destination_path": dest,
		"create_full_path": true,
		"overwrite":        true,
	}, nil)

	// Read it back over the same channel it arrived on, so this test needs no
	// guest network of any kind.
	got := winExec(t, session, ctx, `Get-Content -Raw '`+dest+`'`, 120)
	if strings.TrimSpace(got) != content {
		t.Fatalf("the guest has %q, want %q", strings.TrimSpace(got), content)
	}
	t.Logf("file arrived intact at %s", dest)

	winExec(t, session, ctx, `Remove-Item -Recurse -Force 'C:\hypervm-mcp'; 'cleaned'`, 120)
}

// TestWindowsGuestCopyFrom retrieves a known file over PowerShell Direct,
// verifies the digest reported by the tool, and exercises both overwrite paths.
func TestWindowsGuestCopyFrom(t *testing.T) {
	requireWindowsGuest(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	privateKey, _ := sshKeyPair(t)
	storeCredentialsAs(t, winVMName, winAdmin, winPassword, privateKey)

	const guestPath = `C:\hypervm-mcp\copy-from-probe.txt`
	hostDir := filepath.Join(rockyArtifact, "guest-copy-from")
	hostPath := filepath.Join(hostDir, "retrieved.txt")
	_ = os.RemoveAll(hostDir)
	t.Cleanup(func() {
		_ = os.RemoveAll(hostDir)
		_, _ = winExecErr(session, context.Background(),
			`Remove-Item -LiteralPath '`+guestPath+`' -Force -ErrorAction SilentlyContinue`, 120)
	})

	writeGuest := func(content string) {
		t.Helper()
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		winExec(t, session, ctx,
			`[IO.Directory]::CreateDirectory('C:\hypervm-mcp') | Out-Null; `+
				`[IO.File]::WriteAllBytes('`+guestPath+`',[Convert]::FromBase64String('`+encoded+`')); 'written'`,
			120)
	}

	first := "known guest payload\n"
	writeGuest(first)
	var copied struct {
		SizeBytes int64  `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	}
	call(t, session, ctx, "guest_copy_from", map[string]any{
		"vm_name":          winVMName,
		"source_path":      guestPath,
		"destination_path": hostPath,
		"create_parents":   true,
	}, &copied)

	got, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read retrieved file: %v", err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(first)))
	if string(got) != first || copied.SizeBytes != int64(len(first)) ||
		copied.SHA256 != wantHash {
		t.Fatalf("copy metadata/content mismatch: content=%q result=%+v want sha256=%s", got, copied, wantHash)
	}

	if err := tryCall(t, session, ctx, "guest_copy_from", map[string]any{
		"vm_name":          winVMName,
		"source_path":      guestPath,
		"destination_path": hostPath,
	}); err == nil || !strings.Contains(err.Error(), "VM_ALREADY_EXISTS") {
		t.Fatalf("copy without overwrite got %v, want VM_ALREADY_EXISTS", err)
	}

	second := "replacement guest payload\n"
	writeGuest(second)
	call(t, session, ctx, "guest_copy_from", map[string]any{
		"vm_name":          winVMName,
		"source_path":      guestPath,
		"destination_path": hostPath,
		"overwrite":        true,
	}, &copied)
	got, err = os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read overwritten file: %v", err)
	}
	wantHash = fmt.Sprintf("%x", sha256.Sum256([]byte(second)))
	if string(got) != second || copied.SHA256 != wantHash {
		t.Fatalf("overwrite mismatch: content=%q result=%+v want sha256=%s", got, copied, wantHash)
	}
}

// TestWindowsStaticIP walks the Windows branch of set_guest_static_ip.
//
// It runs on a second adapter on an Internal switch, never the one carrying the
// guest's default route: changing that would cut the guest off from the network
// the rest of the group depends on.
func TestWindowsStaticIP(t *testing.T) {
	requireWindowsGuest(t)
	session, _ := connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	const (
		labAddr   = "10.77.0.21"
		labPrefix = 24
	)

	// The lab switch is the one the private-network test builds; create it if
	// this test runs alone.
	if err := tryCall(t, session, ctx, "create_switch", map[string]any{
		"name": labSwitch, "switch_type": "Internal",
	}); err != nil {
		t.Logf("lab switch already present: %v", err)
	}

	call(t, session, ctx, "set_vm_network", map[string]any{
		"vm_name": winVMName, "switch_name": labSwitch,
		"adapter_name": "lab", "create_adapter": true,
	}, nil)
	defer func() {
		// There is no adapter-removal tool, so leave it disconnected instead;
		// "-" is set_vm_network's way of saying no switch.
		_ = tryCall(t, session, context.Background(), "set_vm_network", map[string]any{
			"vm_name": winVMName, "adapter_name": "lab", "switch_name": "-",
		})
	}()

	// Hyper-V does not say which guest interface belongs to which adapter, so the
	// adapter is found by its MAC — the one thing both sides agree on.
	var detail map[string]any
	call(t, session, ctx, "get_vm", map[string]any{"name": winVMName}, &detail)
	mac := adapterMAC(t, detail, "lab")
	t.Logf("lab adapter MAC: %s", mac)

	iface := strings.TrimSpace(winExec(t, session, ctx, fmt.Sprintf(
		`(Get-NetAdapter | Where-Object { $_.MacAddress -replace '-','' -eq %s }).Name`,
		psQuote(strings.ToUpper(mac))), 300))
	if iface == "" {
		t.Fatalf("no guest interface carries MAC %s", mac)
	}
	t.Logf("guest calls it %q", iface)

	var res map[string]any
	call(t, session, ctx, "set_guest_static_ip", map[string]any{
		"vm_name":        winVMName,
		"address":        labAddr,
		"prefix_length":  labPrefix,
		"interface_name": iface,
		"username":       winAdmin,
		"password":       winPassword,
		// No gateway: this adapter must not take over the default route.
		"auto_checkpoint": false,
		"timeout_seconds": 180,
	}, &res)
	t.Logf("set_guest_static_ip: %v", res)

	// Confirm from inside the guest, then from the host across the switch.
	addrs := winExec(t, session, ctx, fmt.Sprintf(
		`(Get-NetIPAddress -InterfaceAlias %s -AddressFamily IPv4).IPAddress`, psQuote(iface)), 300)
	if !strings.Contains(addrs, labAddr) {
		t.Fatalf("the guest interface has %q, want %s", strings.TrimSpace(addrs), labAddr)
	}
	t.Logf("guest holds %s", labAddr)
}

// ---- helpers ---------------------------------------------------------------

// winExec runs a PowerShell command in the guest and fails the test if it errors.
func winExec(t *testing.T, s *mcp.ClientSession, ctx context.Context, command string, timeoutSeconds int) string {
	t.Helper()
	out, err := winExecErr(s, ctx, command, timeoutSeconds)
	if err != nil {
		t.Fatalf("guest_invoke_command: %v", err)
	}
	return out
}

// winExecErr is winExec for callers that expect failure, such as a poll during
// an install.
func winExecErr(s *mcp.ClientSession, ctx context.Context, command string, timeoutSeconds int) (string, error) {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{
		Name: "guest_invoke_command",
		Arguments: map[string]any{
			"vm_name":         winVMName,
			"command":         command,
			"username":        winAdmin,
			"password":        winPassword,
			"timeout_seconds": timeoutSeconds,
		},
	})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s", contentText(res))
	}
	var out struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode %s: %w", raw, err)
	}
	return out.Stdout, nil
}

// psQuote renders a Go string as a PowerShell single-quoted literal, so guest
// commands never depend on how the value is spelled.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// adapterMAC finds a named adapter's MAC in a get_vm result, without separators.
func adapterMAC(t *testing.T, detail map[string]any, adapter string) string {
	t.Helper()
	list, _ := detail["network_adapters"].([]any)
	for _, a := range list {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := m["name"].(string); !strings.EqualFold(name, adapter) {
			continue
		}
		mac, _ := m["mac_address"].(string)
		mac = strings.NewReplacer("-", "", ":", "").Replace(mac)
		if mac == "" || strings.Trim(mac, "0") == "" {
			t.Fatalf("adapter %q has no MAC yet; the VM may not have started since it was added", adapter)
		}
		return mac
	}
	t.Fatalf("no adapter named %q on the VM: %v", adapter, detail["network_adapters"])
	return ""
}

// firstLine trims a multi-line error down to something a progress log can repeat
// without burying the run.
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
