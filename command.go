package main

import (
	"context"
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

	return d.buildCommand(ctx, nil, true, env, args...).Run()
}

func (d *depInspector) runCommand(ctx context.Context, writer io.Writer, args ...string) error {
	return d.buildCommand(ctx, writer, false, nil, args...).Run()
}

func (d *depInspector) buildCommand(ctx context.Context, writer io.Writer, stderr bool, env []string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if len(args) == 1 {
		cmd = exec.CommandContext(ctx, args[0])
	} else {
		cmd = exec.CommandContext(ctx, args[0], args[1:]...)
	}

	cmd.Env = env
	cmd.Stdout = writer
	if stderr {
		cmd.Stderr = writer
	}

	if d.verbose {
		log.Printf("running command: %q", cmd)
	}

	return cmd
}
