// code_nav_test.ts — v2 guard: index probe caching (YM-83), command segments (YM-84)
// and the Go-hook parity of project key and path rewrite (YM-200).
// Run: bun test plugins/opencode-yesmem-v2/code_nav_test.ts

import { describe, expect, test } from "bun:test"
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { basename, join } from "node:path"
import {
  codeNavGuard,
  indexRelativePath,
  isNonCodeFile,
  navTargetList,
  projectKey,
  type ToolCallEvent,
} from "./code_nav"

type Call = { method: string; params: any; opts?: any }

function fakeRpc(handlers: Record<string, (params: any) => any> = {}) {
  const calls: Call[] = []
  return {
    calls,
    call: async (method: string, params?: any, opts?: any) => {
      calls.push({ method, params, opts })
      const handler = handlers[method]
      const result = handler ? handler(params) : {}
      // A handler may answer with an error response (ok:false) — that is how the
      // daemon reports a project without a code index.
      if (result && result.ok === false) return result
      return { ok: true, result }
    },
  }
}

// The daemon answers a symbol lookup with a text block; a miss says "No symbols found". A path that does
// not resolve inside the project (absolute or escaping) is a miss, too.
const symbols = (params: any) => {
  const file = String(params?.file ?? "")
  const outside = file.startsWith("/") || file.startsWith("..")
  return { text: outside ? `No symbols found in ${file}` : `**${file}** — 3 symbols` }
}
const INDEXED = { search_code_index: () => ({ text: "Found 1 matches" }) }
const NOT_INDEXED = {
  search_code_index: () => ({ ok: false, error: "no code index available for project: /proj" }),
}

function repoDir(name = "myrepo") {
  const dir = join(mkdtempSync(join(tmpdir(), "yesmem-v2-code-nav-")), name)
  mkdirSync(join(dir, ".git"), { recursive: true })
  return dir
}

function shell(command: string, sessionID = "ses_1"): ToolCallEvent {
  return { tool: "shell", sessionID, id: "call_1", input: { command } }
}

function readEvent(path: string, sessionID = "ses_1"): ToolCallEvent {
  return { tool: "read", sessionID, id: "call_2", input: { path } }
}

describe("isNonCodeFile — non-code whitelist", () => {
  test("keeps documentation and build metadata out of the guard", () => {
    for (const file of ["LICENSE", "LICENSE.txt", "README.md", "Dockerfile.dev", "Makefile", "CHANGELOG", "NOTES.txt", "docs/LICENSE"])
      expect(isNonCodeFile(file)).toBe(true)
    for (const file of ["main.go", "index.ts", "app.py", "handler.go"]) expect(isNonCodeFile(file)).toBe(false)
  })
})

