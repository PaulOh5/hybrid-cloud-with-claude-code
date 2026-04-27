package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

func runTeam(env *cmdEnv, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`team: subcommand required ("create" | "add-member" | "list-members")`)
	}
	switch args[0] {
	case "create":
		return teamCreate(env, args[1:])
	case "add-member":
		return teamAddMember(env, args[1:])
	case "list-members":
		return teamListMembers(env, args[1:])
	default:
		return fmt.Errorf("team: unknown subcommand %q", args[0])
	}
}

func teamCreate(env *cmdEnv, args []string) error {
	fs := flag.NewFlagSet("team create", flag.ContinueOnError)
	name := fs.String("name", "", "unique team name (required)")
	desc := fs.String("description", "", "free-form description (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	team, err := env.queries.TeamCreate(env.ctx, dbstore.TeamCreateParams{
		Name:        *name,
		Description: *desc,
	})
	if err != nil {
		return fmt.Errorf("team create: %w", err)
	}
	fmt.Fprintf(env.stdout, "team created  id=%s  name=%s\n", team.ID, team.Name)
	return nil
}

func teamAddMember(env *cmdEnv, args []string) error {
	fs := flag.NewFlagSet("team add-member", flag.ContinueOnError)
	teamIDStr := fs.String("team-id", "", "uuid of the team (required)")
	teamName := fs.String("team-name", "", "team name (alternative to --team-id)")
	userEmail := fs.String("user-email", "", "email of the user to add (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *userEmail == "" {
		return fmt.Errorf("--user-email is required")
	}
	if *teamIDStr == "" && *teamName == "" {
		return fmt.Errorf("--team-id or --team-name is required")
	}

	var teamID uuid.UUID
	if *teamIDStr != "" {
		id, err := uuid.Parse(*teamIDStr)
		if err != nil {
			return fmt.Errorf("--team-id must be a UUID: %w", err)
		}
		teamID = id
	} else {
		t, err := env.queries.TeamGetByName(env.ctx, *teamName)
		if err != nil {
			return fmt.Errorf("look up team %q: %w", *teamName, err)
		}
		teamID = t.ID
	}

	// Look up user by email — admin CLI doesn't have a session, so this
	// is a direct DB query rather than going through the auth package.
	var userID uuid.UUID
	if err := env.pool.QueryRow(env.ctx,
		`select id from users where email = $1`, *userEmail,
	).Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("no user with email %q", *userEmail)
		}
		return fmt.Errorf("look up user: %w", err)
	}

	if err := env.queries.TeamMemberAdd(env.ctx, dbstore.TeamMemberAddParams{
		TeamID: teamID,
		UserID: userID,
	}); err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	fmt.Fprintf(env.stdout, "added user %s (%s) to team %s\n", *userEmail, userID, teamID)
	return nil
}

func teamListMembers(env *cmdEnv, args []string) error {
	fs := flag.NewFlagSet("team list-members", flag.ContinueOnError)
	teamIDStr := fs.String("team-id", "", "uuid of the team (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *teamIDStr == "" {
		return fmt.Errorf("--team-id is required")
	}
	teamID, err := uuid.Parse(*teamIDStr)
	if err != nil {
		return fmt.Errorf("--team-id must be a UUID: %w", err)
	}
	rows, err := env.queries.TeamMembersForTeam(env.ctx, teamID)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	if len(rows) == 0 {
		fmt.Fprintln(env.stdout, "(no members)")
		return nil
	}
	for _, r := range rows {
		fmt.Fprintf(env.stdout, "%s  %s  joined=%s\n",
			r.UserID,
			r.Email,
			r.JoinedAt.Time.UTC().Format("2006-01-02T15:04:05Z"),
		)
	}
	return nil
}
