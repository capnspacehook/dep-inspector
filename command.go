package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
)

func (d *depInspector) runGoCommand(ctx context.Context, args ...string) error {
	env := make([]string, len(goEnvVars))
	for _, envVar := range goEnvVars {
		env = append(env, fmt.Sprintf("%s=%s", envVar, os.Getenv(envVar)))
	}

	cmd, errBuf := d.buildCommand(ctx, nil, env, args...)
	if err := cmd.Run(); err != nil {
		return formatCmdErr(cmd, err, errBuf)
	}
	return nil
}

func (d *depInspector) runCommand(ctx context.Context, writer io.Writer, args ...string) error {
	cmd, errBuf := d.buildCommand(ctx, writer, nil, args...)
	if err := cmd.Run(); err != nil {
		return formatCmdErr(cmd, err, errBuf)
	}
	return nil
}

func (d *depInspector) buildCommand(ctx context.Context, writer io.Writer, env []string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	var cmd *exec.Cmd
	if len(args) == 1 {
		cmd = exec.CommandContext(ctx, args[0])
	} else {
		cmd = exec.CommandContext(ctx, args[0], args[1:]...)
	}

	var errBuf bytes.Buffer
	cmd.Env = env
	cmd.Stdout = writer
	cmd.Stderr = &errBuf

	if d.verbose {
		log.Printf("running command: %q", cmd)
	}

	return cmd, &errBuf
}

func formatCmdErr(cmd *exec.Cmd, err error, errBuf *bytes.Buffer) error {
	var execErr *exec.ExitError
	if errors.As(err, &execErr) {
		return fmt.Errorf("running %s: %s\n%w", cmd, errBuf, err)
	}
	return err
}
