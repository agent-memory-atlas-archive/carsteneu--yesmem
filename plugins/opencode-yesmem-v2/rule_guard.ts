// rule_guard — pre-tool rule compliance hook.
//
// Fires on every tool call and evaluates it against RULES.md via a small model.
//   BLOCK:   Write/Edit/Patch/Shell violating a rule → the call is rejected (Tool.Error)
//   SUGGEST: missed skill activation → directive injected into the tool result
//   PASS:    no issue
//
// Ported from the v1 plugin's rule_guard.ts. v2 differences:
//   - arguments live on the call event (`input`), not on a separate output object
//   - tool names are the v2 set: shell (not bash), plus the file-mutating `patch`
//   - the conversation capture uses `session.hook("context")`; v1's `chat.params` fallback has no v2
//     equivalent and is dropped
//   - `config.yaml` is read without the `yaml` module: the plugin runtime provides only `effect` and
//     `@opencode/plugin`, so a third-party YAML module cannot be relied on

import { readFile } from "node:fs/promises"
import { homedir } from "node:os"
import { Error as ToolError } from "@opencode/plugin/promise/tool"
import { readBodyCapped } from "./http"
import { debugLog } from "./log"
import { isSkillInstalled } from "./skill_whitelist"

type GuardResult = { decision: string; violations?: string[]; suggestion?: string; detail?: string }

// Tools whose BLOCK the hook can enforce: `execute.before` returns the message for any tool and index.ts
// turns it into a Tool.Error, so the decision only needs to be offered. Shell belongs here because the
// Decision Policy names two BLOCK rules that can only ever fire on a command - 25 (destructive bash) and
// 32 (PR merge via CLI/API) - and both were judged and thrown away before (YM-466). Note the reach: the
// guard sees the command through the same 300-character argument excerpt as every other call, so only the
// head of a long command is judged and a destructive tail beyond it is invisible. What the excerpt cannot
// see at all is indirection (a script written by `write` and run by `shell`) and alternative spellings of
// the same act - that limit is the policy's, not the guard's.
export const blockableTools = new Set(["write", "edit", "patch", "shell"])
// Tools refused when the guard itself has no answer. Deliberately narrower than blockableTools: an
// unjudged write can still land, an unjudged `ls` cannot. Refusing a shell call would take the session's
// whole ability to recover with it (no git status, no ls to find out why), which is the false positive
// that locks every call - so a shell call runs and carries the YM-410 notice instead (YM-466).
// Invariant: every entry here must also be in blockableTools. An entry that is not would refuse calls the
// guard was never allowed to block, so an endpoint outage would enforce a rule that does not exist.
export const failClosedTools = new Set(["write", "edit", "patch"])
const suggestableTools = new Set(["write", "edit", "patch", "read", "shell", "grep", "glob"])
const skipTools = new Set(["skill", "subagent", "todowrite"])

