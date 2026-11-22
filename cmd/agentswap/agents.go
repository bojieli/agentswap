package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/bojieli/agentswap/internal/session"
)

func cmdAgents(args []string) error {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "project directory (default: current directory)")
	sourceDir := fs.String("source-dir", "", "read definitions from this directory")
	targetDir := fs.String("target-dir", "", "write definitions here (default: project agent directory)")
	agentFile := fs.String("agent-file", "", "source Kimi root YAML, legacy Codex config TOML, or OpenCode config JSON")
	model := fs.String("model", "", "explicit target model (otherwise inherits target configuration)")
	dryRun := fs.Bool("dry-run", false, "show files and compatibility warnings without writing")
	strict := fs.Bool("strict", false, "refuse conversions with any compatibility warnings")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: agentswap agents <source> <target> [flags]\n\nConvert reusable agent definitions. Session handoffs preserve run histories\nwithout modifying installed agents. Existing definition files are never overwritten.")
		fs.PrintDefaults()
	}
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fs.Usage()
		return nil
	}
	if len(args) < 2 {
		fs.Usage()
		return errors.New("source and target agents are required")
	}
	source, err := session.ParseAgent(args[0])
	if err != nil {
		return err
	}
	target, err := session.ParseAgent(args[1])
	if err != nil {
		return err
	}
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *cwd == "" {
		*cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	result, err := session.TransferDefinitions(source, target, session.DefinitionOptions{CWD: *cwd, SourceDir: *sourceDir, TargetDir: *targetDir, AgentFile: *agentFile, Model: *model, DryRun: *dryRun, Strict: *strict})
	for _, warning := range result.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	if err != nil {
		return err
	}
	verb := "Created"
	if *dryRun {
		verb = "Would create"
	}
	fmt.Printf("%s %d %s agent definitions:\n", verb, len(result.Definitions), target.Display())
	for _, path := range result.Files {
		fmt.Println(" ", path)
	}
	if len(result.Launch) > 0 {
		fmt.Println("Launch:", shellJoin(result.Launch))
	}
	return nil
}
