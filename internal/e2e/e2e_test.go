//go:build windows

// Package e2e drives the installed service exactly the way an MCP client does:
// it spawns `hypervm-mcp bridge` unprivileged and speaks MCP over its stdio.
//
// This is the test that proves the point of the whole design — an unprivileged
// process reaching Hyper-V with no elevation prompt. It needs the service to be
// installed, so it is opt-in:
//
//	$env:HYPERVM_E2E="C:\ProgramData\hypervm-mcp\bin\hypervm-mcp.exe"
//	go test ./internal/e2e -v
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connect(t *testing.T) (*mcp.ClientSession, context.Context) {
	t.Helper()

	exePath := os.Getenv("HYPERVM_E2E")
	if exePath == "" {
		t.Skip("set HYPERVM_E2E to the installed hypervm-mcp.exe to run these")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{
		Command: exec.Command(exePath, "bridge"),
	}, nil)
	if err != nil {
		t.Fatalf("connect through the bridge: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	return session, ctx
}

func TestToolsAreAdvertised(t *testing.T) {
	session, ctx := connect(t)

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	want := map[string]bool{
		"list_vms": false, "get_vm": false, "start_vm": false, "stop_vm": false,
		"restart_vm": false, "suspend_vm": false, "resume_vm": false,
		"wait_for_guest_ip": false, "guest_copy_from": false,
	}
	for _, tool := range res.Tools {
		t.Logf("  %-20s %s", tool.Name, tool.Title)
		want[tool.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q was not advertised", name)
		}
	}
}

// The service runs as LocalSystem, so this call must succeed even though the
// test process itself cannot query Hyper-V.
func TestListVMsThroughBridge(t *testing.T) {
	session, ctx := connect(t)

	var vms []map[string]any
	callList(t, session, ctx, "list_vms", map[string]any{}, &vms)
	t.Logf("%d VM(s)", len(vms))
	for _, vm := range vms {
		t.Logf("  %-30v %-8v gen%v", vm["name"], vm["state"], vm["generation"])
	}
}

// A missing VM must come back as a coded tool error, not a protocol failure.
func TestGetVMNotFound(t *testing.T) {
	session, ctx := connect(t)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_vm",
		Arguments: map[string]any{"name": "zzz-no-such-vm"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected a tool error")
	}
	t.Logf("error content: %s", contentText(res))
}

// TestLifecycle drives a real VM through start, suspend, resume and stop.
//
// It needs a VM you are willing to have power-cycled, named explicitly so it can
// never pick one by accident:
//
//	$env:HYPERVM_E2E_VM="Test-VM"
func TestLifecycle(t *testing.T) {
	session, ctx := connect(t)

	// Without a name, make one. Power control needs no operating system, so a
	// VM with no disk exercises every transition — and a test that builds its
	// own subject cannot be pointed at a guest the rest of the suite depends on,
	// which is what naming a shared VM here used to do.
	name := os.Getenv("HYPERVM_E2E_VM")
	if name == "" {
		name = "hypervm-lifecycle-probe"
		_ = tryCall(t, session, ctx, "delete_vm",
			map[string]any{"name": name, "delete_disks": true, "force": true})
		if err := tryCall(t, session, ctx, "create_vm", map[string]any{
			"name": name, "generation": 2, "memory_mb": 512, "cpu_count": 1,
			"secure_boot": "off",
		}); err != nil {
			t.Fatalf("create the disposable VM: %v", err)
		}
		defer func() {
			_ = tryCall(t, session, context.Background(), "delete_vm",
				map[string]any{"name": name, "delete_disks": true, "force": true})
		}()
		t.Logf("using a disposable VM named %s", name)
	}

	call := func(tool string, args map[string]any) map[string]any {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if res.IsError {
			t.Fatalf("%s: %s", tool, contentText(res))
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		t.Logf("%-18s -> state=%v", tool, out["state"])
		return out
	}

	// Put the VM back the way it was found. This test ends deliberately with the
	// VM off, and in a full run everything after it that needs this guest then
	// fails with an error about a missing address — which describes a symptom
	// three tests away from its cause.
	was := call("get_vm", map[string]any{"name": name})["state"]
	t.Logf("%s is %v; it will be left that way", name, was)
	defer func() {
		bg := context.Background()
		tool := "stop_vm"
		if was == "Running" || was == "Paused" {
			tool = "start_vm"
		}
		_, _ = session.CallTool(bg, &mcp.CallToolParams{
			Name: tool, Arguments: map[string]any{"name": name},
		})
	}()

	if got := call("start_vm", map[string]any{"name": name}); got["state"] != "Running" {
		t.Fatalf("after start_vm the VM is %v, want Running", got["state"])
	}
	if got := call("suspend_vm", map[string]any{"name": name, "mode": "pause"}); got["state"] != "Paused" {
		t.Fatalf("after suspend_vm the VM is %v, want Paused", got["state"])
	}
	if got := call("resume_vm", map[string]any{"name": name}); got["state"] != "Running" {
		t.Fatalf("after resume_vm the VM is %v, want Running", got["state"])
	}
	if got := call("stop_vm", map[string]any{"name": name, "force": true}); got["state"] != "Off" {
		t.Fatalf("after stop_vm the VM is %v, want Off", got["state"])
	}
}

// TestForgetHostKey clears the pinned SSH host key for the Rocky VM.
//
// This checks nothing. It is a maintenance step for when the VM has been rebuilt
// outside TestRockyProvision — which clears the pin itself — leaving every SSH
// test failing on a mismatch that is entirely expected. It is opt-in because
// discarding a pinned key is exactly what an attacker would want, so it should
// never happen as a side effect of running the suite.
func TestForgetHostKey(t *testing.T) {
	if os.Getenv("HYPERVM_E2E_FORGET") == "" {
		t.Skip("set HYPERVM_E2E_FORGET=1 to discard the pinned SSH host key")
	}
	session, ctx := connect(t)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ssh_forget_host_key",
		Arguments: map[string]any{"name": "rocky10-test"},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("%s", contentText(res))
	}
	t.Log("the pinned host key was discarded; the next connection will pin whatever it finds")
}

func contentText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return "(no text content)"
}
