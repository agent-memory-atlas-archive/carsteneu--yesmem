// log_test.ts — YM-460: a test run must not append to the live plugin log.
//
// Only two suites import the fixture that points YESMEM_LOG_FILE at a temp file; the rest wrote their debug
// lines into ~/.claude/yesmem/logs/plugin-v2.log (measured 25.10.: code_nav_test +3547 bytes,
// code_nav_mode_test +766, auto_resolve_test +192). The guard therefore has to live in the writer itself:
// no suite can forget an import it never had.
//
// Run: bun test plugins/opencode-yesmem-v2/log_test.ts

import { describe, expect, test } from "bun:test"
import { existsSync, mkdirSync, mkdtempSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { debugLog } from "./log"

function withEnv<T>(environment: Record<string, string | undefined>, body: () => T): T {
  const previous = new Map(Object.keys(environment).map((key) => [key, process.env[key]]))
  for (const [key, value] of Object.entries(environment)) {
    if (value === undefined) delete process.env[key]
    else process.env[key] = value
  }
  try {
    return body()
  } finally {
    for (const [key, value] of previous) {
      if (value === undefined) delete process.env[key]
      else process.env[key] = value
    }
  }
}

describe("test-run log isolation (YM-460)", () => {
  test("without YESMEM_LOG_FILE a test run creates no log file", () => {
    const home = mkdtempSync(join(tmpdir(), "yesmem-logtest-"))
    // The directory must exist: a missing parent would swallow the write and make the test pass for the
    // wrong reason.
    const logs = join(home, ".claude/yesmem/logs")
    mkdirSync(logs, { recursive: true })

    withEnv({ HOME: home, YESMEM_LOG_FILE: undefined }, () => {
      debugLog("test", "must never reach the live install")
    })

    // The writer would have created this file and appended to it before the fix.
    expect(existsSync(join(logs, "plugin-v2.log"))).toBe(false)
  })

  test("an explicit YESMEM_LOG_FILE still receives the line", () => {
    const directory = mkdtempSync(join(tmpdir(), "yesmem-logtest-"))
    const file = join(directory, "plugin-v2.log")

    withEnv({ YESMEM_LOG_FILE: file }, () => {
      debugLog("test", "kept in the test's own file")
    })

    expect(readFileSync(file, "utf8")).toContain("kept in the test's own file")
  })
})
