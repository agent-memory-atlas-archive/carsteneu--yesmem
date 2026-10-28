package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/config"
	"github.com/carsteneu/yesmem/internal/daemon"
	"gopkg.in/yaml.v3"
)

func runConfig(args []string) {
	dataDir := yesmemDataDir()
	cfgPath := filepath.Join(dataDir, "config.yaml")

	if len(args) == 0 {
		// Show full config
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config: load: %v\n", err)
			os.Exit(1)
		}
		out, err := yaml.Marshal(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config: marshal: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(string(out))
		return
	}

	cmd := args[0]
	switch cmd {
	case "get":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "Usage: yesmem config get <dot-path>")
			fmt.Fprintln(os.Stderr, "  Example: yesmem config get extraction.model")
			os.Exit(1)
		}
		key := args[1]
		value, err := config.GetValue(cfgPath, key)
		if err != nil {
			fmt.Fprintf(os.Stderr, "config get: %v\n", err)
			os.Exit(1)
		}
		if value == nil {
			fmt.Printf("config.%s: (not set)\n", key)
			return
		}
		// Format nicely
		switch v := value.(type) {
		case string:
			fmt.Println(v)
		case float64, int, bool:
			fmt.Printf("%v\n", v)
		case map[string]any, []any:
			b, _ := json.MarshalIndent(v, "", "  ")
			fmt.Println(string(b))
		default:
			fmt.Printf("%v\n", v)
		}

	case "set":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: yesmem config set <dot-path> <value>")
			fmt.Fprintln(os.Stderr, "  Example: yesmem config set extraction.model opus")
			os.Exit(1)
		}
		key := args[1]
		value := strings.Join(args[2:], " ")
		if err := setConfigValue(dataDir, cfgPath, key, value); err != nil {
			fmt.Fprintf(os.Stderr, "config set: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("config.%s = %s\n", key, value)

	case "show":
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("Config file not found: " + cfgPath)
				fmt.Println("Run 'yesmem setup' to create one.")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "config show: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(string(data))

	default:
		fmt.Fprintf(os.Stderr, "Usage: yesmem config [get|set|show]\n")
		fmt.Fprintln(os.Stderr, "  yesmem config            Show merged config (defaults + overrides)")
		fmt.Fprintln(os.Stderr, "  yesmem config show       Show raw config.yaml")
		fmt.Fprintln(os.Stderr, "  yesmem config get <key>  Get a config value by dot-path")
		fmt.Fprintln(os.Stderr, "  yesmem config set <key> <value>  Set a config value by dot-path")
		os.Exit(1)
	}
}

// setConfigValue writes one key into config.yaml and, for a proxy key, into the
// daemon's runtime slot. The path is normalized onto its schema location and the
// value is coerced to the field type first, the same two steps the daemon's
// set_config takes: a value that does not fit the field makes Load reject the
// whole file.
func setConfigValue(dataDir, cfgPath, key, rawValue string) error {
	normKey, err := config.NormalizeSetPath(key)
	if err != nil {
		return err
	}
	coerced, err := config.CoerceValueForPath(normKey, config.CoerceValue(rawValue))
	if err != nil {
		return err
	}
	if err := config.SetValue(cfgPath, normKey, coerced); err != nil {
		return err
	}
	// The running proxy re-reads only the token-threshold family from
	// proxy_state. Mirroring any other proxy key would write a slot that no proxy
	// code consumes while get_config starts reporting it as the effective value.
	// The daemon's own set_config still dual-writes broadly (pre-existing); this
	// path stays honest about what the proxy applies.
	stateKey, hasRuntimeReader := config.ProxyRuntimeStateKey(normKey)
	if !hasRuntimeReader {
		if _, isProxyKey := config.ProxyStateKey(normKey); isProxyKey {
			fmt.Fprintf(os.Stderr, "config set: %q written; the proxy applies it on its next start.\n", normKey)
		}
		return nil
	}
	if err := mirrorProxyState(dataDir, stateKey, coerced); err != nil {
		// The file is the primary store, and the proxy re-reads this family from
		// it. A slot that could not be written is worth a warning, not a failed
		// write.
		fmt.Fprintf(os.Stderr, "config set: %q written; the runtime slot was not updated (%v). The value applies within one proxy refresh window or at the next start.\n", normKey, err)
	}
	return nil
}

// mirrorProxyState writes one runtime slot through the daemon's socket.
func mirrorProxyState(dataDir, stateKey string, value any) error {
	client, err := daemon.DialTimeout(dataDir, 3*time.Second)
	if err != nil {
		return err
	}
	defer client.Close()
	// DialTimeout bounds the dial only; without a deadline on the connection a
	// daemon that accepts and then stalls would hang the CLI.
	if err := client.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	if _, err := client.Call("set_proxy_state", map[string]any{"key": stateKey, "value": fmt.Sprintf("%v", value)}); err != nil {
		return err
	}
	return nil
}
