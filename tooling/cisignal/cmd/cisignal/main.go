// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// cisignal provides fleet health data for LLM-driven CI failure investigation.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/fleet"
	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/gcs"
	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/ops"
	"github.com/Azure/ARO-HCP/tooling/cisignal/pkg/sippy"
)

const usage = `cisignal — CI fleet health

Usage:
  cisignal fleet -env=E [-days=N] [-periodic]
  cisignal ops [-days=N]

All commands print markdown to stdout.

Subcommands:
  fleet    Analyze e2e test fleet health for one environment
  ops      Check health of cleanup, pipeline, and sweeper jobs
`

const defaultLookbackDays = 5

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "fleet":
		err = fleetCmd(os.Args[2:])
	case "ops":
		err = opsCmd(os.Args[2:])
	case "--help", "-h", "help":
		fmt.Fprint(os.Stderr, usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}

	if err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func fleetCmd(args []string) error {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	env := fs.String("env", "", "environment (required): int, stg, prod, dev")
	days := fs.Int("days", defaultLookbackDays, "lookback period in days")
	periodic := fs.Bool("periodic", false, "only analyze periodic runs (skip PR-triggered runs)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument: %s", fs.Arg(0))
	}

	if *env == "" {
		return fmt.Errorf("-env is required (int, stg, prod, dev)")
	}
	if !sippy.ValidEnv(*env) {
		return fmt.Errorf("invalid env %q (valid: int, stg, prod, dev)", *env)
	}

	result, err := fleet.Analyze(*env, *days, *periodic)
	if err != nil {
		return fmt.Errorf("fleet analysis: %w", err)
	}

	if result.Health.TotalRuns == 0 {
		slog.Info("no runs found", "env", *env, "days", *days)
		return nil
	}

	fleet.WriteTriageInput(os.Stdout, result)
	return nil
}

func opsCmd(args []string) error {
	fs := flag.NewFlagSet("ops", flag.ContinueOnError)
	days := fs.Int("days", defaultLookbackDays, "lookback period in days")
	if err := fs.Parse(args); err != nil {
		return err
	}

	g := gcs.NewClient()
	result := ops.Analyze(*days, g)
	result.Print(os.Stdout)
	return nil
}

