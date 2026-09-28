//go:build windows

package svcmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/heavycaffeiner/hypervm-mcp/internal/config"
	"github.com/heavycaffeiner/hypervm-mcp/internal/creds"
	"github.com/heavycaffeiner/hypervm-mcp/internal/guest"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hverr"
	"github.com/heavycaffeiner/hypervm-mcp/internal/hyperv"
	"github.com/heavycaffeiner/hypervm-mcp/internal/ipc"
	"github.com/heavycaffeiner/hypervm-mcp/internal/logx"
	"github.com/heavycaffeiner/hypervm-mcp/internal/mcpsrv"
	"github.com/heavycaffeiner/hypervm-mcp/internal/netfw"
	"github.com/heavycaffeiner/hypervm-mcp/internal/psrun"
	"github.com/heavycaffeiner/hypervm-mcp/internal/sshx"
	"github.com/heavycaffeiner/hypervm-mcp/internal/tailnet"
	"github.com/heavycaffeiner/hypervm-mcp/internal/tunnel"
	"github.com/heavycaffeiner/hypervm-mcp/internal/winsec"
)

// program holds the process-wide singletons every connection shares.
type program struct {
	version string
	cfg     *config.Config
	log     *logx.Logger
	deps    *mcpsrv.Deps
}

// Run is the entry point the SCM invokes. It is not meant to be run by hand.
func Run(version string) error {
	inService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("determine the process context: %w", err)
	}
	if !inService {
		return errors.New("`service run` is started by the service control manager; " +
			"use `hypervm-mcp service start` instead")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	log, logErr := logx.NewFile(config.LogPath(), logx.ParseLevel(cfg.LogLevel))
	if elog, err := eventlog.Open(config.ServiceName()); err == nil {
		log.SetSink(elog)
		defer elog.Close()
	}
	logx.SetDefault(log)
	defer log.Close()
	if logErr != nil {
		log.Warnf("file logging unavailable: %v", logErr)
	}

	runner := psrun.New(cfg.PowerShellPath,
		time.Duration(cfg.PowerShellTimeoutSeconds)*time.Second,
		cfg.MaxConcurrentPowerShell)

	vmClient := hyperv.NewClient(runner)
	credStore := creds.NewStore()
	hostKeys := sshx.NewHostKeys()
	pool := sshx.NewPool(hostKeys, log)
	dialer := &guest.Dialer{VM: vmClient, Creds: credStore, Pool: pool}
	ts := tailnet.New(cfg.TailscalePath)

	tunnels := tunnel.NewManager(tunnel.Deps{
		ResolveGuestIP: func(ctx context.Context, vmName string) (string, error) {
			addr, _, err := vmClient.ResolveGuestIP(ctx, vmName)
			return addr, err
		},
		SSHClient:    dialer.ConnectStored,
		TailnetAddrs: ts.Addrs,
		Firewall:     netfw.New(runner),
		Log:          log,
	})

	p := &program{
		version: version,
		cfg:     cfg,
		log:     log,
		deps: &mcpsrv.Deps{
			VM:       vmClient,
			Guest:    dialer,
			Creds:    credStore,
			SSH:      pool,
			HostKeys: hostKeys,
			Tunnels:  tunnels,
			Tailnet:  ts,
			Log:      log,
		},
	}

	return svc.Run(config.ServiceName(), p)
}

// Execute is the SCM control loop.
func (p *program) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ln, err := ipc.Listen(p.cfg.PipePath(), winsec.PipeSDDL(p.cfg.AllowedSID))
	if err != nil {
		p.log.Errorf("could not open %s: %v", p.cfg.PipePath(), err)
		status <- svc.Status{State: svc.Stopped}
		return true, 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.acceptLoop(ctx, ln)

	p.log.Infof("listening on %s (version %s)", p.cfg.PipePath(), p.version)
	status <- svc.Status{State: svc.Running, Accepts: accepted}

	// Reopen tunnels in the background: a VM that is slow to answer must not
	// hold up the service reaching the Running state.
	go p.deps.Tunnels.Restore(ctx)

	for req := range requests {
		switch req.Cmd {
		case svc.Interrogate:
			status <- req.CurrentStatus
		case svc.Stop, svc.Shutdown:
			p.log.Infof("stopping")
			status <- svc.Status{State: svc.StopPending}
			ln.Close()
			// Tunnels are torn down explicitly: a firewall rule left behind would
			// allow a port that nothing is listening on any more.
			shutdownCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
			p.deps.Tunnels.CloseAll(shutdownCtx)
			done()
			p.deps.SSH.Close()
			cancel()
			return false, 0
		default:
			p.log.Warnf("ignoring unexpected service control %d", req.Cmd)
		}
	}
	return false, 0
}