describe("code_nav — index probe (YM-83)", () => {
  test("treats an existing index without a match as indexed", async () => {
    // A miss inside an existing index is ok:true with "No matches found ...";
    // a missing index is an error response.
    const rpc = fakeRpc({
      search_code_index: () => ({ text: "No matches found for pattern: func" }),
      get_file_symbols: symbols,
    })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("cat internal/x.go"))).toContain("YesMem: BLOCKED")
  })

  test("probes once and caches the negative answer for the TTL", async () => {
    let now = 1_000_000
    const rpc = fakeRpc(NOT_INDEXED)
    const nav = codeNavGuard(rpc as any, "/proj", () => now)

    await nav.before(shell("cat internal/x.go"))
    await nav.before(shell("cat internal/y.go"))
    await nav.before(shell("grep -n needle internal/z.go"))

    expect(rpc.calls.filter((call) => call.method === "search_code_index")).toHaveLength(1)

    now += 61_000
    await nav.before(shell("cat internal/x.go"))

    expect(rpc.calls.filter((call) => call.method === "search_code_index")).toHaveLength(2)
  })

  test("retries after a transport failure once the short TTL passed", async () => {
    let now = 1_000_000
    const rpc = fakeRpc({
      search_code_index: () => ({ ok: false, error: "rpc error: connect ECONNREFUSED" }),
    })
    const nav = codeNavGuard(rpc as any, "/proj", () => now)

    await nav.before(shell("cat internal/x.go"))
    await nav.before(shell("cat internal/y.go"))
    // A failed probe is no answer: within the short retry window the guard stays quiet instead of paying
    // the 2s probe on every call while a first scan is still running.
    expect(rpc.calls.filter((call) => call.method === "search_code_index")).toHaveLength(1)

    now += 6_000
    await nav.before(shell("cat internal/y.go"))
    expect(rpc.calls.filter((call) => call.method === "search_code_index")).toHaveLength(2)
  })

  test("stays silent while the index is missing", async () => {
    const rpc = fakeRpc(NOT_INDEXED)
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("cat internal/x.go"))).toBeUndefined()
  })

  test("bounds the probe with a short timeout", async () => {
    const rpc = fakeRpc(INDEXED)
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)
    await nav.before(shell("cat internal/x.go"))

    const probe = rpc.calls.find((call) => call.method === "search_code_index")
    expect(probe!.params.project).toBeDefined()
    expect(probe!.opts?.timeoutMs).toBe(2000)
  })

  test("priming asks the probe question and fills the cache", async () => {
    const rpc = fakeRpc(INDEXED)
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    await nav.prime()

    const probe = rpc.calls.find((call) => call.method === "search_code_index")
    expect(probe!.params.project_dir).toBe("/proj")
    expect(probe!.opts?.timeoutMs).toBe(2000)

    // The priming answer is the probe's answer: the first navigation does not probe again.
    await nav.before(shell("cat internal/x.go"))
    expect(rpc.calls.filter((call) => call.method === "search_code_index")).toHaveLength(1)
  })
})

