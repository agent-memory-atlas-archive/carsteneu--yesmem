// YM-257 [PLG-F25] — the session id must not leak out of the session that owns it.
//
// Under opencode v2 one `opencode serve --service` process hosts every session. Two channels carried the id
// of the last-created session into unrelated sessions: the process environment (session.ts) and the shell
// environment (shell.ts). Both are covered here.

import { describe, expect, test, beforeEach, afterEach } from "bun:test"
import { sessionTracker } from "./session"
import { shellEnvironment } from "./shell"

const ENV_KEY = "YESMEM_SESSION_ID"

function fakeRpc() {
  const calls: Array<{ method: string; params: any }> = []
  return {
    calls,
    socket: "/tmp/yesmem-los23b-test.sock",
    call: async (method: string, params?: any) => {
      calls.push({ method, params })
      return { ok: true, result: { status: "ok" } }
    },
  }
}

let savedEnv: string | undefined

beforeEach(() => {
  savedEnv = process.env[ENV_KEY]
  delete process.env[ENV_KEY]
})

afterEach(() => {
  if (savedEnv === undefined) delete process.env[ENV_KEY]
  else process.env[ENV_KEY] = savedEnv
})

describe("sessionTracker", () => {
  test("session.created does not write the process-wide env", async () => {
    const rpc = fakeRpc()
    const tracker = sessionTracker(rpc as any)

    await tracker.observe({ type: "session.created", data: { sessionID: "ses_first" } })

    expect(process.env[ENV_KEY]).toBeUndefined()
    expect(rpc.calls[0]?.params.session_id).toBe("opencode:ses_first")
  })

  test("a later session does not overwrite the environment of the earlier one", async () => {
    const rpc = fakeRpc()
    const tracker = sessionTracker(rpc as any)

    await tracker.observe({ type: "session.created", data: { sessionID: "ses_a" } })
    await tracker.observe({ type: "session.created", data: { sessionID: "ses_b" } })

    // The shared service hosts both; nothing about either session may be visible process-wide.
    expect(process.env[ENV_KEY]).toBeUndefined()
    expect(rpc.calls.map((call) => call.params.session_id)).toEqual(["opencode:ses_a", "opencode:ses_b"])
  })
})

describe("shellEnvironment", () => {
  test("a shell without a host session id gets no foreign id", async () => {
    const rpc = fakeRpc()
    const tracker = sessionTracker(rpc as any)
    await tracker.observe({ type: "session.created", data: { sessionID: "ses_a" } })
    await tracker.observe({ type: "session.created", data: { sessionID: "ses_b" } })
    rpc.calls.length = 0

    const shell = shellEnvironment(rpc as any, "/tmp/yesmem-los23b.sock")
    // Session A spawns a shell after session B was created. The host hands over a copy of its own
    // environment, so the shell event arrives with whatever the service inherited: the plugin cannot know
    // A and must not let B's id through.
    const event: { env: Record<string, string | undefined>; sessionID?: string } = {
      env: { YESMEM_SESSION_ID: "ses_b" },
    }
    await shell.before(event)

    expect(event.env[ENV_KEY]).toBeUndefined()
    expect(event.env.YESMEM_SOCKET).toBe("/tmp/yesmem-los23b.sock")
    expect(event.env.YESMEM_SOURCE_AGENT).toBe("opencode")
    // The service PID identifies no session, so there is nothing to register.
    expect(rpc.calls.filter((call) => call.method === "register_pid")).toEqual([])
  })

  test("a shell that does carry a session id uses exactly that id", async () => {
    const rpc = fakeRpc()
    const shell = shellEnvironment(rpc as any, "/tmp/yesmem-los23b.sock")
    const event: { env: Record<string, string | undefined>; sessionID?: string } = {
      env: { YESMEM_SESSION_ID: "ses_stale" },
      sessionID: "ses_caller",
    }
    await shell.before(event)

    expect(event.env[ENV_KEY]).toBe("ses_caller")
    const registration = rpc.calls.find((call) => call.method === "register_pid")
    expect(registration?.params.session_id).toBe("opencode:ses_caller")
  })

  test("an empty or non-string host id is not forwarded either", async () => {
    const rpc = fakeRpc()
    const shell = shellEnvironment(rpc as any, "/tmp/yesmem-los23b.sock")

    for (const candidate of ["", "   ", 42, null, undefined]) {
      const event: { env: Record<string, string | undefined>; sessionID?: any } = {
        env: { YESMEM_SESSION_ID: "ses_stale" },
        sessionID: candidate,
      }
      await shell.before(event)

      expect(event.env[ENV_KEY]).toBeUndefined()
    }
    expect(rpc.calls).toEqual([])
  })
})
