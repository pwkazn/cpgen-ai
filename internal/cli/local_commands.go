package cli

import (
	"errors"
	"fmt"
	"io"
)

func localReadCommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	if args[0] == "review" {
		return args[1] == "show"
	}
	if args[0] != "run" {
		return false
	}
	switch args[1] {
	case "list", "show", "events", "export":
		return true
	default:
		return false
	}
}

// Reject unknown commands and malformed positional identities before acquiring
// local resources or performing execution preflight. Flag values are validated
// by their command handlers.
func validateCommandShape(args []string, stdout, stderr io.Writer) int {
	if args[0] == "generate" {
		return 0
	}
	if len(args) < 2 {
		return writeStateError(stdout, stderr, 2, "usage", errors.New(args[0]+" subcommand is required"))
	}
	valid := false
	if args[0] == "run" {
		switch args[1] {
		case "list", "show", "events", "export", "resume", "cancel":
			valid = true
		}
	} else {
		switch args[1] {
		case "show", "revise", "retry", "waive", "reject":
			valid = true
		}
	}
	if !valid {
		return writeStateError(stdout, stderr, 2, "unknown_command", fmt.Errorf("unknown %s command %q", args[0], args[1]))
	}
	if args[1] == "show" || args[1] == "resume" {
		if len(args) != 3 {
			return writeStateError(stdout, stderr, 2, "usage", fmt.Errorf("usage: %s %s RUN_ID", args[0], args[1]))
		}
		if _, err := parseRunID(args[2]); err != nil {
			return writeStateError(stdout, stderr, 3, "invalid_id", err)
		}
	}
	return 0
}
