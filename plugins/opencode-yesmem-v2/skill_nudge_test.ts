// skill_nudge_test.ts — YM-88: the nudge must read the same RULES.md the guard reads.
//
// rule_guard loads <project>/RULES.md. The nudge accepted a list of candidates that put the
// plugin directory first, so a copy shipped next to the plugin would shadow the project file —
// the mechanism that let the two catalogues drift apart in v1.
// Run: bun test plugins/opencode-yesmem-v2/skill_nudge_test.ts

import { afterAll, beforeAll, describe, expect, mock, test } from "bun:test"
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

// skill_nudge -> rule_guard -> @opencode/plugin/promise/tool. That package lives inside the opencode
// fork; the fixture registers the stand-in for it so every test file in this directory shares one
// definition. A competing plain Error class here would win over the fixture's and break the yieldable
// tagged error index.ts relies on.
import "./opencode-yesmem-fixture"

const { cacheKeyLabel, catalogCandidates, skillNudge } = await import("./skill_nudge")
type ContextEvent = import("./skill_nudge").ContextEvent

function dirWithCatalog(trigger: string, skill: string): string {
  const dir = mkdtempSync(join(tmpdir(), "yesmem-v2-skill-nudge-"))
  writeFileSync(
    join(dir, "RULES.md"),
    [
      "# Rules",
      "",
      "## Rules",
      "1. A rule that is not a skill trigger.",
      "",
      "## Skill Catalog",
      "rules:",
      "  - id: 900",
      `    skill: ${skill}`,
      "    priority: MUST",
      `    triggers: ["${trigger}"]`,
      `    rule: "probe rule for ${skill}"`,
      "",
    ].join("\n"),
  )
  return dir
}

function userEvent(text: string): ContextEvent {
  // v2 host messages are flat: {id, role, content[], metadata}. A human turn carries metadata
  // (synthetic and derived messages are told apart by it, see messages.ts).
  return {
    sessionID: "ses_1",
    messages: [{ id: "msg_1", role: "user", metadata: {}, content: [{ type: "text", text }] }],
  }
}

async function nudgedText(paths: readonly string[], text: string): Promise<string> {
  const nudge = skillNudge(paths)
  const event = userEvent(text)
  await nudge.context(event)
  return event.messages![0].content![0].text ?? ""
}

describe("skill_nudge catalogue sources (YM-88)", () => {
  test("the project RULES.md comes before a copy next to the plugin", () => {
    const pluginDir = dirWithCatalog("zzz-plugin-trigger", "brainstorming")
    const projectDir = dirWithCatalog("zzz-project-trigger", "brainstorming")

    const candidates = catalogCandidates({ pluginDirectory: pluginDir, projectDirectory: projectDir })

    expect(candidates[0]).toBe(join(projectDir, "RULES.md"))
    expect(candidates[1]).toBe(join(pluginDir, "RULES.md"))
  })

  test("an explicit catalogue override still wins", () => {
    const override = join(dirWithCatalog("zzz-env-trigger", "brainstorming"), "RULES.md")
    const candidates = catalogCandidates({
      override,
      pluginDirectory: "/plugin",
      projectDirectory: "/project",
    })

    expect(candidates[0]).toBe(override)
  })

  test("an override that is not named RULES.md is kept verbatim", () => {
    const candidates = catalogCandidates({
      override: "/home/user/catalogs/my-own-catalog.md",
      pluginDirectory: "/plugin",
      projectDirectory: "/project",
    })

    expect(candidates[0]).toBe("/home/user/catalogs/my-own-catalog.md")
  })

  test("falls back to the plugin copy when the project file has no Skill Catalog", async () => {
    const pluginDir = dirWithCatalog("zzz-plugin-trigger", "brainstorming")
    const projectDir = mkdtempSync(join(tmpdir(), "yesmem-v2-skill-nudge-nocat-"))
    writeFileSync(join(projectDir, "RULES.md"), "# Rules\n\n## Rules\n1. A project rule without any catalog.\n")

    const text = await nudgedText(
      catalogCandidates({ pluginDirectory: pluginDir, projectDirectory: projectDir }),
      "please run the zzz-plugin-trigger now",
    )

    expect(text).toContain("MANDATORY CHECK — activate brainstorming")
  })

  test("the nudge reads the project catalogue, not the plugin copy", async () => {
    const pluginDir = dirWithCatalog("zzz-plugin-trigger", "brainstorming")
    const projectDir = dirWithCatalog("zzz-project-trigger", "yesmem-config")

    const text = await nudgedText(
      catalogCandidates({ pluginDirectory: pluginDir, projectDirectory: projectDir }),
      "please run the zzz-project-trigger now",
    )

    // Two distinct skills: the nudge can only name the project's entry.
    expect(text).toContain("MANDATORY CHECK — activate yesmem-config")
    expect(text).not.toContain("brainstorming")
  })

  test("the plugin copy is the fallback when the project has no RULES.md", async () => {
    const pluginDir = dirWithCatalog("zzz-plugin-trigger", "brainstorming")
    const emptyProject = mkdtempSync(join(tmpdir(), "yesmem-v2-skill-nudge-empty-"))

    const text = await nudgedText(
      catalogCandidates({ pluginDirectory: pluginDir, projectDirectory: emptyProject }),
      "please run the zzz-plugin-trigger now",
    )

    expect(text).toContain("MANDATORY CHECK — activate brainstorming")
  })

  test("leaves a message without a trigger untouched", async () => {
    const projectDir = dirWithCatalog("zzz-project-trigger", "brainstorming")
    const text = await nudgedText([join(projectDir, "RULES.md")], "plain sentence without any trigger")

    expect(text).toBe("plain sentence without any trigger")
  })

  test("stops nudging when the catalogue disappears", async () => {
    const projectDir = dirWithCatalog("zzz-project-trigger", "brainstorming")
    const path = join(projectDir, "RULES.md")
    const nudge = skillNudge([path])

    const first = userEvent("please run the zzz-project-trigger now")
    await nudge.context(first)
    expect(first.messages![0].content![0].text).toContain("MANDATORY CHECK")

    // Rule file loses its catalogue: the next turn must not keep using the stale one.
    writeFileSync(path, "# Rules\n\n## Rules\n1. Only a rule now.\n")
    const second = userEvent("please run the zzz-project-trigger now")
    await nudge.context(second)
    expect(second.messages![0].content![0].text).toBe("please run the zzz-project-trigger now")
  })
})