describe("code_nav — shell command segments (YM-84)", () => {
  test("blocks a navigation command chained after cd", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("cd repo && grep -n needle internal/x.go"))).toContain("YesMem: BLOCKED")
  })

  test("blocks a navigation command after a semicolon", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("git status; cat internal/x.go"))).toContain("YesMem: BLOCKED")
  })

  test("leaves a chained command without a nav segment alone", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("cd repo && ls -la"))).toBeUndefined()
    expect(rpc.calls.filter((call) => call.method === "get_file_symbols")).toHaveLength(0)
  })

  test("resolves a nav target against the directory the chain changed into", async () => {
    const dir = join(mkdtempSync(join(tmpdir(), "yesmem-v2-cd-")), "project")
    mkdirSync(join(dir, ".git"), { recursive: true })
    mkdirSync(join(dir, "internal", "hooks"), { recursive: true })
    writeFileSync(join(dir, "internal", "hooks", "codenav.go"), "package hooks\n")
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, dir, () => 1_000_000)

    await nav.before(shell("cd internal && cat hooks/codenav.go"))

    const lookup = rpc.calls.find((call) => call.method === "get_file_symbols")
    expect(lookup!.params.file).toBe("internal/hooks/codenav.go")
  })

  test("does not block a nav target that the chain moved outside the project", async () => {
    const dir = repoDir("outside-repo")
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, dir, () => 1_000_000)

    expect(await nav.before(shell("cd /tmp && cat code_nav.ts"))).toBeUndefined()
  })

  test("counts every command as its own retry, not the leading file", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("cat internal/x.go"))).toContain("YesMem: BLOCKED")
    // A different command that happens to start with the same file is a first
    // strike of its own, not the retry of the command above.
    expect(await nav.before(shell("cat internal/x.go && cat internal/y.go"))).toContain("YesMem: BLOCKED")
    expect(await nav.before(shell("cat internal/x.go && cat internal/y.go"))).toBeUndefined()
  })

  test("keeps the strike memory per session", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("cat internal/x.go", "ses_A"))).toContain("YesMem: BLOCKED")
    // One plugin instance serves several sessions: a block in A must not hand B its retry.
    expect(await nav.before(shell("cat internal/x.go", "ses_B"))).toContain("YesMem: BLOCKED")
    expect(await nav.before(shell("cat internal/x.go", "ses_A"))).toBeUndefined()
  })

  test("blocks a read of an indexed file through the v2 path field", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(readEvent("internal/x.go"))).toContain("YesMem: BLOCKED")
    expect(await nav.before(readEvent("internal/x.go"))).toBeUndefined()
  })

  test("a read and a shell command on one file are separate strikes", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(readEvent("internal/x.go"))).toContain("YesMem: BLOCKED")
    expect(await nav.before(shell("grep -n needle internal/x.go"))).toContain("YesMem: BLOCKED")
  })

  test("resolves a nav target against the shell's workdir", async () => {
    const dir = join(mkdtempSync(join(tmpdir(), "yesmem-v2-workdir-")), "project")
    mkdirSync(join(dir, ".git"), { recursive: true })
    mkdirSync(join(dir, "plugins", "p"), { recursive: true })
    writeFileSync(join(dir, "plugins", "p", "code_nav.ts"), "export {}\n")
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, dir, () => 1_000_000)

    await nav.before({
      tool: "shell",
      sessionID: "ses_1",
      id: "call_3",
      input: { command: "cat code_nav.ts", workdir: "plugins/p" },
    })

    const lookup = rpc.calls.find((call) => call.method === "get_file_symbols")
    expect(lookup!.params.file).toBe("plugins/p/code_nav.ts")
  })

  test("blocks a redirect written without spaces", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("cat internal/x.go>out.json"))).toContain("YesMem: BLOCKED")
  })

  test("keeps the retry working when a relative redirect operand is added", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("cat internal/x.go"))).toContain("YesMem: BLOCKED")
    expect(await nav.before(shell("cat internal/x.go > out.json"))).toBeUndefined()
  })

  test("does not let a stray apostrophe swallow the rest of the line", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("git commit -m 'wip' ; echo don't && cat internal/x.go"))).toContain("YesMem: BLOCKED")
  })

  test("does not let two stray apostrophes swallow a nav segment between them", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("echo don't; cat internal/x.go; echo can't"))).toContain("YesMem: BLOCKED")
  })

  test("returns to the previous directory after popd", async () => {
    const dir = join(mkdtempSync(join(tmpdir(), "yesmem-v2-popd-")), "project")
    mkdirSync(join(dir, ".git"), { recursive: true })
    mkdirSync(join(dir, "sub"), { recursive: true })
    writeFileSync(join(dir, "root.go"), "package main\n")
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, dir, () => 1_000_000)

    await nav.before(shell("pushd sub && echo working && popd && cat root.go"))

    const lookups = rpc.calls.filter((call) => call.method === "get_file_symbols").map((call) => call.params.file)
    expect(lookups).toContain("root.go")
  })

  test("blocks tail like the Go hook does", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("tail -n 20 internal/x.go"))).toContain("YesMem: BLOCKED")
  })

  test("stays linear on a pathological quote pattern", async () => {
    const rpc = fakeRpc(INDEXED)
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)
    // Every quote sits at a boundary and none closes: a per-quote look-ahead is quadratic here.
    const command = 'cat x.go ' + '("a '.repeat(8192)

    const started = Date.now()
    await nav.before(shell(command))
    const elapsed = Date.now() - started

    expect(elapsed).toBeLessThan(3000)
  })

  test("bounds how many lookups one command may trigger", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)
    const command = "cat " + Array.from({ length: 500 }, (_, i) => `internal/f${i}.go`).join(" ")

    await nav.before(shell(command))

    expect(rpc.calls.filter((call) => call.method === "get_file_symbols").length).toBeLessThanOrEqual(32)
  })

  test("ignores a target containing NUL instead of letting it join the key", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    await nav.before(shell('cat internal/x.go "internal/a.go\u0000b.go"'))
    const lookups = rpc.calls.filter((call) => call.method === "get_file_symbols").map((call) => call.params.file)

    expect(lookups).not.toContain("internal/a.go\u0000b.go")
    // The same command without the NUL is still a first strike of its own.
    expect(await nav.before(shell("cat internal/x.go internal/a.go"))).toContain("YesMem: BLOCKED")
  })

  test("strips quotes and line separators from the advice", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)
    // A repository may hold a file whose name carries a quote and a line separator; the advice embeds it
    // inside a quoted pseudo-call, so both must not survive.
    const hostile = 'internal/a"b\u2028c.go'

    const message = await nav.before(shell(`cat '${hostile}'`))

    expect(message).toContain("YesMem: BLOCKED")
    expect(message).not.toContain("\u2028")
    expect(message).not.toContain('a"b')
    expect(message).toContain("internal/abc.go")
  })

  test("keeps the advertised retry working when a redirect is added", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("cat internal/x.go"))).toContain("YesMem: BLOCKED")
    // The block message tells the caller to run the command again; a redirect is
    // not part of the navigation target.
    expect(await nav.before(shell("cat internal/x.go 2>/dev/null"))).toBeUndefined()
  })

  test("reads a backslash-escaped path as one token", async () => {
    const dir = join(mkdtempSync(join(tmpdir(), "yesmem-v2-esc-")), "project")
    mkdirSync(join(dir, ".git"), { recursive: true })
    mkdirSync(join(dir, "my dir"), { recursive: true })
    writeFileSync(join(dir, "my dir", "x.go"), "package main\n")
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, dir, () => 1_000_000)

    await nav.before(shell("cd my\\ dir && cat x.go"))

    const lookup = rpc.calls.find((call) => call.method === "get_file_symbols")
    expect(lookup!.params.file).toBe("my dir/x.go")
  })
})

