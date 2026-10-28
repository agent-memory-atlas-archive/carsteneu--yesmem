// code_nav_mode_test.ts — YM-442 (Los 61d): the v2 guard reads code_nav_mode.
//
// The Go PreToolUse hook has honored the setting since los-60c (block, nudge, off);
// the opencode plugin blocked whatever the value said, so one setting had two
// addressees and two behaviours. `tool.execute.before` can only reject a call, but
// the plugin already ships a pass-with-hint channel: `execute.after` prefixes the
// tool result (rule_guard.injectPrefix). The guard uses it for `nudge`.
//
// Run: bun test plugins/opencode-yesmem-v2/code_nav_mode_test.ts

import { describe, expect, test } from "bun:test"
import { codeNavGuard, parseCodeNavMode } from "./code_nav"

type Call = { method: string; params: any }

function los61dRpc() {
  const calls: Call[] = []
  return {
    calls,
    call: async (method: string, params?: any) => {
      calls.push({ method, params })
      if (method === "search_code_index") return { ok: true, result: { text: "Found 1 matches" } }
      if (method === "get_file_symbols") return { ok: true, result: { text: "**internal/x.go** — 3 symbols" } }
      if (method === "get_file_index") return { ok: true, result: { text: "**internal** — 3 files" } }
      if (method === "is_code_nav_dismissed") return { ok: true, result: { dismissed: false } }
      return { ok: true, result: {} }
    },
  }
}

function shellCall(id = "call_9", command = "cat internal/x.go") {
  return { tool: "shell", sessionID: "ses_1", id, input: { command } }
}

function resultOf(id = "call_9") {
  return { id, status: "success", result: { content: "package x" } }
}

function guard(mode: "block" | "nudge" | "off") {
  return codeNavGuard(los61dRpc() as any, "/proj", () => 1_000_000, () => mode)
}

describe("parseCodeNavMode — the same normalization as the Go hook", () => {
  test("reads the value under proxy", () => {
    expect(parseCodeNavMode("proxy:\n  code_nav_mode: nudge\n")).toBe("nudge")
    expect(parseCodeNavMode("proxy:\n  code_nav_mode: off\n")).toBe("off")
    expect(parseCodeNavMode("proxy:\n  code_nav_mode: block\n")).toBe("block")
  })

  test("ignores case and surrounding spaces", () => {
    expect(parseCodeNavMode('proxy:\n  code_nav_mode: "  OFF "\n')).toBe("off")
    expect(parseCodeNavMode("proxy:\n  code_nav_mode: NuDgE\n")).toBe("nudge")
  })

  test("an unknown value and a missing key block", () => {
    expect(parseCodeNavMode("proxy:\n  code_nav_mode: nonsense\n")).toBe("block")
    expect(parseCodeNavMode("proxy:\n  provider: x\n")).toBe("block")
    expect(parseCodeNavMode("")).toBe("block")
  })

  test("a key outside the proxy section is not the setting", () => {
    expect(parseCodeNavMode("extraction:\n  code_nav_mode: off\n")).toBe("block")
  })

  test("comments and blank lines do not shift the section", () => {
    expect(parseCodeNavMode("# head\n\nproxy:\n  # inner\n  code_nav_mode: nudge\n")).toBe("nudge")
  })

  test("drops a trailing comment on the value", () => {
    expect(parseCodeNavMode("proxy:\n  code_nav_mode: nudge # advice only\n")).toBe("nudge")
  })

  test("a key nested below a proxy sub-mapping is not the setting", () => {
    expect(parseCodeNavMode("proxy:\n  other:\n    code_nav_mode: off\n")).toBe("block")
  })
})

describe("code_nav_mode nudge — pass with a hint", () => {
  test("lets the call through", async () => {
    const nav = guard("nudge")
    expect(await nav.before(shellCall())).toBeUndefined()
  })

  test("hands the hint to the tool result", async () => {
    const nav = guard("nudge")
    await nav.before(shellCall())
    const event = resultOf()

    nav.after(event)

    const text = String(event.result.content)
    expect(text).toContain("code_nav_mode")
    expect(text).toContain("internal/x.go")
    expect(text).toContain("get_file_symbols")
    // The tool's own output stays readable behind the hint.
    expect(text).toContain("package x")
  })

  test("a hint is delivered once, not on every later call", async () => {
    const nav = guard("nudge")
    await nav.before(shellCall())
    const first = resultOf()
    nav.after(first)
    const second = resultOf()
    nav.after(second)

    expect(String(second.result.content)).toBe("package x")
  })

  test("does not ask the daemon whether the session dismissed the guard", async () => {
    const rpc = los61dRpc()
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000, () => "nudge")
    await nav.before(shellCall())

    expect(rpc.calls.filter((call) => call.method === "is_code_nav_dismissed")).toHaveLength(0)
  })

  test("a hint recorded for a call that failed is dropped", async () => {
    const nav = guard("nudge")
    await nav.before(shellCall())
    const event = { id: "call_9", status: "error", error: { message: "boom" } }

    nav.after(event)

    expect(event.error.message).toBe("boom")
  })

  test("a result that is neither text nor a content list keeps its payload", async () => {
    const nav = guard("nudge")
    await nav.before(shellCall())
    const payload = { lines: 3 }
    const event = { id: "call_9", status: "success", result: { content: payload as unknown } }

    nav.after(event)

    expect(event.result.content).toBe(payload)
  })
})

describe("code_nav_mode off — no advice at all", () => {
  test("lets the call through and writes nothing", async () => {
    const nav = guard("off")
    expect(await nav.before(shellCall())).toBeUndefined()
    const event = resultOf()

    nav.after(event)

    expect(String(event.result.content)).toBe("package x")
  })

  test("answers before the index is asked", async () => {
    const rpc = los61dRpc()
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000, () => "off")

    expect(await nav.before(shellCall())).toBeUndefined()
    expect(rpc.calls).toHaveLength(0)
  })
})

describe("code_nav_mode — the value is re-read after the TTL", () => {
  test("a changed value lands after a minute, not within it", async () => {
    let clock = 1_000_000
    let value: "block" | "nudge" = "block"
    const nav = codeNavGuard(los61dRpc() as any, "/proj", () => clock, () => value)

    expect(await nav.before(shellCall("call_a", "cat internal/a.go"))).toContain("YesMem: BLOCKED")

    value = "nudge"
    clock += 30_000
    expect(await nav.before(shellCall("call_b", "cat internal/b.go"))).toContain("YesMem: BLOCKED")

    clock += 31_000
    expect(await nav.before(shellCall("call_c", "cat internal/c.go"))).toBeUndefined()
  })
})

describe("code_nav_mode block — unchanged", () => {
  test("still blocks and delivers no hint", async () => {
    const nav = guard("block")
    expect(await nav.before(shellCall())).toContain("YesMem: BLOCKED")
    const event = resultOf()

    nav.after(event)

    expect(String(event.result.content)).toBe("package x")
  })
})
