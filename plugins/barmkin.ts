// Barmkin plugin for opencode.
//
// Spawns `barmkin eval` as a subprocess on every tool.execute.before event.
// The binary reads an AdapterRequest from stdin and exits:
//   exit 0 = allow (silent)
//   exit 2 = deny (stderr contains the reason)
//
// The plugin parses the exit code. On deny it throws an Error with the
// reason, which opencode shows to the LLM as a tool failure. On allow
// it returns silently.
//
// All telemetry (OTel, audit log, stats) is handled by the Go binary.
// The plugin is intentionally thin - just normalize + spawn + check exit code.
//
// Environment variables:
//   BARMKIN_BIN       binary path            (default "barmkin")
//   BARMKIN_TITLE     toast title            (default "Barmkin")
//   BARMKIN_TOAST     set "1" to enable toast notifications
//   BARMKIN_DRY_RUN   set "1" to log denials without blocking
//   BARMKIN_OFF       set "1" to disable plugin entirely

import type { Plugin } from "@opencode-ai/plugin"

// ── Configuration ────────────────────────────────────────────────────────

const BIN = process.env.BARMKIN_BIN || "barmkin"
const DRY_RUN = process.env.BARMKIN_DRY_RUN === "1"
const DISABLED = process.env.BARMKIN_OFF === "1"
const TOAST = process.env.BARMKIN_TOAST === "1"
const TITLE = `● ${process.env.BARMKIN_TITLE || "Barmkin"}`
const AGENT_ID = `opencode-${process.env.USER || "unknown"}`

// ── Toast ────────────────────────────────────────────────────────────────
// Optional - disabled by default. Uses the opencode SDK client's
// tui.showToast() method. Only fires when BARMKIN_TOAST=1.

let client: any = null

async function toast(variant: "info" | "warning" | "error", message: string, duration = 8000) {
  if (!TOAST || !client?.tui) return
  try {
    await client.tui.showToast({ body: { title: TITLE, message, variant, duration } })
  } catch {}
}

// ── Normalization ────────────────────────────────────────────────────────
// Maps opencode tool names and args to the AdapterRequest format
// that barmkin expects. Mirrors sondera's normalize.ts logic.

const TOOL_ACTION: Record<string, string> = {
  bash: "ShellCommand", read: "FileRead", edit: "FileEdit",
  write: "FileWrite", apply_patch: "FileEdit", glob: "FileSearch",
  grep: "ContentSearch", webfetch: "WebFetch", websearch: "WebSearch",
}

// Extract a string from an unknown value, returning undefined if empty.
function str(v: unknown): string | undefined {
  return typeof v === "string" && v.length > 0 ? v : undefined
}

// Convert raw opencode tool args into the normalized args dict
// that AdapterRequest expects (command for bash, path for files, etc).
function toolArgs(tool: string, a: Record<string, unknown>): Record<string, unknown> {
  switch (tool) {
    case "bash": return { command: a.command || "", workdir: str(a.workdir) }
    case "read": return { path: str(a.filePath) || str(a.path) || "" }
    case "edit": return { path: str(a.filePath) || str(a.path) || "" }
    case "write": return { path: str(a.filePath) || str(a.path) || "" }
    default: return a
  }
}

// ── Evaluate ─────────────────────────────────────────────────────────────
// Spawn `barmkin eval`, pipe the AdapterRequest JSON to stdin,
// and check the exit code.

async function evaluate(
  tool: string,
  args: Record<string, unknown>,
  cwd: string,
  sid: string,
): Promise<string | null> {
  const req = JSON.stringify({
    trajectory_id: sid || "unknown",
    agent_id: AGENT_ID,
    tool,
    action: TOOL_ACTION[tool] || "ToolCall",
    args: toolArgs(tool, args),
    cwd,
    event_type: "before",
  })

  try {
    const proc = Bun.spawn({
      cmd: [BIN, "eval"],
      stdin: "pipe",
      stdout: "pipe",
      stderr: "pipe",
    })
    proc.stdin.write(req)
    proc.stdin.end()

    const code = await proc.exited

    // Exit 2 = deny. Read stderr for the reason.
    if (code === 2) {
      const stderr = await Bun.readableStreamToText(proc.stderr)
      return stderr.trim() || "denied"
    }
    // Exit 0 = allow. Silent.
    return null
  } catch {
    // Spawn failure - fail open so a broken guardrail doesn't block work.
    return null
  }
}

// ── Plugin ───────────────────────────────────────────────────────────────

export const BarmkinPlugin: Plugin = async (ctx: any) => {
  if (DISABLED) return {}

  // Store the SDK client for toast notifications.
  client = ctx.client

  // Fire startup toast if toasts are enabled.
  toast("info", DRY_RUN ? "dry-run mode" : "enforcing")

  return {
    // Intercept every tool call before execution.
    "tool.execute.before": async (input: any, output: any) => {
      if (!output?.args || typeof output.args !== "object") return

      const reason = await evaluate(
        input.tool,
        output.args as Record<string, unknown>,
        ctx.directory,
        input.sessionID ?? "",
      )

      // null = allowed or daemon unavailable - fail open.
      if (!reason) return

      // Dry-run mode: warn but don't block.
      if (DRY_RUN) {
        await toast("warning", `[dry-run] would deny: ${reason}`)
        return
      }

      // Deny: toast (if enabled) + throw error for the LLM to see.
      await toast("error", reason)
      throw new Error(reason)
    },
  }
}
