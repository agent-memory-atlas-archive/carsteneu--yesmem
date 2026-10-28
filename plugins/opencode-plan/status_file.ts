// Reading the per-thread status file the proxy writes every second.
//
// Apart from the rendering rules on purpose: the file naming is an assumption
// about the proxy (thread ids carry a `<backend>:` prefix), and a wrong
// assumption here shows another session's plan in the sidebar. That is worth a
// test with a real directory, which a TUI slot cannot provide.

import { existsSync, readdirSync, readFileSync } from "node:fs"
import { join } from "node:path"

// statusCandidates lists the file names a session's status may live under. The
// proxy names the file after the thread id, which is `<backend>:<session>`, while
// the TUI knows the bare session id: both spellings are tried, the bare one
// first in case a future proxy drops the prefix.
//
// An id that is not made of the characters a session id uses yields no candidate
// at all: the id comes from the TUI's own session context, but a lookup that can
// be pointed at an arbitrary path is not worth the one saved line. The charset
// mirrors the proxy's own status-file rule (isStatusFileIDSafe).
export function statusCandidates(sessionId: string): string[] {
  if (!/^[A-Za-z0-9._:-]+$/.test(sessionId)) return []
  if (sessionId.includes("..")) return []
  return [`status-${sessionId}.json`, `status-opencode:${sessionId}.json`]
}

// readStatusFile reads this session's status file. It never falls back to
// another session's file: a cheap lookup is the point, the tick runs it twice a
// second.
export function readStatusFile<T>(dir: string, sessionId: string): T | null {
  for (const name of statusCandidates(sessionId)) {
    const path = join(dir, name)
    if (!existsSync(path)) continue
    try {
      return JSON.parse(readFileSync(path, "utf-8")) as T
    } catch {
      return null
    }
  }
  return null
}

// readNewestStatusFile is the mount-time fallback for a session whose file is
// not there yet. It scans the directory, which is far too expensive for the
// refresh tick: the directory holds one file per session ever seen. "Newest"
// here means the last name in lexical order — the session ids sort by creation
// time in practice, and this is a heuristic for a single display, not a lookup.
export function readNewestStatusFile<T>(dir: string): T | null {
  try {
    if (!existsSync(dir)) return null
    const files = readdirSync(dir)
      .filter((f) => f.startsWith("status-") && f.endsWith(".json"))
      .sort()
      .reverse()
    for (const file of files) {
      try {
        return JSON.parse(readFileSync(join(dir, file), "utf-8")) as T
      } catch {
        continue
      }
    }
  } catch {
    /* ignore */
  }
  return null
}
