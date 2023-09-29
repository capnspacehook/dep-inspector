package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
)

func runGoCommand(args ...string) error {
	var writer io.Writer
	if verbose {
		writer = os.Stderr
	}

	env := make([]string, len(goEnvVars))
	for _, envVar := range goEnvVars {
		env = append(env, fmt.Sprintf("%s=%s", envVar, os.Getenv(envVar)))
	}

	return buildCommand(writer, true, env, args...).Run()
}

// TODO: wrap error to print stderr
//
//nolint:unparam
func runCommand(writer io.Writer, stderr bool, args ...string) error {
	return buildCommand(writer, stderr, nil, args...).Run()
}

func buildCommand(writer io.Writer, stderr bool, env []string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if len(args) == 1 {
		cmd = exec.Command(args[0])
	} else {
		cmd = exec.Command(args[0], args[1:]...)
	}

	cmd.Env = env
	cmd.Stdout = writer
	if stderr {
		cmd.Stderr = writer
	}

	if verbose {
		log.Printf("running command: %q", cmd)
	}

	return cmd
}
