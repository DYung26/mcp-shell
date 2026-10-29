//go:build !windows

package main

import (
	"os/exec"
	"os/user"
	"testing"

	"github.com/rs/zerolog"
)

func TestConfigureRunAsUserSetsProcessCredentials(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skipf("current user lookup unavailable: %v", err)
	}
	cmd := &exec.Cmd{}
	if err := configureRunAsUser(cmd, current.Username, zerolog.Nop()); err != nil {
		t.Fatalf("configureRunAsUser returned error: %v", err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil {
		t.Fatal("expected process credentials to be configured")
	}
}
