package plugins

import "embed"

// BundledOpencodePlugin contains the TypeScript plugin source files
// for the opencode YesMem plugin. Installed to
// ~/.local/share/yesmem/plugins/opencode-yesmem/ during setup.
//
//go:embed opencode-yesmem/*
var BundledOpencodePlugin embed.FS

// WindowPlugin contains the v2 opencode TUI plugin that registers which
// session a terminal window shows (register_window), so spawned agents open
// on the caller's desktop. Installed to
// ~/.config/opencode/plugins/yesmem-window/, where opencode auto-discovers
// and hot-reloads it into every running TUI.
//
//go:embed yesmem-window/tui.tsx
var WindowPlugin embed.FS

// AgentThemes contains the opencode TUI theme family: one shared dark base with
// one accent per agent type (yesmem-dark for the orchestrator, yesmem-loop for
// yesloop, yesmem-research for yesresearch, yesmem-subresearch for its
// sub-agents). Installed to ~/.config/opencode/themes/ during setup, where
// opencode discovers theme files for every TUI.
//
//go:embed themes/*.json
var AgentThemes embed.FS