func (p *program) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, winio.ErrPipeListenerClosed) || errors.Is(err, net.ErrClosed) {
				return
			}
			// A single failed accept should not take the service down.
			p.log.Warnf("accept failed: %v", err)
			continue
		}
		go p.handle(ctx, conn)
	}
}

// handle serves one connection: either an MCP session or a one-shot control
// request from the CLI.
func (p *program) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	defer func() {
		// One misbehaving session must not kill the service for every other one.
		if r := recover(); r != nil {
			p.log.Errorf("session panicked: %v\n%s", r, debug.Stack())
		}
	}()

	frame, reader, firstLine, err := ipc.Peek(conn)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			p.log.Warnf("could not read the opening frame: %v", err)
		}
		return
	}

	if frame == ipc.FrameControl {
		if err := ipc.ServeControl(ctx, conn, firstLine, p.control); err != nil {
			p.log.Warnf("control request failed: %v", err)
		}
		return
	}

	deps := *p.deps
	deps.Impersonate = func(fn func() error) error {
		return winsec.WithNamedPipeClient(conn, fn)
	}
	srv := mcpsrv.New(p.version, &deps)
	transport := &mcp.IOTransport{
		Reader: io.NopCloser(reader),
		Writer: nopWriteCloser{conn}, // conn is closed once, by the defer above
	}
	if err := srv.Run(ctx, transport); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		p.log.Warnf("MCP session ended: %v", err)
	}
}

// credRequest is the control frame the CLI sends to store a credential.
//
// Storing goes through the service because the two ends run as different
// accounts: the CLI is the logged-in user, and only the service can write the
// machine-scope encrypted file in its own data directory.
type credRequest struct {
	VM            string `json:"vm"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	SSHPort       int    `json:"ssh_port"`
	SSHPrivateKey string `json:"ssh_private_key"`
	SSHPassphrase string `json:"ssh_key_passphrase"`
}

// control answers the CLI's one-shot requests.
func (p *program) control(_ context.Context, req ipc.ControlRequest) ipc.ControlResponse {
	switch req.Op {
	case "ping":
		return ok(map[string]any{"version": p.version})

	case "status":
		return ok(map[string]any{
			"version":    p.version,
			"pipe":       p.cfg.PipePath(),
			"allowed":    p.cfg.AllowedSID,
			"powershell": p.cfg.PowerShellPath,
			"tunnels":    len(p.deps.Tunnels.List("")),
		})

	case "cred.set":
		var c credRequest
		if err := json.Unmarshal(req.Raw, &c); err != nil {
			return fail(hverr.InvalidArgument, "malformed cred.set request")
		}
		if c.VM == "" {
			return fail(hverr.InvalidArgument, "vm is required")
		}
		err := p.deps.Creds.Set(c.VM, creds.Entry{
			Username:      c.Username,
			Password:      c.Password,
			SSHPort:       c.SSHPort,
			SSHPrivateKey: c.SSHPrivateKey,
			SSHPassphrase: c.SSHPassphrase,
		})
		if err != nil {
			return fail(hverr.Internal, "%v", err)
		}
		// A credential change invalidates any pooled connection made with the old one.
		p.deps.SSH.Drop(c.VM)
		p.log.Infof("stored credentials for %q", c.VM)
		return ok(map[string]any{"vm": c.VM})

	case "cred.list":
		list, err := p.deps.Creds.List()
		if err != nil {
			return fail(hverr.Internal, "%v", err)
		}
		return ok(list)

	case "cred.delete":
		var c credRequest
		if err := json.Unmarshal(req.Raw, &c); err != nil || c.VM == "" {
			return fail(hverr.InvalidArgument, "vm is required")
		}
		if err := p.deps.Creds.Delete(c.VM); err != nil {
			return fail(hverr.Internal, "%v", err)
		}
		p.deps.SSH.Drop(c.VM)
		return ok(map[string]any{"vm": c.VM})

	case "tunnel.list":
		return ok(p.deps.Tunnels.List(""))

	case "doctor":
		// The same checks the MCP tool runs, so the two never disagree.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return ok(mcpsrv.Diagnose(ctx, p.deps))

	default:
		return fail(hverr.InvalidArgument, "unknown control op %q", req.Op)
	}
}

func ok(data any) ipc.ControlResponse {
	return ipc.ControlResponse{OK: true, Data: data}
}

func fail(code hverr.Code, format string, args ...any) ipc.ControlResponse {
	return ipc.ControlResponse{OK: false, Code: string(code), Error: fmt.Sprintf(format, args...)}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
