//go:build windows

// Package ipc carries MCP traffic between the unprivileged user session and the
// LocalSystem service over a named pipe.
//
// The pipe's DACL is the only thing authorising access, so the bridge on the
// user side deliberately holds no protocol state: it copies bytes and nothing
// more.
package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const (
	pipeBufferSize = 64 * 1024

	// dialRetryWindow covers the gap between the service being reported as
	// started and its listener actually accepting.
	dialRetryWindow   = 10 * time.Second
	dialRetryInterval = 200 * time.Millisecond
)

// Listen opens the service's pipe with the given security descriptor.
func Listen(pipePath, sddl string) (net.Listener, error) {
	return winio.ListenPipe(pipePath, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        false, // MCP frames are newline-delimited JSON, not messages
		InputBufferSize:    pipeBufferSize,
		OutputBufferSize:   pipeBufferSize,
	})
}

// Dial connects to the service, retrying briefly so a client started alongside
// the service does not fail on a race.
func Dial(ctx context.Context, pipePath string) (net.Conn, error) {
	deadline := time.Now().Add(dialRetryWindow)
	for {
		conn, err := dialPipe(ctx, pipePath)
		if err == nil {
			return conn, nil
		}
		// Permission problems will not fix themselves; fail immediately.
		if errors.Is(err, fs.ErrPermission) || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			// Report why the pipe would not open, not merely that we gave up.
			return nil, err
		case <-time.After(dialRetryInterval):
		}
	}
}

// DialOnce attempts a single connection. Diagnostics use it so "the service is
// not installed" is reported as such instead of as a retry timeout.
func DialOnce(ctx context.Context, pipePath string) (net.Conn, error) {
	return dialPipe(ctx, pipePath)
}

func dialPipe(ctx context.Context, pipePath string) (net.Conn, error) {
	return winio.DialPipeAccessImpLevel(ctx, pipePath,
		uint32(windows.GENERIC_READ|windows.GENERIC_WRITE),
		winio.PipeImpLevelImpersonation)
}

// Frame kinds distinguish the two protocols sharing this pipe.
type Frame int

const (
	// FrameMCP is JSON-RPC traffic from an MCP client.
	FrameMCP Frame = iota
	// FrameControl is a single-request CLI operation such as cred.set.
	FrameControl
)

// Peek reads the first line of a connection to decide what it is, then returns a
// reader with that line put back so the chosen handler sees the whole stream.
//
// Sharing one pipe between the MCP bridge and the CLI keeps installation to a
// single name and a single ACL.
func Peek(conn net.Conn) (Frame, io.Reader, []byte, error) {
	br := bufio.NewReaderSize(conn, pipeBufferSize)
	line, err := br.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return FrameMCP, nil, nil, err
	}

	// br still holds everything read ahead of the line, so prepending the line
	// reconstructs the original stream.
	rest := io.MultiReader(bytes.NewReader(line), br)

	var probe struct {
		JSONRPC string `json:"jsonrpc"`
		Op      string `json:"op"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &probe) == nil && probe.JSONRPC == "" && probe.Op != "" {
		return FrameControl, rest, line, nil
	}
	return FrameMCP, rest, line, nil
}
