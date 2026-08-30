package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"cpgen/internal/transfer"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		writeUsage(stderr)
		return 2
	}
	switch args[0] {
	case "import":
		return runImport(args[1:], stdin, stderr)
	case "export":
		return runExport(args[1:], stdout, stderr)
	case "keep":
		if len(args) != 1 {
			writeUsage(stderr)
			return 2
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := transfer.Keep(ctx); err != nil {
			fmt.Fprintf(stderr, "keep: %v\n", err)
			return 1
		}
		return 0
	default:
		writeUsage(stderr)
		return 2
	}
}

func runImport(args []string, stdin io.Reader, stderr io.Writer) int {
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	rootPath := flags.String("root", "", "absolute import volume root")
	maxFiles := flags.Int("max-files", 0, "maximum imported files")
	maxTotalBytes := flags.Int64("max-total-bytes", -1, "maximum imported bytes")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	if !filepath.IsAbs(*rootPath) {
		fmt.Fprintln(stderr, "import root must be absolute")
		return 2
	}
	limits := transfer.ImportLimits{MaxFiles: *maxFiles, MaxTotalBytes: *maxTotalBytes}
	if err := limits.Validate(); err != nil {
		fmt.Fprintf(stderr, "invalid import limits: %v\n", err)
		return 2
	}
	root, err := os.OpenRoot(*rootPath)
	if err != nil {
		fmt.Fprintf(stderr, "open import root: %v\n", err)
		return 1
	}
	defer root.Close()
	if _, err := transfer.Import(context.Background(), root, stdin, limits); err != nil {
		fmt.Fprintf(stderr, "import: %v\n", err)
		return 1
	}
	return 0
}

func runExport(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	rootPath := flags.String("root", "", "absolute export volume root")
	planBase64 := flags.String("plan-base64", "", "base64-encoded strict export plan")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	if !filepath.IsAbs(*rootPath) || *planBase64 == "" {
		fmt.Fprintln(stderr, "export requires an absolute root and plan-base64")
		return 2
	}
	planBytes, err := base64.StdEncoding.Strict().DecodeString(*planBase64)
	if err != nil {
		fmt.Fprintf(stderr, "decode export plan: %v\n", err)
		return 2
	}
	plan, err := transfer.ParseExportPlan(planBytes)
	if err != nil {
		fmt.Fprintf(stderr, "invalid export plan: %v\n", err)
		return 2
	}
	root, err := os.OpenRoot(*rootPath)
	if err != nil {
		fmt.Fprintf(stderr, "open export root: %v\n", err)
		return 1
	}
	defer root.Close()
	if _, err := transfer.Export(context.Background(), root, plan, stdout); err != nil {
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 1
	}
	return 0
}

func writeUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: cpgen-transfer import --root ROOT --max-files N --max-total-bytes N | export --root ROOT --plan-base64 BASE64 | keep")
}
