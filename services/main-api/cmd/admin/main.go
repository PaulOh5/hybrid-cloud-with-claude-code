// admin is the operator CLI for Phase 2 lifecycle work — issuing /
// listing / revoking node tokens and managing teams. Designed for
// runbook use, not interactive shells: every command exits 0 on
// success, non-zero on error, and prints stable text the runbook can
// scrape.
//
// Usage:
//
//	admin node-token create  --node-name X --owner-team T
//	admin node-token list    --node-name X
//	admin node-token revoke  --token-id ID
//	admin team create        --name X [--description X]
//	admin team add-member    --team-id ID --user-email X
//	admin team list-members  --team-id ID
//
// All commands read DATABASE_URL from the environment. node-token
// create also reads SSH_PROXY_MUX_ENDPOINT so the partner-facing
// instructions can include the right endpoint without the operator
// memorising it.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

func main() {
	if err := run(context.Background(), os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run is the package's testable entry point. It builds the DB-backed env
// then dispatches to the relevant command. Tests inject a fake env.
func run(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	if len(args) == 0 {
		printUsage(stderr)
		return fmt.Errorf("a subcommand is required")
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect db: %w", err)
	}
	defer pool.Close()
	q := dbstore.New(pool)

	env := &cmdEnv{
		ctx:         ctx,
		queries:     q,
		pool:        pool,
		stdout:      stdout,
		muxEndpoint: os.Getenv("SSH_PROXY_MUX_ENDPOINT"),
	}
	return dispatch(env, args)
}

// cmdEnv is the shared dependencies a subcommand uses. Wrapping pgxpool
// + queries + writer keeps each command's signature small and testable.
type cmdEnv struct {
	ctx         context.Context
	queries     *dbstore.Queries
	pool        *pgxpool.Pool
	stdout      io.Writer
	muxEndpoint string
}

// dispatch routes by subcommand. Kept separate from run() so tests can
// invoke it with a hand-built env.
func dispatch(env *cmdEnv, args []string) error {
	switch args[0] {
	case "node-token":
		return runNodeToken(env, args[1:])
	case "team":
		return runTeam(env, args[1:])
	case "help", "-h", "--help":
		printUsage(env.stdout)
		return nil
	default:
		printUsage(env.stdout)
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `admin — hybrid-cloud operator CLI (Phase 2)

Subcommands:
  node-token create  --node-name X --owner-team T
  node-token list    --node-name X
  node-token revoke  --token-id ID
  team create        --name X [--description X]
  team add-member    --team-id ID --user-email X
  team list-members  --team-id ID

Environment:
  DATABASE_URL              Postgres URL (required)
  SSH_PROXY_MUX_ENDPOINT    Printed in partner-facing instructions
                            from "node-token create" (default empty)
`)
}
