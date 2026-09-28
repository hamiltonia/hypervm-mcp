//go:build windows

package winsec

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func TestWithNamedPipeClient(t *testing.T) {
	pipe := fmt.Sprintf(`\\.\pipe\hypervm-winsec-test-%d`, time.Now().UnixNano())
	listener, err := winio.ListenPipe(pipe, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;WD)",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan error, 1)
	go func() {
		server, err := listener.Accept()
		if err != nil {
			accepted <- err
			return
		}
		defer server.Close()

		err = WithNamedPipeClient(server, func() error {
			user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
			if err != nil {
				return err
			}
			processUser, err := windows.GetCurrentProcessToken().GetTokenUser()
			if err != nil {
				return err
			}
			if !user.User.Sid.Equals(processUser.User.Sid) {
				return fmt.Errorf("impersonated SID %s, want %s",
					user.User.Sid, processUser.User.Sid)
			}
			return nil
		})
		accepted <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := winio.DialPipeAccessImpLevel(ctx, pipe,
		uint32(windows.GENERIC_READ|windows.GENERIC_WRITE),
		winio.PipeImpLevelImpersonation)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()

	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