describe("code_nav — Go-hook parity of key and path (YM-200)", () => {
  test("projectKey returns the repository basename", () => {
    expect(projectKey(repoDir("myrepo"))).toBe("myrepo")
  })

  test("projectKey resolves a linked worktree to its parent repository", () => {
    const parent = mkdtempSync(join(tmpdir(), "yesmem-v2-worktree-"))
    const worktree = join(parent, "repo-wt")
    mkdirSync(worktree, { recursive: true })
    writeFileSync(join(worktree, ".git"), `gitdir: ${join(parent, "repo", ".git", "worktrees", "wt")}\n`)

    expect(projectKey(worktree)).toBe("repo")
  })

  test("projectKey resolves a nested directory to its repository", () => {
    const dir = repoDir("nested-repo")
    const sub = join(dir, "internal", "proxy")
    mkdirSync(sub, { recursive: true })

    expect(projectKey(sub)).toBe("nested-repo")
  })

  test("projectKey falls back to the directory name outside a repository", () => {
    const dir = mkdtempSync(join(tmpdir(), "yesmem-v2-plain-"))
    expect(projectKey(dir)).toBe(basename(dir))
  })

  test("indexRelativePath rewrites an absolute path inside the project", () => {
    expect(indexRelativePath("/repo", "/repo/internal/x.go")).toBe("internal/x.go")
  })

  test("indexRelativePath leaves a path outside the project untouched", () => {
    expect(indexRelativePath("/repo", "/etc/passwd")).toBe("/etc/passwd")
    expect(indexRelativePath("/repo", "/repo-sibling/x.go")).toBe("/repo-sibling/x.go")
  })

  test("indexRelativePath leaves a relative path untouched", () => {
    expect(indexRelativePath("/repo", "internal/x.go")).toBe("internal/x.go")
  })

  test("indexRelativePath matches Go for a target equal to the working dir", () => {
    expect(indexRelativePath("/repo", "/repo")).toBe(".")
  })

  test("the guard sends the Go project key and the file root, and suggests an absolute path", async () => {
    const dir = repoDir("parity-repo")
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, dir, () => 1_000_000)

    const message = await nav.before(shell(`cat ${join(dir, "internal", "x.go")}`))
    // The advice is a real tool call for the model: a bare index key resolves through the session history
    // and would point at the main checkout of a worktree.
    expect(message).toContain(`get_file_symbols("internal/x.go", "${dir}")`)

    const probe = rpc.calls.find((call) => call.method === "search_code_index")
    expect(probe!.params.project).toBe("parity-repo")
    // project_dir is the file root: the daemon resolves a project key to the
    // parent repository, so without it a worktree file is looked up in main.
    expect(probe!.params.project_dir).toBe(dir)
    expect(probe!.params._cwd).toBe(dir)

    const lookup = rpc.calls.find((call) => call.method === "get_file_symbols")
    expect(lookup!.params.project).toBe("parity-repo")
    expect(lookup!.params.project_dir).toBe(dir)
    expect(lookup!.params._cwd).toBe(dir)
    expect(lookup!.params.file).toBe("internal/x.go")
  })
})