// A read-only GitHub or git call writes nothing and notifies nobody, so no rule in a catalogue about
// posting, promising, or destroying work can be violated by it. On 28.10.2026 the model answered BLOCK on
// `gh pr view 58662` with "this session is heading toward posting/commenting/reopening on GitHub": it read
// a direction into the call and judged the goal instead of the command. A boundary whose position depends
// on how the goal is worded is not a boundary, so this one is a list.
const READ_ONLY_SHELL_STARTS = [
  /^gh\s+pr\s+(view|list|diff|checks|status)\b/,
  /^gh\s+issue\s+(view|list)\b/,
  /^gh\s+run\s+(view|list)\b/,
  /^gh\s+release\s+(view|list)\b/,
  /^gh\s+repo\s+(view|list)\b/,
  /^gh\s+branch\s+list\b/,
  /^gh\s+search\s+(repos|issues|prs|code)\b/,
  /^git\s+ls-remote\b/,
]
// Anything that chains, redirects or substitutes: `gh pr view 1; gh pr close 1` is two calls, and only the
// first is the one this list knows.
const SHELL_CHAINING = /[;&|<>`\n]|\$\(/
// `gh api` is a GET only while it carries neither a method nor a field: `-f key=value` makes it a POST.
const GH_API_WRITE = /(^|\s)(-X|--method|-f|-F|--field|--raw-field|--input|-d)(\s|=|$)/

/** True only for a single command that provably reads. Everything else keeps going to the model. */
export function isReadOnlyShell(command: unknown): boolean {
  if (typeof command !== "string") return false
  const line = command.trim()
  if (!line || SHELL_CHAINING.test(line)) return false
  if (/^gh\s+api\b/.test(line)) return !GH_API_WRITE.test(line)
  return READ_ONLY_SHELL_STARTS.some((pattern) => pattern.test(line))
}


const GUARD_CACHE_TTL_MS = 5000
// How long a refused request is remembered. Short on purpose: it only bounds what a broken credential
// costs per tool call, it is not a verdict about the call.
const REFUSAL_RETRY_MS = 60_000
const MAX_GUARD_RESPONSE_CHARS = 100_000
const MAX_VIOLATION_CHARS = 1000
const MAX_LOG_EXCERPT_CHARS = 300

/**
 * A capped one-line excerpt for a log line. The body comes from a remote endpoint and may carry
 * newlines, so it is JSON-quoted: a raw body could otherwise forge a second log entry.
 */
function logExcerpt(text: string): string {
  const capped = text.length > MAX_LOG_EXCERPT_CHARS ? text.substring(0, MAX_LOG_EXCERPT_CHARS) + "…" : text
  return JSON.stringify(capped)
}

// Keyed on the full text, not a 32-bit hash: the map holds a handful of entries and a hash collision would
// hand one call another call's decision.
const guardCache = new Map<string, { result: GuardResult; ts: number }>()

// The last refused request. Deliberately not in guardCache: a refusal is no judgement about the call,
// it only stops an expired credential from costing a request timeout per tool call.
let lastRefusal: { key: string; result: GuardResult; ts: number } | null = null

/**
 * Test seam: forgets the module-level state so one test cannot inherit another's decision. Production
 * never calls it; the caches are keyed by content and expire on their own.
 */
export function resetGuardState() {
  guardCache.clear()
  lastRefusal = null
  lastResolution = ""
}

function guardCacheKey(rules: string, tool: string, context: string): string {
  return `${rules}\u0000${tool}\u0000${context}`
}

async function readText(file: string): Promise<string | undefined> {
  try {
    return await readFile(file, "utf8")
  } catch {
    return undefined
  }
}

async function loadRules(directory: string): Promise<string> {
  const content = await readText(`${directory}/RULES.md`)
  if (content === undefined) return "No RULES.md found in project root"
  // The numbered catalogue is built from the catalogue part only: a policy or skill section written as a
  // numbered list would otherwise feed existingCitedRules with rule numbers that do not exist (YM-456).
  const catalogue = content
    .replace(/## Decision Policy[^\n]*\n[\s\S]*?(?=\n## |$)/, "")
    .replace(/## Skill Catalog[^\n]*\n[\s\S]*?(?=\n## |$)/, "")
  const rules = catalogue
    .split("\n")
    .filter((line) => /^\d+\.\s/.test(line))
    .join("\n")
  // The Decision Policy says which rules may BLOCK. It sits above the numbered rules and carries no
  // numbers of its own, so filtering to `NN.` lines dropped it and the model had to guess (YM-456).
  const policy = sectionBody(content, "Decision Policy")
  const skillSection = sectionBody(content, "Skill Catalog")
  const parts = rules ? [rules] : []
  if (policy) parts.push(`## Decision Policy (binding: which rules may BLOCK)\n${policy}\n## End of Decision Policy`)
  if (skillSection) parts.push("## Skill Catalog\n" + skillSection)
  return parts.join("\n\n") || "No numbered rules found"
}

/** The body of one `## <title>` section, up to the next heading or horizontal rule. */
function sectionBody(content: string, title: string): string | undefined {
  const escaped = title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
  const match = new RegExp(`## ${escaped}\\n([\\s\\S]*?)(?=\\n## |\\n# [^#]|\\n---\\n|$)`).exec(content)
  return match ? match[1].trim() : undefined
}

export async function getGitBranch(directory: string): Promise<string> {
  try {
    // `.git` is a directory in a plain checkout and a `gitdir:` pointer file in a worktree. Reading it
    // as text fails on the directory, so the pointer decides only when it parses as one.
    const gitFile = (await readText(`${directory}/.git`)) ?? ""
    const match = gitFile.match(/^gitdir:\s*(.+)$/m)
    const gitDir = match ? match[1].trim() : `${directory}/.git`
    const head = (await readText(`${gitDir}/HEAD`)) ?? ""
    // Only a branch ref counts: a detached HEAD is a SHA, and calling that a feature branch would repeat
    // the false assurance YM-85 is about.
    const branch = /^ref:\s*refs\/heads\/(.+)$/.exec(head.trim())
    return branch ? branch[1] : ""
  } catch {
    return ""
  }
}

const defaultBranches = new Set(["main", "master"])

/**
 * The context line that tells the model a feature-branch rule is already satisfied. It belongs to a
 * feature branch only: on main the rule is exactly what the guard must keep suggesting (YM-85).
 */
export function featureBranchNote(branch: string): string {
  if (!branch || defaultBranches.has(branch)) return ""
  return `\nGit branch: ${branch} (feature branch rules are satisfied — no need to suggest worktree/branch skills)`
}

// ---------------------------------------------------------------------------------------------
// Guard configuration

