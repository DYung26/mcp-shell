//go:build !windows

package main

import (
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"

	"github.com/rs/zerolog"
)

func configureRunAsUser(cmd *exec.Cmd, username string, logger zerolog.Logger) error {
	u, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("look up user: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("parse user ID: %w", err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return fmt.Errorf("parse group ID: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
	logger.Debug().Str("user", username).Int("uid", uid).Int("gid", gid).Msg("Set process credentials")
	return nil
}
