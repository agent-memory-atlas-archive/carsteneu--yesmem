// yesmem window registration (v2 TUI plugin).
//
// Registers which opencode session this terminal window shows with the
// yesmem daemon (register_window over ~/.claude/yesmem/daemon.sock), so
// spawn_agent can open new agent windows on the desktop of the terminal
// that spawned them. The TUI server plugin cannot do this: it runs in the
// shared `opencode serve` process, which has no window. This plugin runs
// in the TUI client — the process sitting in the ghostty window — and
// knows both the session it shows (ui.router.current) and its window
// (WINDOWID, set by ghostty). Modelled after the sibling plugins in
// ~/.config/opencode/plugins (export default Plugin.define).
import { Plugin } from "@opencode/plugin/tui"
import { appendFileSync, readFileSync } from "node:fs"

const LOG_FILE = `${process.env.HOME || "/home/chief"}/.claude/yesmem/logs/plugin.log`
function dbgLog(tag: string, msg: string) {
  try {
    appendFileSync(LOG_FILE, `[${new Date().toISOString()}] [yesmem-window] ${tag} ${msg}\n`)
  } catch {}
}

const SOCKET = `${process.env.HOME || "/home/chief"}/.claude/yesmem/daemon.sock`

// ghostty exports WINDOWID in decimal; wmctrl and the daemon use padded hex.
function windowIdFromEnv(env: Record<string, string | undefined>): string {
  const raw = env.WINDOWID
  if (!raw || !/^[0-9]+$/.test(raw)) return ""
  const n = Number(raw)
  if (!Number.isSafeInteger(n) || n <= 0) return ""
  return "0x" + n.toString(16).padStart(8, "0")
}

function isGhostty(env: Record<string, string | undefined>): boolean {
  return Boolean(env.GHOSTTY_RESOURCES_DIR) || env.TERM_PROGRAM === "ghostty"
}

// The pid of the ghostty process among pid's ancestors, i.e. the owner of
// the window WINDOWID names, or 0 when this process does not run in a
// ghostty window. Environment variables are inherited and can lie: the
// yesmem daemon passes its own environment to every agent TUI behind its
// PTY bridge, and the chain of a daemon-spawned agent ends at the daemon,
// never at ghostty. The walk stops at a yesmem process so a daemon started
// in a terminal does not lend its window to its agents.
function ghosttyAncestorPid(pid: number): number {
  const seen = new Set<number>()
  for (let hops = 0; hops < 16 && pid > 1 && !seen.has(pid); hops++) {
    seen.add(pid)
    let stat: string
    try {
      stat = readFileSync(`/proc/${pid}/stat`, "utf8")
    } catch {
      return 0
    }
    const open = stat.indexOf("(")
    const close = stat.lastIndexOf(")")
    if (open < 0 || close < open) return 0
    const comm = stat.slice(open + 1, close)
    if (comm === "yesmem") return 0
    if (comm.includes("ghostty")) return pid
    const ppid = Number(stat.slice(close + 1).trim().split(/\s+/)[1])
    if (!Number.isInteger(ppid)) return 0
    pid = ppid
  }
  return 0
}

async function registerWindow(sessionID: string, windowID: string, ownerPid: number): Promise<boolean> {
  const payload = JSON.stringify({
    jsonrpc: "2.0",
    id: 1,
    method: "register_window",
    params: {
      session_id: `opencode:${sessionID}`,
      window_id: windowID,
      terminal: "ghostty",
      window_pid: ownerPid,
    },
  })
  try {
    const out = await Bun.$`echo ${payload} | nc -U -N ${SOCKET}`.quiet()
    const text = out.text() ?? ""
    return text.includes(`"ok"`) || text.includes(`"status":"ok"`) || !text.includes(`"error"`)
  } catch {
    return false
  }
}

const ROUTE_POLL_MS = 1000
// Same-session re-registration heartbeat: heals a mapping another window
// briefly took over (mirrors the v1 per-prompt refresh).
const HEARTBEAT_TICKS = 30

export default Plugin.define({
  id: "yesmem.window-register",
  setup(context) {
    const env = process.env as Record<string, string | undefined>
    const windowID = isGhostty(env) ? windowIdFromEnv(env) : ""
    const ownerPid = windowID !== "" ? ghosttyAncestorPid(process.pid) : 0
    if (windowID === "" || ownerPid === 0) {
      dbgLog("init", "disabled: no ghostty window in process chain")
      return
    }
    if (typeof context?.ui?.router?.current !== "function") {
      dbgLog("init", "disabled: ui.router.current unavailable")
      return
    }

    let last = ""
    let ticks = 0
    const tick = async () => {
      try {
        const route = context.ui.router.current() as { type?: string; sessionID?: string } | undefined
        if (!route || route.type !== "session" || typeof route.sessionID !== "string") {
          // Leaving the session view: the next return must re-register.
          last = ""
          return
        }
        ticks++
        const heartbeat = ticks % HEARTBEAT_TICKS === 0
        if (route.sessionID === last && !heartbeat) return
        const ok = await registerWindow(route.sessionID, windowID, ownerPid)
        if (ok) {
          if (last !== route.sessionID) dbgLog("register", `${route.sessionID} -> ${windowID} (pid ${ownerPid})`)
          last = route.sessionID
        }
        // Failed (daemon down): keep last only on success path above, so the
        // next tick retries.
        if (!ok) last = ""
      } catch {
        // A plugin must never take the TUI down.
      }
    }

    dbgLog("init", `enabled window=${windowID} ownerPid=${ownerPid}`)
    void tick()
    const timer = setInterval(() => void tick(), ROUTE_POLL_MS)
    timer.unref?.()
  },
})