describe("skill_nudge log privacy (YM-22)", () => {
  const saved = {
    HOME: process.env.HOME,
    LOG: process.env.YESMEM_LOG_FILE,
    URL: process.env.YESMEM_GUARD_URL,
    KEY: process.env.YESMEM_GUARD_KEY,
  }
  let log = ""

  beforeAll(() => {
    // Empty provider registry: the model stage answers "no key", so no request leaves the machine.
    const home = mkdtempSync(join(tmpdir(), "yesmem-los22-nudge-home-"))
    mkdirSync(join(home, ".cache", "opencode"), { recursive: true })
    writeFileSync(join(home, ".cache", "opencode", "models.json"), "{}")
    log = join(mkdtempSync(join(tmpdir(), "yesmem-los22-nudge-log-")), "plugin-v2.log")
    writeFileSync(log, "")
    process.env.HOME = home
    process.env.YESMEM_LOG_FILE = log
    delete process.env.YESMEM_GUARD_URL
    delete process.env.YESMEM_GUARD_KEY
  })

  afterAll(() => {
    if (saved.HOME === undefined) delete process.env.HOME
    else process.env.HOME = saved.HOME
    for (const [key, value] of [
      ["YESMEM_LOG_FILE", saved.LOG],
      ["YESMEM_GUARD_URL", saved.URL],
      ["YESMEM_GUARD_KEY", saved.KEY],
    ] as const) {
      if (value === undefined) delete process.env[key]
      else process.env[key] = value
    }
  })

  test("the key label keeps the catalogue hash and hides the user text", () => {
    const label = cacheKeyLabel("-123\u0000LOS22SECRET")

    expect(label).toMatch(/^-123#-?\d+$/)
    expect(label).not.toContain("LOS22SECRET")
  })

  test("a log line about the cache never carries the user message", async () => {
    const secret = "LOS22USERTEXT-do-not-log-me"
    const projectDir = dirWithCatalog("zzz-project-trigger", "brainstorming")
    const nudge = skillNudge([join(projectDir, "RULES.md")])

    const event = userEvent(secret)
    await nudge.context(event)

    // The model stage runs in the background; poll for its log line.
    let content = ""
    for (let attempt = 0; attempt < 40; attempt++) {
      content = readFileSync(log, "utf8")
      if (content.includes("skill_nudge")) break
      await Bun.sleep(50)
    }

    expect(content).toContain("skill_nudge")
    expect(content).not.toContain(secret)
  })
})
