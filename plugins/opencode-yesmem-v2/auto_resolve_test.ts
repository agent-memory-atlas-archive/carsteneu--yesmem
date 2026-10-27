// auto_resolve_test.ts — v2 guard: the commit hook must use the graded commit
// resolver, never the plain text match (YM-260).
// Run: ~/.bun/bin/bun test plugins/opencode-yesmem-v2/auto_resolve_test.ts

import { describe, expect, test } from "bun:test"
import { autoResolve } from "./auto_resolve"

type Call = { method: string; params: any }

function fakeRpc(result: any = { resolved: true }) {
  const calls: Call[] = []
  return {
    calls,
    call: async (method: string, params?: any) => {
      calls.push({ method, params })
      return { ok: true, result }
    },
  }
}

function commitEvent(command: string) {
  return { tool: "shell", status: "completed", input: { command } }
}

describe("autoResolve", () => {
  test("calls resolve_by_commit with the raw message", async () => {
    const rpc = fakeRpc()
    const hook = autoResolve(rpc as any, "/home/chief/memory/yesmem")

    await hook.after(commitEvent(`git commit -m "fix(proxy): keepalive"`))

    expect(rpc.calls).toHaveLength(1)
    expect(rpc.calls[0].method).toBe("resolve_by_commit")
    expect(rpc.calls[0].params.message).toBe("fix(proxy): keepalive")
    expect(rpc.calls[0].params.project).toBe("/home/chief/memory/yesmem")
  })

  test("never falls back to resolve_by_text", async () => {
    const rpc = fakeRpc()
    const hook = autoResolve(rpc as any, "/p")

    await hook.after(commitEvent(`git commit -m "Los 11: Proxy Rules-Cache und Injektionszähler"`))

    expect(rpc.calls.map((c) => c.method)).not.toContain("resolve_by_text")
  })

  test("ignores a failed commit", async () => {
    const rpc = fakeRpc()
    const hook = autoResolve(rpc as any, "/p")

    await hook.after({ tool: "shell", status: "error", input: { command: `git commit -m "nope"` } })

    expect(rpc.calls).toHaveLength(0)
  })
})
