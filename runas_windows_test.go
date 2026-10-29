//go:build windows

package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestConfigureRunAsUserRejectsUnsupportedWindowsSwitch(t *testing.T) {
	err := configureRunAsUser(&exec.Cmd{}, "another-user", zerolog.Nop())
	if err == nil || !strings.Contains(err.Error(), "not supported on Windows") {
		t.Fatalf("expected explicit Windows run_as_user error, got %v", err)
	}
}
