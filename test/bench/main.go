// Command bench measures fleet-rollout: what resumable downloads save, how far staging limits a
// bad release, how long a breach takes to halt, and how many simulated devices one machine can
// serve. docs/benchmarks.md has the results and the commands that produced them.
//
//	go run ./test/bench resume  [flags]   bytes downloaded with and without resume
//	go run ./test/bench staging [flags]   devices a bad release reaches, staged and not
//	go run ./test/bench halt    [flags]   time from a breach to the halt, against real rolloutd
//	go run ./test/bench scale   [flags]   latency and load at growing fleet sizes, against real rolloutd
//
// Each needs a Postgres server it can create scratch databases on, from --database-url or
// ROLLOUT_BENCH_DATABASE_URL. halt and scale also need a rolloutd binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

const databaseEnv = "ROLLOUT_BENCH_DATABASE_URL"

const usage = `usage: bench <resume|staging|halt|scale> [flags]

Run "bench <command> -h" for flags`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	commands := map[string]func(context.Context, []string, func(string) string, io.Writer, io.Writer) error{
		"resume":  runResume,
		"staging": runStaging,
		"halt":    runHalt,
		"scale":   runScale,
	}
	command, ok := commands[args[0]]
	if !ok {
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
	return command(ctx, args[1:], getenv, stdout, stderr)
}

// databaseFlag adds --database-url, which falls back to ROLLOUT_BENCH_DATABASE_URL.
func databaseFlag(flags *flag.FlagSet) *string {
	return flags.String("database-url", "", "Postgres server to create scratch databases on; "+databaseEnv+" works too")
}

func databaseServer(value string, getenv func(string) string) (string, error) {
	if value == "" {
		value = getenv(databaseEnv)
	}
	if value == "" {
		return "", errors.New("--database-url or " + databaseEnv + " is required")
	}
	return value, nil
}