/**
 * Reads the config.yaml values the guard needs. v1 used the `yaml` module; that module is not
 * available to the plugin runtime, so this scans the document instead. Only these shapes are understood:
 *
 *   rule_guard:
 *     model: <name>
 *   proxy:
 *     provider_targets:
 *       <model>: <url>
 *
 * The guard has its own model key on purpose: the extraction model is tuned for long transcripts and may
 * answer only in its reasoning channel, which left the guard judging nothing (YM-78).
 */
export function parseGuardConfig(text: string): { ruleGuardModel?: string; providerTargets: Record<string, string> } {
  const providerTargets: Record<string, string> = {}
  let ruleGuardModel: string | undefined
  let section: string | undefined
  let subsectionIndent = -1
  let inProviderTargets = false

  const unquote = (value: string) => value.replace(/^["']|["']$/g, "").trim()

  for (const raw of text.split("\n")) {
    const line = raw.trim()
    if (!line || line.startsWith("#")) continue
    const indent = raw.length - raw.trimStart().length
    const match = /^([A-Za-z0-9_.\-"']+):\s*(.*)$/.exec(line)
    if (!match) continue
    const key = unquote(match[1])
    const value = match[2]

    if (indent === 0) {
      section = key
      inProviderTargets = false
      subsectionIndent = -1
      continue
    }
    if (section === "rule_guard" && key === "model" && value) {
      ruleGuardModel = unquote(value)
      continue
    }
    if (section !== "proxy") continue
    if (key === "provider_targets" && !value) {
      inProviderTargets = true
      subsectionIndent = indent
      continue
    }
    if (inProviderTargets && indent > subsectionIndent && value) providerTargets[key] = unquote(value)
  }

  return { ...(ruleGuardModel === undefined ? {} : { ruleGuardModel }), providerTargets }
}

const FIRST_PARTY_DEFAULTS: Record<string, string> = {
  openai: "https://api.openai.com",
  anthropic: "https://api.anthropic.com",
  google: "https://generativelanguage.googleapis.com",
  groq: "https://api.groq.com",
  mistral: "https://api.mistral.ai",
}

const OPENAI_COMPAT_NPMS = new Set(["@ai-sdk/openai-compatible", "@ai-sdk/openai", "@ai-sdk/mistral"])

/**
 * Endpoints that accept the DeepSeek-style `thinking` switch. The list is explicit because the
 * parameter is not part of the openai-compatible surface: an endpoint that does not know it rejects
 * the whole request (the litellm gateway answered 400 UnsupportedParamsError), and that rejection
 * used to be read as a PASS (YM-410). It cannot be derived from the provider's npm either, which is
 * the same for DeepSeek and for a gateway in front of it.
 */
const THINKING_CAPABLE_HOSTS = new Set(["api.deepseek.com", "api.z.ai"])

export function supportsThinkingDisable(apiUrl: string): boolean {
  try {
    return THINKING_CAPABLE_HOSTS.has(new URL(apiUrl).host)
  } catch {
    return false
  }
}

/**
 * Statuses that mean the endpoint refused this request, not that it was busy: a bad parameter, a
 * credential, a route, a validation. They do not fix themselves while a session runs, so the guard
 * cannot pretend it judged the call.
 */
const REQUEST_REJECTED_STATUSES = new Set([400, 401, 403, 404, 422])

async function readJson(file: string): Promise<Record<string, any>> {
  const text = await readText(file)
  if (text === undefined) return {}
  try {
    return JSON.parse(text)
  } catch {
    return {}
  }
}

export async function resolveGuardConfig(): Promise<{ model: string; apiUrl: string; apiKey: string; npm: string }> {
  // Explicit overrides win: without them the guard can only be pointed at the user's real provider.
  const overrideUrl = process.env.YESMEM_GUARD_URL
  if (overrideUrl) {
    return {
      model: process.env.YESMEM_GUARD_MODEL || "deepseek-v4-flash",
      apiUrl: overrideUrl.replace(/\/+$/, ""),
      apiKey: process.env.YESMEM_GUARD_KEY || "",
      npm: process.env.YESMEM_GUARD_NPM || "",
    }
  }

  const home = process.env.HOME || homedir()
  const config = parseGuardConfig((await readText(`${home}/.claude/yesmem/config.yaml`)) ?? "")
  // Not extraction.model: that one may be a reasoning model, and its empty answers made the guard pass
  // every call without judging it (YM-78).
  const model = config.ruleGuardModel || "deepseek-v4-flash"
  const providerTargets = config.providerTargets

  const models = await readJson(`${home}/.cache/opencode/models.json`)
  const auth = await readJson(`${home}/.local/share/opencode/auth.json`)

  let apiUrl = "https://api.deepseek.com"
  let apiKey = ""
  let npm = ""
  let fallbackApiUrl = ""
  let fallbackApiKey = ""

  for (const [providerId, provider] of Object.entries(models)) {
    const entry = provider as any
    if (!entry?.models?.[model]) continue
    const key = auth[providerId]?.key || ""
    const url = entry.api || FIRST_PARTY_DEFAULTS[providerId] || ""
    if (key) {
      apiUrl = url
      apiKey = key
      npm = entry.npm || ""
      break
    }
    if (!fallbackApiKey) {
      fallbackApiUrl = url
      fallbackApiKey = key
    }
  }

  if (!apiKey && fallbackApiUrl) {
    apiUrl = fallbackApiUrl
    apiKey = fallbackApiKey
  }

  // Private/gateway models (e.g. privateTom) are usually not in models.json: they are served by a custom
  // upstream from config.yaml and authenticated with the opencode "gateway" auth key.
  if (!apiKey) {
    let target = providerTargets[model] || ""
    if (!target) {
      const flat = Object.keys(providerTargets).find((key) => key.toLowerCase() === model.toLowerCase())
      target = flat ? providerTargets[flat] : ""
    }
    if (target) {
      apiUrl = target.replace(/\/+$/, "")
      apiKey = auth.gateway?.key || ""
      npm = "@ai-sdk/openai-compatible"
    }
  }

  return { model, apiUrl, apiKey, npm }
}

// ---------------------------------------------------------------------------------------------
// Guard decision

/**
 * The host of an endpoint for messages and log lines. An endpoint the guard cannot parse is named as such
 * rather than echoed: this text reaches a tool result and the archive, and a configured value may carry
 * credentials in its userinfo.
 */
function hostOf(apiUrl: string): string {
  try {
    return new URL(apiUrl).host
  } catch {
    return "(unparseable endpoint)"
  }
}

/**
 * Logs model and endpoint host once per change. The host, never the key: the guard's answer changes with
 * both, and a silent switch between providers is otherwise invisible in the log.
 */
let lastResolution = ""
function logResolution(config: { model: string; apiUrl: string; npm: string }) {
  const host = hostOf(config.apiUrl)
  const resolution = `${config.model} via ${host}`
  if (resolution === lastResolution) return
  lastResolution = resolution
  debugLog("guard", `RESOLVED model=${config.model} host=${host} npm=${config.npm || "-"}`)
}

async function guardCheck(
  rules: string,
  tool: string,
  context: string,
  canBlock: boolean,
  retried = false,
  optionalLevel = 2,
): Promise<GuardResult> {
  const now = Date.now()
  for (const [key, entry] of guardCache) if (now - entry.ts >= GUARD_CACHE_TTL_MS) guardCache.delete(key)
  const cacheKey = guardCacheKey(rules, tool, context)
  const cached = guardCache.get(cacheKey)
  if (cached) return cached.result

  const config = await resolveGuardConfig()
  if (!config.apiKey || !config.apiUrl) {
    debugLog("guard", `NO-KEY url=${config.apiUrl} key=${config.apiKey ? "yes" : "no"}`)
    return { decision: "PASS" }
  }
  // The endpoint and the credential change with the model. Say it once per resolution, host only, so a
  // model switch (rule_guard.model) is visible in the log instead of being inferred from errors.
  logResolution(config)

  const refusalKey = `${config.apiUrl}\u0000${config.model}`
  if (lastRefusal && lastRefusal.key === refusalKey && now - lastRefusal.ts < REFUSAL_RETRY_MS) {
    return lastRefusal.result
  }

  const blockOption = canBlock
    ? `- BLOCK: call violates a rule → {"decision":"BLOCK","violations":["Rule X: reason"]}\n`
    : ""
  const systemPrompt =
    `Output ONLY this JSON format. Do NOT reason, explain, or think. Just output the JSON.\n\n` +
    `RULES:\n${rules}\n\n` +
    `Formats:\n` +
    blockOption +
    `- {"decision":"SUGGEST","suggestion":"skill: short reason max 60 chars"}\n` +
    `- {"decision":"PASS"}\n\n` +
    `For SUGGEST: use exact skill name from rules. Evaluate all Skill Catalog entries against context.`
  const userPrompt = `PROPOSED TOOL CALL: ${tool}\nCONTEXT: ${context}`

  try {
    const started = Date.now()
    const body: Record<string, any> = {
      model: config.model,
      messages: [
        { role: "system", content: systemPrompt },
        { role: "user", content: userPrompt },
      ],
      temperature: 0,
      max_tokens: 4096,
    }
    // The optional fields are an optimisation, the judgement is not. optionalLevel 2 sends both, 1 sends
    // only the JSON answer format, 0 sends neither. An endpoint that refuses one of them gets the next
    // level before the guard declares itself unavailable.
    if (OPENAI_COMPAT_NPMS.has(config.npm) && optionalLevel >= 1) {
      body.response_format = { type: "json_object" }
    }
    // Only where the parameter is known. An endpoint that does not know it rejects the whole
    // request instead of ignoring the field (YM-410).
    if (OPENAI_COMPAT_NPMS.has(config.npm) && optionalLevel >= 2 && supportsThinkingDisable(config.apiUrl)) {
      body.thinking = { type: "disabled" }
    }

    const response = await fetch(`${config.apiUrl}/v1/chat/completions`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Authorization: `Bearer ${config.apiKey}` },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(15000),
      // The endpoint may be redirected by an override URL; a redirect would replay the prompt elsewhere.
      redirect: "error",
    })
    const raw = await readBodyCapped(response, MAX_GUARD_RESPONSE_CHARS)
    const elapsed = Date.now() - started
    // Every abnormal answer is logged with status, model and body excerpt. Without that the guard passed
    // silently for days and the cause was unreadable (YM-78).
    if (!response.ok) {
      debugLog("guard", `[${elapsed}ms] HTTP-ERROR status=${response.status} model=${config.model} body=${logExcerpt(raw)}`)
      // The optional request fields are an optimisation, the judgement is not. One step down the ladder
      // before the guard declares itself unavailable: otherwise a gateway that refuses response_format
      // locks every write, which is the same trap as the thinking parameter one layer down (YM-410).
      if (response.status === 400 && optionalLevel > 0 && OPENAI_COMPAT_NPMS.has(config.npm)) {
        debugLog("guard", `[${elapsed}ms] RETRY-WITHOUT-OPTIONAL status=400 level=${optionalLevel} model=${config.model}`)
        return guardCheck(rules, tool, context, canBlock, retried, optionalLevel - 1)
      }
      // A refused request is not an answered one. Passing it on is what let the guard judge nothing for
      // days: 19,000 PASS lines with an HTTP 400 behind every one of them (YM-410).
      if (REQUEST_REJECTED_STATUSES.has(response.status)) {
        const refusal: GuardResult = {
          decision: "UNAVAILABLE",
          detail: `HTTP ${response.status} from ${hostOf(config.apiUrl)}`,
        }
        lastRefusal = { key: refusalKey, result: refusal, ts: Date.now() }
        return refusal
      }
      return { decision: "PASS" }
    }
    lastRefusal = null
    let payload: any
    try {
      payload = JSON.parse(raw)
    } catch {
      debugLog("guard", `[${elapsed}ms] NON-JSON status=${response.status} model=${config.model} body=${logExcerpt(raw)}`)
      return { decision: "PASS" }
    }
    const message = payload?.choices?.[0]?.message
    const content: string = message?.content || ""

    if (!content) {
      debugLog(
        "guard",
        `[${elapsed}ms] EMPTY-CONTENT status=${response.status} model=${config.model} finish=${message?.finish_reason ?? "?"} body=${logExcerpt(raw)}`,
      )
      const result: GuardResult = { decision: "PASS" }
      guardCache.set(cacheKey, { result, ts: Date.now() })
      return result
    }

    const parsed = (() => {
      try {
        return JSON.parse(content) as GuardResult
      } catch {
        return undefined
      }
    })()
    if (!parsed) {
      // Prose instead of JSON is the same class as an empty answer: the guard did not judge, and the reply
      // says why. Retrying costs a second request and hides the reason.
      debugLog(
        "guard",
        `[${elapsed}ms] NON-JSON-ANSWER status=${response.status} model=${config.model} content=${logExcerpt(content)}`,
      )
      return { decision: "PASS" }
    }
    const decision = normalizeDecision(parsed)
    guardCache.set(cacheKey, { result: decision, ts: Date.now() })
    if (decision.decision === "BLOCK") {
      debugLog(
        "guard",
        `[${elapsed}ms] ${canBlock ? "BLOCKED" : "BLOCK-IGNORED (tool cannot be blocked)"} ${logExcerpt((decision.violations ?? []).join("; "))}`,
      )
      return decision
    }
    if (decision.decision === "SUGGEST") {
      debugLog("guard", `[${elapsed}ms] SUGGEST ${logExcerpt(decision.suggestion ?? "")}`)
      return decision
    }
    debugLog("guard", `[${elapsed}ms] PASS`)
    return decision
  } catch (error: any) {
    const aborted = error?.name === "AbortError" || error?.name === "TimeoutError"
    // A timeout is not retried: the caller has already waited the full budget.
    if (!retried && !aborted) {
      await new Promise((resolve) => setTimeout(resolve, 500))
      return guardCheck(rules, tool, context, canBlock, true, optionalLevel)
    }
    debugLog("guard", `check failed: ${error?.message || String(error)}`)
    return { decision: "PASS" }
  }
}

/**
 * One line for text a model produced. The prompt embeds RULES.md, so a violation string can carry text an
 * attacker steered; collapsing whitespace and control characters keeps it from forging structure (a second
 * "RULE:" block, a fake system line) in the tool error the agent reads. It is not a boundary against a
 * hostile repository, which can answer PASS just as well.
 */
function guardMessage(text: string): string {
  const collapsed = text
    .replace(/[\u0000-\u001f\u007f-\u009f\u2028\u2029]/g, " ")
    .replace(/\s+/g, " ")
    .trim()
  return collapsed.length > MAX_VIOLATION_CHARS ? collapsed.substring(0, MAX_VIOLATION_CHARS) + "…" : collapsed
}

/**
 * The decisions the guard can make on its own. A model answer outside this set counts as PASS (the call
 * was not judged) and never reaches the cache: the prompt embeds RULES.md, so the answer is untrusted
 * input, and a fourth decision name invented there must not turn into a rejection or a notice text.
 */
function normalizeDecision(parsed: GuardResult): GuardResult {
  if (parsed.decision === "BLOCK" && parsed.violations?.length) {
    return { decision: "BLOCK", violations: parsed.violations }
  }
  if (parsed.decision === "SUGGEST" && parsed.suggestion) {
    return { decision: "SUGGEST", suggestion: parsed.suggestion }
  }
  return { decision: "PASS" }
}

/**
 * Rule numbers a BLOCK may cite: only rules the loaded catalogue actually contains. Without this filter a
 * model-invented rule number blocks a call (the audit saw "Rule 42", which does not exist). A citation may
 * read "Rule 2", "Rule #2" or "Rules 2 and 4"; everything else in the text is ignored.
 */
export function existingCitedRules(rules: string, violations: string[]): number[] {
  const known = new Set<number>()
  // Only the numbered catalogue counts as an existing rule: the Decision Policy block is prose and may be
  // written as a numbered list, and its numbers must not become citations (YM-456).
  for (const line of rules.split("## Decision Policy")[0].split("\n")) {
    const match = /^(\d+)\.\s/.exec(line.trim())
    if (match) known.add(Number(match[1]))
  }
  const cited = new Set<number>()
  for (const violation of violations) {
    for (const number of citedRuleNumbers(violation)) if (known.has(number)) cited.add(number)
  }
  return [...cited]
}

/** The numbers a violation cites: right after "rule"/"rules" (optionally "#") and in a following list. */
function citedRuleNumbers(violation: string): number[] {
  const numbers: number[] = []
  for (const segment of violation.split(/\brule(?:s)?\b/i).slice(1)) {
    // The separator group carries its own leading `\s*` so it never competes with a `\s*` outside it: two
    // ways to split one whitespace run is quadratic backtracking, and this text comes from a model (YM-456
    // security review).
    const head = /^[\s#]*(\d+(?:(?:\s*,\s*|\s+and\s+|\s*&\s*)#?\d+)*)/.exec(segment)
    if (!head) continue
    for (const number of head[1].match(/\d+/g) ?? []) numbers.push(Number(number))
  }
  return numbers
}

/**
 * The rule numbers the loaded policy lets the guard BLOCK on. Undefined when the file carries no Decision
 * Policy or no BLOCK sentence: then nothing narrows the set and an existing citation still blocks, which is
 * the behaviour before the policy was read at all. Guessing a set here would weaken the guard (YM-456).
 */
export function blockingRuleNumbers(rules: string): Set<number> | undefined {
  const policy = /## Decision Policy[^\n]*\n([\s\S]*?)(?=\n## |$)/.exec(rules)
  if (!policy) return undefined
  const numbers = new Set<number>()
  // Every BLOCK bullet counts, and a bullet may wrap: with the `m` flag a bare `$` would already match at
  // the first line break and silently drop the rules on the second line. The separator group keeps its own
  // leading `\s*` for the same reason as above: RULES.md is repository content and a whitespace run must
  // not backtrack (YM-456 security review).
  for (const block of policy[1].matchAll(/^[-*]\s+\*\*block:?\*\*([\s\S]*?)(?=\n[-*]\s|(?![\s\S]))/gim)) {
    for (const match of block[1].matchAll(/\brules?\s+(\d+(?:(?:\s*,\s*|\s+and\s+|\s*&\s*)#?\d+)*)/gi)) {
      for (const number of match[1].match(/\d+/g) ?? []) numbers.add(Number(number))
    }
  }
  return numbers.size > 0 ? numbers : undefined
}

function detectUserIntent(message: string, tool: string, args: Record<string, any>): boolean {
  if (!message) return false
  const lower = message.toLowerCase()
  const command = (args?.command || "").toLowerCase()
  for (const verb of ["commit", "push", "deploy", "do it", "mach ", "tu es", "mach es", "fire and forget"]) {
    if (!lower.includes(verb)) continue
    if (tool === "shell" && command && lower.includes(command)) return true
    if (tool !== "shell" && verb.length > 3) return true
  }
  return false
}

// ---------------------------------------------------------------------------------------------
// Hooks

export interface ToolCallEvent {
  readonly tool?: string
  readonly sessionID?: string
  readonly id?: string
  input?: unknown
}

export interface ToolResultEvent {
  readonly tool?: string
  readonly id?: string
  readonly status?: string
  result?: any
  error?: any
}

export function ruleGuard(directory: string) {
  let rulesCache = ""
  let rulesLoaded = false
  let gitBranch = ""
  let recentConversation: string[] = []
  const suggestions = new Map<string, { suggestion: string; level: string }>()
  // Calls the guard could not judge: the reason travels with the tool result, because a tool that
  // cannot be blocked would otherwise pass without anyone noticing the guard was down (YM-410).
  const notices = new Map<string, string>()

  /** Read-only capture of the recent conversation for the guard context. */
  function capture(messages: ReadonlyArray<any>) {
    try {
      const collected: string[] = []
      for (let i = messages.length - 1; i >= 0 && collected.length < 6; i--) {
        const message = messages[i]
        const role = message?.role
        if (role !== "user" && role !== "assistant") continue
        const parts = message?.content
        if (!Array.isArray(parts)) continue
        const text = parts
          .filter((part: any) => part?.type === "text" && part.text)
          .map((part: any) => part.text)
          .join("\n")
        if (text) collected.unshift(`[${role}]: ${text.substring(0, 500)}`)
      }
      if (collected.length > 0) recentConversation = collected
    } catch {}
  }

  async function before(event: ToolCallEvent): Promise<string | undefined> {
    try {
      if (!rulesLoaded) {
        rulesCache = await loadRules(directory)
        gitBranch = await getGitBranch(directory)
        rulesLoaded = true
        debugLog(
          "guard",
          `loaded ${rulesCache.split("\n").length} rules ~${Math.round(rulesCache.length / 4)} sys-tok branch=${gitBranch || "?"}`,
        )
        // "nothing to judge" must be distinguishable from "the guard is broken" (YM-78).
        if (!rulesCache || rulesCache.startsWith("No ")) debugLog("guard", `SKIP-NORULES ${rulesCache}`)
        else {
          // A policy that does not parse degrades to the pre-YM-456 behaviour, so say which case holds.
          const blockable = blockingRuleNumbers(rulesCache)
          debugLog(
            "guard",
            blockable
              ? `policy BLOCK set ${[...blockable].sort((a, b) => a - b).join(",")}`
              : "policy absent or unparseable: an existing citation may block",
          )
        }
      }
      if (!rulesCache || rulesCache.startsWith("No ")) return

      const toolName = (event.tool || "").toString().toLowerCase()
      const args = (event.input ?? {}) as Record<string, any>
      if (skipTools.has(toolName)) return
      if (!suggestableTools.has(toolName)) return

      // Checked before the file-target skips below, because a shell call has no file target: the command
      // itself is the whole evidence.
      if (toolName === "shell" && isReadOnlyShell(args?.command)) {
        debugLog("guard", `SKIP-READONLY shell cmd=${logExcerpt(String(args?.command))}`)
        return
      }

      // A shell call has no file target: its arguments are a command, so a `path`-shaped field it happens
      // to carry must not excuse the call from the guard (the doc exemption and the two filename skips
      // below all key on this value).
      const filePath = toolName === "shell" ? "" : args?.filePath || args?.file_path || args?.path || ""
      if (filePath && (filePath.endsWith("RULES.md") || filePath.endsWith("rule_guard.ts"))) return

      const docExtensions = /\.(md|txt|rst|pdf)(\*|$|,)/
      const isDocTarget =
        docExtensions.test(filePath) ||
        (toolName === "grep" && docExtensions.test(args?.include || "")) ||
        (toolName === "glob" && docExtensions.test(args?.pattern || ""))
      if (isDocTarget) {
        debugLog("guard", `SKIP-DOC tool=${toolName} filePath=${filePath}`)
        return
      }

      // The excerpt is capped, and for a shell call two fields carry the decision: the command, and the
      // working directory that rule 25 turns on ("outside scratch/temp dirs"). A long `description` ahead
      // of them in the argument object would push them out of the cap entirely, leaving the guard to judge
      // a shell call it cannot see. Both are rebuilt to the head, so the command always arrives; workdir
      // follows it and stays whole only as far as the remaining budget reaches. The other fields stay.
      const argText =
        toolName === "shell" && typeof args?.command === "string"
          ? JSON.stringify({ command: args.command, workdir: args.workdir, ...args })
          : JSON.stringify(args)
      let context = `Tool: ${toolName}, Args: ${argText.substring(0, 300)}`
      if (recentConversation.length > 0) {
        context += `\nRecent conversation:\n${recentConversation.join("\n")}`
        const lastUser = recentConversation.filter((line) => line.startsWith("[user]:")).pop() || ""
        if (detectUserIntent(lastUser, toolName, args)) {
          context +=
            `\nNOTE: The user explicitly requested this action. Rules about user-decision (auto-commit, deployment, destructive ops) should yield to explicit user instruction.`
        }
      }
      if (filePath && filePath.endsWith("_test.go")) {
        context += `\nNOTE: This is a test file ("_test.go"). Writing test files before implementation IS TDD-compliant — do not BLOCK under TDD/test-first rules.`
      }
      if (gitBranch) {
        context += featureBranchNote(gitBranch)
      }

      const canBlock = blockableTools.has(toolName)
      const result = await guardCheck(rulesCache, toolName, context, canBlock)
      if (result.decision === "UNAVAILABLE") {
        const detail = result.detail || "no answer"
        if (!failClosedTools.has(toolName)) {
          if (event.id) notices.set(event.id, detail)
          return
        }
        return `Rule check unavailable (${detail}): this call was not judged.`
      }
      if (result.decision === "BLOCK" && canBlock) {
        const violations = result.violations || []
        // A model-invented rule number may not block a call; only a rule the catalogue holds counts.
        const cited = existingCitedRules(rulesCache, violations)
        if (cited.length === 0) {
          debugLog("guard", `DOWNGRADE no existing rule cited: ${logExcerpt(violations.join("; "))}`)
          return
        }
        // A rule the policy classes as advisory is suggested, not enforced: the model answered BLOCK on a
        // rule that only asks for a check, and the guard saw exactly one such case in the live log (rule 14
        // and 15, both advisory) against 675 suggestions (YM-456).
        const blockable = blockingRuleNumbers(rulesCache)
        if (blockable && !cited.some((number) => blockable.has(number))) {
          debugLog(
            "guard",
            `DOWNGRADE advisory rule(s) ${cited.join(",")} outside the BLOCK set: ${logExcerpt(violations.join("; "))}`,
          )
          if (event.id) suggestions.set(event.id, { suggestion: violations.join("; "), level: "ADVISORY" })
          return
        }
        // The text comes from a model whose prompt embeds repository content; it reaches the model again.
        return `Rule check blocked this call: ${guardMessage(violations.join("; "))}`
      }
      if (result.decision === "SUGGEST") {
        const suggestion = result.suggestion || ""
        // The policy prescribes "<skill-name>: <reason>" (RULES.md), and the live log showed every answer in
        // that shape dropped as a ghost skill: the check compared the whole string with a directory name
        // (733 SUGGEST, 733 SKIP ghost skill, none delivered). The prefix decides whether the skill exists;
        // the reason stays in the message.
        const skillName = suggestion.split(":", 1)[0].trim()
        if (suggestion && !isSkillInstalled(suggestion) && !isSkillInstalled(skillName)) {
          debugLog("guard", `SKIP ghost skill: ${suggestion}`)
          return
        }
        if (event.id) {
          suggestions.set(event.id, { suggestion, level: toolName === "shell" ? "MANDATORY" : "REQUIRED" })
        }
      }
      return
    } catch (error: any) {
      debugLog("guard", `CRASH ${error?.message || String(error)}`)
      return
    }
  }

  /** Prefixes the tool result (or its error) with a guard message. */
  function injectPrefix(event: ToolResultEvent, prefix: string) {
    if (event.status === "error" && event.error) {
      event.error = new ToolError({
        message: prefix + (event.error.message || ""),
        ...(event.error.error === undefined ? {} : { error: event.error.error }),
        ...(event.error.metadata === undefined ? {} : { metadata: event.error.metadata }),
      })
      return
    }
    if (!event.result) return
    const content = event.result.content
    if (Array.isArray(content)) event.result.content = [{ type: "text", text: prefix }, ...content]
    else event.result.content = prefix + (typeof content === "string" ? content : "")
  }

  /** Injects a pending guard message into the tool result (or its error). */
  function after(event: ToolResultEvent) {
    try {
      if (!event.id) return
      const notice = notices.get(event.id)
      if (notice) {
        notices.delete(event.id)
        injectPrefix(
          event,
          `[guard] RULE GUARD UNAVAILABLE: ${notice}. This call was not checked against RULES.md.\n\n`,
        )
      }
      const entry = suggestions.get(event.id)
      if (!entry) return
      suggestions.delete(event.id)
      // The suggestion is model output, and every branch below puts it in the tool result: collapse and cap
      // it, or a repository-steered answer forges structure in the agent's own context (YM-456 review).
      const text = guardMessage(entry.suggestion)
      const prefix =
        entry.level === "MANDATORY"
          ? `[guard] ⚠️ MANDATORY CHECK — activate ${text} (use Skill tool)\n\n`
          : entry.level === "ADVISORY"
            ? // A downgraded BLOCK: the policy reserves the rejection for other rules, so the call stands
              // and the model is told what to verify (YM-456).
              `[guard] SUGGEST — rule check did not block: ${text}\n\n`
            : `[guard] ACTION REQUIRED: activate ${text} — use Skill tool now.\n\n`

      injectPrefix(event, prefix)
    } catch (error: any) {
      debugLog("guard", `AFTER-CRASH ${error?.message || String(error)}`)
    }
  }

  return { before, after, capture }
}
