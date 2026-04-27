package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// nodeTokenBytes — 32 random bytes → 43 base64url chars. Plenty against
// brute force; bcrypt at cost 12 makes per-comparison cost meaningful.
const nodeTokenBytes = 32

// nodeTokenBcryptCost matches the password bcrypt cost so production
// auth latency is predictable across both code paths.
const nodeTokenBcryptCost = 12

func runNodeToken(env *cmdEnv, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`node-token: subcommand required ("create" | "list" | "revoke")`)
	}
	switch args[0] {
	case "create":
		return nodeTokenCreate(env, args[1:])
	case "list":
		return nodeTokenList(env, args[1:])
	case "revoke":
		return nodeTokenRevoke(env, args[1:])
	default:
		return fmt.Errorf("node-token: unknown subcommand %q", args[0])
	}
}

func nodeTokenCreate(env *cmdEnv, args []string) error {
	fs := flag.NewFlagSet("node-token create", flag.ContinueOnError)
	nodeName := fs.String("node-name", "", "node name to issue the token for (required)")
	ownerTeam := fs.String("owner-team", "", "team name to associate (required; sets nodes.access_policy='owner_team')")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *nodeName == "" || *ownerTeam == "" {
		return fmt.Errorf("--node-name and --owner-team are required")
	}

	node, err := env.queries.GetNodeByName(env.ctx, *nodeName)
	if err != nil {
		return fmt.Errorf("look up node %q: %w", *nodeName, err)
	}
	team, err := env.queries.TeamGetByName(env.ctx, *ownerTeam)
	if err != nil {
		return fmt.Errorf("look up team %q: %w", *ownerTeam, err)
	}

	plaintext, err := generateTokenPlaintext()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), nodeTokenBcryptCost)
	if err != nil {
		return fmt.Errorf("hash token: %w", err)
	}

	row, err := env.queries.NodeTokenInsert(env.ctx, dbstore.NodeTokenInsertParams{
		NodeID:    node.ID,
		TokenHash: string(hash),
		// CreatedBy left null — admin CLI is operator-driven, not a
		// session-bound user. Phase 3 may flip this to the operator
		// session id once the CLI gains an authenticated mode.
		CreatedBy: uuid.NullUUID{},
	})
	if err != nil {
		return fmt.Errorf("insert token: %w", err)
	}

	// Pin node ACL to owner_team policy. Idempotent — running the
	// command twice for the same node is safe.
	if _, err := env.pool.Exec(env.ctx,
		`update nodes set access_policy = 'owner_team', owner_team_id = $1 where id = $2`,
		team.ID, node.ID,
	); err != nil {
		return fmt.Errorf("pin node ACL: %w", err)
	}

	fmt.Fprintf(env.stdout, `Token created (visible once — copy it now).

  AGENT_API_TOKEN=%s
  AGENT_MUX_ENDPOINT=%s

Token id:    %s
Node:        %s (%s)
Owner team:  %s (%s)
`,
		plaintext,
		env.muxEndpoint,
		row.ID,
		node.NodeName, node.ID,
		team.Name, team.ID,
	)
	return nil
}

func nodeTokenList(env *cmdEnv, args []string) error {
	fs := flag.NewFlagSet("node-token list", flag.ContinueOnError)
	nodeName := fs.String("node-name", "", "node name to list tokens for (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *nodeName == "" {
		return fmt.Errorf("--node-name is required")
	}
	node, err := env.queries.GetNodeByName(env.ctx, *nodeName)
	if err != nil {
		return fmt.Errorf("look up node: %w", err)
	}
	rows, err := env.queries.ListNodeTokens(env.ctx, node.ID)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.stdout, "(no tokens)")
		return nil
	}
	for _, r := range rows {
		state := "active"
		revokedAt := "-"
		if r.RevokedAt.Valid {
			state = "revoked"
			revokedAt = r.RevokedAt.Time.UTC().Format("2006-01-02T15:04:05Z")
		}
		fmt.Fprintf(env.stdout, "%s  state=%s  created=%s  revoked=%s\n",
			r.ID,
			state,
			r.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05Z"),
			revokedAt,
		)
	}
	return nil
}

func nodeTokenRevoke(env *cmdEnv, args []string) error {
	fs := flag.NewFlagSet("node-token revoke", flag.ContinueOnError)
	tokenIDStr := fs.String("token-id", "", "uuid of the token to revoke (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tokenIDStr == "" {
		return fmt.Errorf("--token-id is required")
	}
	tokenID, err := uuid.Parse(*tokenIDStr)
	if err != nil {
		return fmt.Errorf("--token-id must be a UUID: %w", err)
	}
	if err := env.queries.NodeTokenRevoke(env.ctx, tokenID); err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	fmt.Fprintf(env.stdout, "revoked %s\n", tokenID)
	return nil
}

// generateTokenPlaintext returns a base64url-encoded 32-byte random
// token. Caller bcrypt-hashes the result before persisting.
func generateTokenPlaintext() (string, error) {
	buf := make([]byte, nodeTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
