package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/carsteneu/yesmem/internal/claudemd"
	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/storage"
)

func runClaudeMd() {
	flags := flag.NewFlagSet("claudemd", flag.ExitOnError)
	project := flags.String("project", "", "project short name (DB key)")
	projectDir := flags.String("dir", "", "project filesystem path")
	doSetup := flags.Bool("setup", false, "inject @-import into CLAUDE.md and update .gitignore")
	opencode := flags.Bool("opencode", false, "write AGENTS.md at the project root instead of .claude/yesmem-ops.md")
	all := flags.Bool("all", false, "regenerate all known projects")
	dryRun := flags.Bool("dry-run", false, "show what would be regenerated (with --all)")
	flags.Parse(os.Args[2:])

	dataDir := yesmemDataDir()
	cfg, err := config.Load(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := storage.Open(cfg.Paths.DB)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	if *all {
		runClaudeMdAll(store, cfg, *dryRun, *opencode)
		return
	}

	if *project == "" || *projectDir == "" {
		fmt.Fprintln(os.Stderr, "usage: yesmem claudemd --project <name> --dir <path> [--setup] [--opencode]")
		fmt.Fprintln(os.Stderr, "       yesmem claudemd --all [--dry-run] [--opencode]")
		os.Exit(1)
	}

	model := cfg.ClaudeMd.Model
	if model == "" {
		model = cfg.Extraction.Model
	}
	client, err := extraction.NewLLMClient(cfg.LLM.Provider, cfg.ResolvedAPIKey(), model, cfg.LLM.ClaudeBinary, cfg.ResolvedOpenAIBaseURL())
	if err != nil {
		log.Fatalf("llm client: %v", err)
	}
	client = redacted(cfg, client)
	if client == nil {
		log.Fatalf("no LLM client available: set ANTHROPIC_API_KEY/OPENAI_API_KEY or configure claude_binary")
	}

	out := claudeMdOutputFor(&cfg.ClaudeMd, *opencode)
	gen := claudemd.NewGenerator(store, client, &cfg.ClaudeMd)
	if err := generateFor(gen, *project, *projectDir, out); err != nil {
		log.Fatalf("generate: %v", err)
	}
	fmt.Printf("Generated %s for %s\n", displayTarget(out), *project)

	if *doSetup {
		if out.agents {
			// AGENTS.md needs no import: opencode reads it at the project root.
			fmt.Println("--setup is Claude-only and was skipped in --opencode mode")
		} else {
			if err := claudemd.SetupProject(*projectDir, cfg.ClaudeMd.OutputFileName); err != nil {
				log.Fatalf("setup: %v", err)
			}
			fmt.Println("Setup complete: @-import injected into CLAUDE.md")
		}
	}
}

// claudeMdOutput selects which generated file the CLI writes.
type claudeMdOutput struct {
	fileName string
	agents   bool
}

func claudeMdOutputFor(cfg *config.ClaudeMdConfig, opencode bool) claudeMdOutput {
	if opencode {
		return claudeMdOutput{fileName: claudemd.AgentsFileName, agents: true}
	}
	return claudeMdOutput{fileName: cfg.OutputFileName}
}

// generateFor dispatches to the generator method for the selected target.
func generateFor(gen *claudemd.Generator, project, projectDir string, out claudeMdOutput) error {
	if out.agents {
		return gen.GenerateAgentsMd(project, projectDir)
	}
	return gen.Generate(project, projectDir)
}

func displayTarget(out claudeMdOutput) string {
	if out.agents {
		return out.fileName
	}
	return ".claude/" + out.fileName
}

func runClaudeMdAll(store *storage.Store, cfg *config.Config, dryRun, opencode bool) {
	projects, err := store.ListProjects()
	if err != nil {
		log.Fatalf("list projects: %v", err)
	}

	model := cfg.ClaudeMd.Model
	if model == "" {
		model = cfg.Extraction.Model
	}

	var client extraction.LLMClient
	if !dryRun {
		client, err = extraction.NewLLMClient(cfg.LLM.Provider, cfg.ResolvedAPIKey(), model, cfg.LLM.ClaudeBinary, cfg.ResolvedOpenAIBaseURL())
		if err != nil {
			log.Fatalf("llm client: %v", err)
		}
		client = redacted(cfg, client)
		if client == nil {
			log.Fatalf("no LLM client available: set ANTHROPIC_API_KEY/OPENAI_API_KEY or configure claude_binary")
		}
	}

	gen := claudemd.NewGenerator(store, client, &cfg.ClaudeMd)
	out := claudeMdOutputFor(&cfg.ClaudeMd, opencode)
	regenerated := 0

	for _, p := range projects {
		if p.SessionCount < cfg.ClaudeMd.MinSessions {
			continue
		}
		if info, err := os.Stat(p.Project); err != nil || !info.IsDir() {
			continue
		}

		needs, err := gen.NeedsRefreshFor(p.ProjectShort, out.fileName)
		if err != nil {
			fmt.Printf("  SKIP  %-20s  (error: %v)\n", p.ProjectShort, err)
			continue
		}

		if dryRun {
			status := "up-to-date"
			if needs {
				status = "STALE — would regenerate"
			}
			fmt.Printf("  %-20s  %s  (%s)\n", p.ProjectShort, p.Project, status)
			continue
		}

		if !needs {
			continue
		}

		if err := generateFor(gen, p.ProjectShort, p.Project, out); err != nil {
			fmt.Printf("  FAIL  %-20s  %v\n", p.ProjectShort, err)
		} else {
			if !out.agents {
				if err := claudemd.SetupProject(p.Project, cfg.ClaudeMd.OutputFileName); err != nil {
					fmt.Printf("  WARN  %-20s  setup: %v\n", p.ProjectShort, err)
				}
			}
			fmt.Printf("  OK    %-20s  %s\n", p.ProjectShort, p.Project)
			regenerated++
		}
	}

	if !dryRun {
		fmt.Printf("\nRegenerated %d project(s)\n", regenerated)
	}
}
