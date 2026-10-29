//go:build windows

package main

import (
	"errors"
	"os/exec"

	"github.com/rs/zerolog"
)

func configureRunAsUser(_ *exec.Cmd, username string, _ zerolog.Logger) error {
	return errors.New("run_as_user is not supported on Windows; run mcp-shell under the intended Windows account")
}