describe("code_nav — dismissal and advice (YM-425)", () => {
  // The block message names dismiss_code_nav. The tool answers through the daemon and the guard blocks
  // inside the plugin process, so the guard has to read the state or the escape is a lie.
  test("honours a dismissed session instead of blocking again", async () => {
    const rpc = fakeRpc({
      ...INDEXED,
      get_file_symbols: symbols,
      is_code_nav_dismissed: () => ({ dismissed: true }),
    })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("grep -rn needle internal/x.go"))).toBeUndefined()
  })

  test("asks the daemon with the session id that would be blocked", async () => {
    const rpc = fakeRpc({
      ...INDEXED,
      get_file_symbols: symbols,
      is_code_nav_dismissed: () => ({ dismissed: false }),
    })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    expect(await nav.before(shell("grep -rn needle internal/x.go"))).toContain("YesMem: BLOCKED")

    const asked = rpc.calls.filter((call) => call.method === "is_code_nav_dismissed")
    expect(asked).toHaveLength(1)
    expect(asked[0].params.session_id).toBe("ses_1")
    expect(asked[0].opts?.timeoutMs).toBe(2000)
  })

  test("blocks when the daemon knows no dismissal for the session", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    // No handler: the daemon answered without a dismissal flag, which must not count as one.
    expect(await nav.before(shell("grep -rn needle internal/x.go"))).toContain("YesMem: BLOCKED")
  })

  test("does not ask the daemon when nothing would block", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    await nav.before(shell("go test ./internal/proxy/"))

    expect(rpc.calls.filter((call) => call.method === "is_code_nav_dismissed")).toHaveLength(0)
  })

  // A call-site question is what a shell grep usually asks. search_code answers it with file and line,
  // so the advice must name it next to the per-file tools.
  test("names search_code for call sites and the escape the guard honours", async () => {
    const rpc = fakeRpc({ ...INDEXED, get_file_symbols: symbols })
    const nav = codeNavGuard(rpc as any, "/proj", () => 1_000_000)

    const message = (await nav.before(shell("grep -rn needle internal/x.go"))) || ""

    expect(message).toContain('search_code("<symbol>("')
    expect(message).toContain("dismiss_code_nav")
    expect(message).toContain('session_id="ses_1"')
    // The retry is the second escape and it works: a second identical call is allowed.
    expect(message).toContain("run the same command again")
  })
})

describe("code_nav — shared fixtures with the Go hook (YM-409)", () => {
  // One fixture file, two implementations: the Claude Code hook and this plugin must resolve the same
  // shell line to the same targets. A divergence means a command blocks on one side and slips through on
  // the other, which is exactly the defect the fixture prevents from coming back.
  //
  // The fixture lives in the Go tree on purpose, so this suite only runs inside the repository. Copying
  // the plugin directory somewhere else fails here with ENOENT, which reads like a plugin defect but is
  // the missing fixture: run the tests from the repo root (`make test-plugin` locally; `bun install
  // --frozen-lockfile` and `bun test` in the CI job "plugin").
  const fixture = JSON.parse(
    readFileSync(join(import.meta.dir, "..", "..", "internal", "hooks", "testdata", "codenav_parity.json"), "utf8"),
  ) as { cwd: string; cases: { command: string; expected: { tool: string; file: string }[] }[] }

  test("the fixture file carries cases", () => {
    expect(fixture.cases.length).toBeGreaterThan(5)
  })

  for (const testCase of fixture.cases) {
    test(`resolves ${JSON.stringify(testCase.command)}`, () => {
      const targets = navTargetList(testCase.command, fixture.cwd)

      // Compared exactly: a trailing separator would mean one side treats the target as a directory and
      // the other as a file, which is the divergence this fixture exists to catch.
      expect(targets.map((target) => ({ tool: target.tool, file: target.file }))).toEqual(
        testCase.expected.map((target) => ({ tool: target.tool, file: target.file })),
      )
    })
  }})
