import { afterEach, describe, expect, test } from "bun:test"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { readNewestStatusFile, readStatusFile, statusCandidates } from "./status_file"

// The proxy names the status file after the thread id (`opencode:<session>`),
// while the TUI knows the bare session id. Getting this wrong means the sidebar
// draws another session's plan, so the lookup is tested against real files.

const dirs: string[] = []

function tempDir(): string {
  const dir = mkdtempSync(join(tmpdir(), "yesmem-status-test-"))
  dirs.push(dir)
  return dir
}

afterEach(() => {
  for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true })
})

describe("statusCandidates", () => {
  test("tries the bare id and the prefixed thread id", () => {
    expect(statusCandidates("ses_abc")).toEqual([
      "status-ses_abc.json",
      "status-opencode:ses_abc.json",
    ])
  })

  test("refuses an id that would leave the directory", () => {
    for (const id of ["a/../../x", "..", "a\\b", "a b"]) {
      expect(statusCandidates(id)).toEqual([])
    }
  })
})

describe("readStatusFile", () => {
  test("finds the file the proxy writes for this session", () => {
    const dir = tempDir()
    writeFileSync(join(dir, "status-opencode:ses_abc.json"), JSON.stringify({ thread_id: "opencode:ses_abc" }))
    const got = readStatusFile<{ thread_id: string }>(dir, "ses_abc")
    expect(got?.thread_id).toBe("opencode:ses_abc")
  })

  test("accepts an unprefixed file name too", () => {
    const dir = tempDir()
    writeFileSync(join(dir, "status-ses_abc.json"), JSON.stringify({ thread_id: "ses_abc" }))
    expect(readStatusFile<{ thread_id: string }>(dir, "ses_abc")?.thread_id).toBe("ses_abc")
  })

  test("returns null instead of another session's file", () => {
    const dir = tempDir()
    writeFileSync(join(dir, "status-opencode:ses_other.json"), JSON.stringify({ thread_id: "other" }))
    expect(readStatusFile(dir, "ses_abc")).toBeNull()
  })

  test("survives a half-written file", () => {
    const dir = tempDir()
    writeFileSync(join(dir, "status-opencode:ses_abc.json"), "{not json")
    expect(readStatusFile(dir, "ses_abc")).toBeNull()
  })

  test("missing directory is not an error", () => {
    expect(readStatusFile(join(tempDir(), "nope"), "ses_abc")).toBeNull()
  })
})

describe("readNewestStatusFile", () => {
  test("falls back to the most recent file and skips broken ones", () => {
    const dir = tempDir()
    writeFileSync(join(dir, "status-a.json"), JSON.stringify({ thread_id: "a" }))
    writeFileSync(join(dir, "status-b.json"), "{broken")
    expect(readNewestStatusFile<{ thread_id: string }>(dir)?.thread_id).toBe("a")
  })

  test("empty or missing directory yields null", () => {
    expect(readNewestStatusFile(tempDir())).toBeNull()
    expect(readNewestStatusFile(join(tempDir(), "nope"))).toBeNull()
  })
})
