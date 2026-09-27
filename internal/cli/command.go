package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"

	"cpgen/internal/application"
	"cpgen/internal/domain"
)

// preparedCommand contains only application lifetime requirements and work
// that captures already parsed, command-specific arguments.
type preparedCommand struct {
	runID                                 domain.RunID
	local, restore, configOnly, effective bool
	serveListen                           string
	serveCapacity                         int
	execute                               func(*application.Application, io.Writer, io.Writer) int
}

type commandError struct {
	exit     int
	code     string
	err      error
	stateful bool
}

func (e *commandError) write(stdout, stderr io.Writer) int {
	if e.stateful {
		return writeStateError(stdout, stderr, e.exit, e.code, e.err)
	}
	fmt.Fprintln(stderr, e.err)
	return e.exit
}

func argumentError(code string, exit int, err error) *commandError {
	return &commandError{exit: exit, code: code, err: err, stateful: true}
}

type invocation struct {
	configPath string
	command    *preparedCommand
	standalone func(io.Writer, io.Writer, Dependencies) int
}

func (i invocation) run(stdout, stderr io.Writer, deps Dependencies) int {
	if i.standalone != nil {
		return i.standalone(stdout, stderr, deps)
	}
	return runStateful(i.command, i.configPath, stdout, stderr, deps)
}

func parseInvocation(args []string) (invocation, *commandError) {
	fail := func(message string) (invocation, *commandError) {
		return invocation{}, &commandError{exit: 2, err: errors.New(message)}
	}
	if len(args) == 0 {
		args = []string{"help"}
	}
	switch args[0] {
	case "help", "-h", "--help":
		if len(args) != 1 {
			return fail("usage: cpgen help")
		}
		return invocation{standalone: func(out, _ io.Writer, _ Dependencies) int { writeHelp(out); return 0 }}, nil
	case "version":
		jsonOutput := len(args) == 2 && args[1] == "--json"
		if len(args) != 1 && !jsonOutput {
			return fail("usage: cpgen version [--json]")
		}
		return invocation{standalone: func(out, diagnostic io.Writer, _ Dependencies) int {
			if jsonOutput {
				return encodeJSON(out, diagnostic, versionOutput{SchemaVersion: "cpgen.cli-version/v1", Version: Version, GoVersion: runtime.Version()}, 0)
			}
			fmt.Fprintln(out, Version)
			return 0
		}}, nil
	case "doctor":
		return invocation{standalone: func(out, diagnostic io.Writer, deps Dependencies) int {
			return runDoctor(args[1:], out, diagnostic, deps)
		}}, nil
	case "sandbox-watchdog":
		return invocation{standalone: func(_ io.Writer, diagnostic io.Writer, deps Dependencies) int {
			return runSandboxWatchdog(args[1:], diagnostic, deps)
		}}, nil
	}
	root := flag.NewFlagSet("cpgen", flag.ContinueOnError)
	root.SetOutput(io.Discard)
	var path string
	configSeen := false
	root.Func("config", "configuration path", func(value string) error {
		if configSeen {
			return errors.New("duplicate --config")
		}
		configSeen = true
		path = value
		return nil
	})
	if err := root.Parse(args); err != nil {
		return fail(err.Error())
	}
	args = root.Args()
	if !configSeen && len(args) > 0 {
		return fail(fmt.Sprintf("unknown command %q\nrun 'cpgen help' for usage", args[0]))
	}
	if path == "" || len(args) == 0 {
		return fail("usage: cpgen --config PATH <command> ...")
	}
	switch args[0] {
	case "doctor", "version", "help", "-h", "--help", "sandbox-watchdog":
		return fail("this command is config-independent and cannot use --config")
	}
	command, err := parseStatefulCommand(args)
	if err != nil {
		return invocation{}, err
	}
	return invocation{configPath: path, command: command}, nil
}

func parseStatefulCommand(args []string) (*preparedCommand, *commandError) {
	if len(args) == 0 {
		return nil, argumentError("usage", 2, errors.New("command is required"))
	}
	if args[0] == "generate" {
		return prepareGenerate(args[1:])
	}
	if args[0] == "serve" {
		return prepareServe(args[1:])
	}
	if len(args) >= 2 {
		switch args[0] + " " + args[1] {
		case "config validate":
			return prepareConfig(args[2:], false)
		case "config effective":
			return prepareConfig(args[2:], true)
		case "run list":
			return prepareRunList(args[2:])
		case "run show":
			return prepareRunShow(args[2:])
		case "run events":
			return prepareRunEvents(args[2:])
		case "run export":
			return preparePackageExport(args[2:])
		case "run resume":
			return prepareRunResume(args[2:])
		case "run cancel":
			return prepareRunCancel(args[2:])
		case "review show":
			return prepareReviewShow(args[2:])
		case "review revise", "review retry", "review waive", "review reject":
			return prepareReviewMutation(args[1], args[2:])
		}
	}
	return nil, argumentError("unknown_command", 2, fmt.Errorf("unknown or incomplete command %q", strings.Join(args, " ")))
}

func commandFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func parseFlags(flags *flag.FlagSet, args []string) *commandError {
	if err := flags.Parse(args); err != nil {
		return argumentError("usage", 2, err)
	}
	if flags.NArg() != 0 {
		return usageError(flags.Name() + " takes no positional arguments")
	}
	return nil
}

func usageError(message string) *commandError {
	return argumentError("usage", 2, errors.New(message))
}

// The documented form is RUN_ID followed by options. FlagSet also supports
// options followed by RUN_ID. It owns all option and value parsing.
func parseRunFlags(flags *flag.FlagSet, args []string) (domain.RunID, *commandError) {
	var raw string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		raw, args = args[0], args[1:]
		if err := parseFlags(flags, args); err != nil {
			return "", err
		}
	} else {
		if err := flags.Parse(args); err != nil {
			return "", argumentError("usage", 2, err)
		}
		if flags.NArg() != 1 {
			return "", usageError(flags.Name() + " requires RUN_ID")
		}
		raw = flags.Arg(0)
	}
	id, err := parseRunID(raw)
	if err != nil {
		return "", argumentError("invalid_id", 3, err)
	}
	return id, nil
}
