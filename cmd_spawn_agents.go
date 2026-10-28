package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/carsteneu/yesmem/internal/daemon"
	"github.com/carsteneu/yesmem/internal/orchestrator"
)

func runSpawnAgents(args []string) {
	fs := flag.NewFlagSet("spawn-agents", flag.ExitOnError)
	project := fs.String("project", "", "Project name (required)")
	count := fs.Int("count", 1, "Number of agents to spawn (1-10)")
	tasks := fs.String("tasks", "", "Comma-separated section names for agents")
	callerSession := fs.String("caller-session", "", "Session ID of the calling agent (for send_to callback)")
	fs.Parse(args)

	if *project == "" {
		fmt.Fprintln(os.Stderr, "Error: --project is required")
		os.Exit(1)
	}
	if *count < 1 || *count > 10 {
		fmt.Fprintln(os.Stderr, "Error: --count must be between 1 and 10")
		os.Exit(1)
	}

	sections := buildSections(*tasks, *count)
	terminal := orchestrator.DetectTerminal()
	fmt.Printf("Terminal detected: %s\n", terminal)

	dataDir := yesmemDataDir()
	yesmemBin, _ := os.Executable()

	// Connect to daemon for agent state management
	client, err := daemon.DialTimeout(dataDir, 5*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	type spawnedAgent struct {
		id      string
		section string
		bridge  *orchestrator.AgentBridge
	}

	spawned := make([]spawnedAgent, 0, *count)

	for i := 0; i < *count; i++ {
		section := sections[i]

		// The prompt is typed into the spawned TUI as keyboard input, so build and
		// validate it before the daemon creates a row: a field that cannot be
		// rendered is refused here instead of leaving a row behind that no process
		// owns (YM-255).
		prompt, err := buildAgentPrompt(*project, section, *callerSession)
		if err != nil {
			fmt.Fprintf(os.Stderr, "section %q: %v\n", section, err)
			continue
		}

		// 1. Create agent record in DB via daemon RPC
		client.SetDeadline(time.Now().Add(10 * time.Second))
		result, err := client.Call("spawn_agent", map[string]any{
			"project":        *project,
			"section":        section,
			"caller_session": *callerSession,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent-%d: spawn_agent RPC failed: %v\n", i, err)
			continue
		}

		var spawnResult struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(result, &spawnResult); err != nil {
			fmt.Fprintf(os.Stderr, "agent-%d: parse spawn result: %v\n", i, err)
			continue
		}

		// 2. Create PTY bridge locally (the prompt was built and validated above)
		sockPath := filepath.Join(dataDir, fmt.Sprintf("%s.sock", spawnResult.ID))
		bridge, err := orchestrator.NewAgentBridge("claude", []string{"--session-id", spawnResult.SessionID, "--name", fmt.Sprintf("%s-%s", *project, section), prompt}, sockPath, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: bridge failed: %v\n", spawnResult.ID, err)
			continue
		}

		go bridge.Serve()

		// 3. Open terminal window
		bin, spawnArgs := orchestrator.BuildSpawnCommand(terminal, yesmemBin, "", "agent-tty", "--sock", sockPath)
		termCmd := exec.Command(bin, spawnArgs...)
		if err := termCmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: terminal failed: %v\n", spawnResult.ID, err)
			bridge.Close()
			continue
		}

		// 4. Register PID + socket path with daemon. The daemon refuses a row that
		// is no longer live (stopped in the meantime) — this CLI started the
		// process, so a refusal has to end it here instead of printing a spawn
		// whose process belongs to nobody.
		client.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := client.Call("register_agent", map[string]any{
			"id":         spawnResult.ID,
			"pid":        float64(bridge.Cmd.Process.Pid),
			"sock_path":  sockPath,
			"session_id": spawnResult.SessionID,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "%s: register_agent refused: %v — shutting the process down\n", spawnResult.ID, err)
			bridge.Close()
			bridge.Cmd.Process.Signal(syscall.SIGTERM)
			continue
		}

		fmt.Printf("Spawned %s -> section '%s' (PID %d, session %s)\n", spawnResult.ID, section, bridge.Cmd.Process.Pid, spawnResult.SessionID)

		spawned = append(spawned, spawnedAgent{
			id:      spawnResult.ID,
			section: section,
			bridge:  bridge,
		})
	}

	if len(spawned) == 0 {
		fmt.Fprintln(os.Stderr, "No agents could be spawned.")
		os.Exit(1)
	}

	// Wait for all claude processes to finish
	var wg sync.WaitGroup
	for _, a := range spawned {
		wg.Add(1)
		go func(ag spawnedAgent) {
			defer wg.Done()
			exitErr := ag.bridge.Cmd.Wait()
			ourPID := ag.bridge.Cmd.Process.Pid

			// Update daemon with final status
			status := "finished"
			errMsg := ""
			if exitErr != nil {
				status = "error"
				errMsg = exitErr.Error()
				fmt.Fprintf(os.Stderr, "%s finished with error: %v\n", ag.id, exitErr)
			} else {
				fmt.Printf("%s finished\n", ag.id)
			}

			// The pid and the socket paths belong to the agent id, not to this
			// process: a resume or a restart can bind a successor while this one is
			// still winding down. The exit write is therefore a compare-and-set on
			// our pid, and only a row that still names this process may lose the
			// socket files — otherwise a stale process unlinked the sockets its
			// successor is listening on (YM-202, same rule as the daemon's spawn exit
			// tail).
			// A failed write leaves the row as ours: a resume or a restart can only
			// rebind it through the daemon, and this daemon answered nothing at all.
			// The socket files are therefore removed as before instead of being left
			// behind by every exit with an unreachable daemon.
			ours := true
			c, err := daemon.DialTimeout(dataDir, 3*time.Second)
			if err == nil {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				res, callErr := c.Call("update_agent", agentExitWrite(ag.id, ourPID, status, errMsg))
				if callErr == nil {
					var out struct {
						Written bool `json:"written"`
					}
					if json.Unmarshal(res, &out) == nil {
						ours = out.Written
					}
				} else {
					fmt.Fprintf(os.Stderr, "%s: recording the exit state failed: %v\n", ag.id, callErr)
				}
			} else {
				fmt.Fprintf(os.Stderr, "%s: recording the exit state failed: %v\n", ag.id, err)
			}

			if ours {
				ag.bridge.Close()
			} else {
				ag.bridge.CloseKeepingSockets()
			}
		}(a)
	}
	wg.Wait()
	fmt.Printf("All %d agents finished.\n", len(spawned))
}

// agentExitWrite builds the update_agent request that records a CLI-started
// agent's exit. expected_pid makes the write a compare-and-set: a resume or a
// restart can bind a successor to the row while this process winds down, and the
// reply's "written" flag then tells the caller that the socket files belong to the
// successor (YM-202).
func agentExitWrite(id string, pid int, status, errMsg string) map[string]any {
	fields := map[string]any{
		"status":     status,
		"stopped_at": time.Now().UTC().Format(time.RFC3339),
	}
	if errMsg != "" {
		fields["error"] = errMsg
	}
	return map[string]any{"id": id, "fields": fields, "expected_pid": float64(pid)}
}

// buildSections returns a slice of length count. Entries from the comma-separated
// tasks string are used first; any remaining slots get a default "agent-N" name.
func buildSections(tasks string, count int) []string {
	sections := make([]string, count)
	if tasks != "" {
		parts := strings.Split(tasks, ",")
		for i, p := range parts {
			if i >= count {
				break
			}
			sections[i] = strings.TrimSpace(p)
		}
	}
	for i := range sections {
		if sections[i] == "" {
			sections[i] = fmt.Sprintf("agent-%d", i)
		}
	}
	return sections
}

// buildAgentPrompt returns the initial instruction for each spawned agent, or an
// error when a field cannot be rendered into it. The CLI hands the prompt to the
// backend as one argv element (no shell, no TUI keystrokes), so a quote cannot
// break an argument here; the fields are refused anyway because the same values are
// echoed into the text the agent reads, and because the daemon's own builder, which
// does type into a TUI, applies this rule (orchestrator.PromptFieldUnsafe).
func buildAgentPrompt(project, section, callerSession string) (string, error) {
	for _, f := range []struct{ name, value string }{
		{"project", project},
		{"section", section},
		{"caller_session", callerSession},
	} {
		if orchestrator.PromptFieldUnsafe(f.value) {
			return "", fmt.Errorf("%s contains characters that cannot be rendered into the agent prompt (quotes, control or bidi characters)", f.name)
		}
	}
	prompt := fmt.Sprintf(
		"You are working on project '%s', section '%s'. "+
			"FIRST ACTION: Immediately write scratchpad_append(project=\"%s\", section=\"%s\", content=\"Status: started\") so the main agent can see you are working. "+
			"Then read scratchpad_read(project=\"%s\", section=\"%s\") for context and work through the task. "+
			"Write your results with scratchpad_write(project=\"%s\", section=\"%s\", content=...).",
		project, section, project, section, project, section, project, section,
	)
	if callerSession != "" {
		prompt += fmt.Sprintf(
			" When you are DONE, send send_to(target=\"%s\", content=\"DONE: Section '%s' in project '%s' is finished.\") so the main agent is notified.",
			callerSession, section, project,
		)
	}
	return prompt, nil
}
