# CCMonet Bot — Technical Architecture Report

**Document Version:** 1.0
**Date:** 2026-02-27
**Project:** `github.com/PlatoX-Type/monet-bot` v0.1.0
**Language:** Go 1.23
**Classification:** Internal Engineering Document

---

## 1. Executive Summary

CCMonet Bot is an internal team AI agent built in Go. It provides a persistent, tool-augmented conversational interface that connects team members — via Lark or a local CLI — to large language models capable of executing real-world actions: reading and writing code, running shell commands, querying internal APIs, searching the web, scheduling tasks, and spawning background sub-agents.

**Problems it solves:**

- **Context fragmentation.** Team knowledge is scattered across Lark chats, code repos, wikis, and individual memory. CCMonet maintains a persistent, LLM-consolidated memory (`MEMORY.md`) and searchable history log (`HISTORY.md`) across all conversations.
- **Repetitive operational tasks.** The bot can read codebases, run shell commands, query APIs, and generate reports autonomously through its ReAct tool-use loop, eliminating manual grunt work.
- **Communication overhead.** By integrating directly into Lark, the bot participates in group chats as a team member — answering questions, performing lookups, and running background tasks without context-switching to separate tools.
- **Scheduling and monitoring gaps.** Built-in cron scheduling and an LLM-powered heartbeat service allow the bot to proactively execute periodic tasks (standups, health checks, reminders) without human triggering.

The system is designed with minimal dependencies (only 3 external Go modules), clean interface boundaries, and a message-bus architecture that makes adding new channels, tools, or providers straightforward.

---

## 2. Architecture Overview

### 2.1 High-Level Architecture Diagram

```
┌──────────────────────────────────────────────────────────────────────┐
│                        CCMonet Bot Process                           │
│                                                                      │
│  ┌──────────────────┐     ┌──────────────────┐                       │
│  │   CLI Channel     │     │   Lark Channel    │                      │
│  │   (stdin/stdout)  │     │   (HTTP :9000)    │                      │
│  └────────┬─────────┘     └────────┬─────────┘                       │
│           │                        │                                  │
│           ▼                        ▼                                  │
│  ┌────────────────────────────────────────────┐                       │
│  │          Message Bus (bus.MessageBus)       │                      │
│  │    Inbound chan ◄───  ───► Outbound chan    │                      │
│  └────────┬───────────────────┬───────────────┘                       │
│           │                   │                                       │
│           ▼                   ▼                                       │
│  ┌────────────────────┐  ┌────────────────────┐                       │
│  │    Agent Loop       │  │ Channel Manager    │                      │
│  │  (agent.Loop)       │  │ (routes outbound)  │                      │
│  │                     │  └────────────────────┘                       │
│  │ ┌────────────────┐  │                                              │
│  │ │ System Prompt   │  │  ┌──────────────────┐                       │
│  │ │ Builder         │  │  │ Subagent Manager │                       │
│  │ ├────────────────┤  │  │ (background tasks)│                       │
│  │ │ Session Mgr     │  │  └──────────────────┘                       │
│  │ │ (JSONL files)   │  │                                              │
│  │ ├────────────────┤  │  ┌──────────────────┐                       │
│  │ │ Memory System   │  │  │ Cron Service      │                      │
│  │ │ (MEMORY/HISTORY)│  │  │ (robfig/cron)     │                      │
│  │ ├────────────────┤  │  └──────────────────┘                       │
│  │ │ Skills Loader   │  │                                              │
│  │ └────────────────┘  │  ┌──────────────────┐                       │
│  └────────┬────────────┘  │ Heartbeat Service │                       │
│           │                │ (LLM-powered)     │                      │
│           ▼                └──────────────────┘                       │
│  ┌────────────────────┐                                               │
│  │   LLM Provider     │  ┌──────────────────┐                        │
│  │  (12+ providers)   │  │ Repo Manager      │                       │
│  └────────┬───────────┘  │ (git clone/pull)  │                       │
│           │               └──────────────────┘                       │
│           ▼                                                           │
│  ┌────────────────────┐                                               │
│  │   Tool Registry     │                                              │
│  │                     │                                              │
│  │ read_file  exec     │                                              │
│  │ write_file web_fetch│                                              │
│  │ edit_file  message  │                                              │
│  │ list_dir   spawn    │                                              │
│  │ web_search cron     │                                              │
│  │ query_api  mcp_*    │                                              │
│  └─────────────────────┘                                              │
└──────────────────────────────────────────────────────────────────────┘
```

### 2.2 Component Breakdown

| Component | Package | Responsibility |
|---|---|---|
| Entry Point | `cmd/root.go` | CLI parsing via Cobra, wiring all components |
| Agent Loop | `agent/loop.go` | ReAct reasoning loop, command handling, cancellation |
| System Prompt | `agent/context.go` | Assembles SOUL.md + bootstrap files + workspace info + memory + skills |
| Memory | `agent/memory.go` | LLM-powered consolidation to MEMORY.md and HISTORY.md |
| Sessions | `agent/session.go` | Per-chat JSONL persistence with consolidation pointer |
| Skills | `agent/skills.go` | Markdown-based skill loading with frontmatter metadata |
| LLM Provider | `providers/provider.go` | Multi-provider API client with auto-detection and prompt caching |
| Message Bus | `bus/bus.go` | Buffered Inbound/Outbound channels decoupling all components |
| Tool Registry | `tools/registry.go` | Tool registration, schema generation, execution with error hints |
| Filesystem Tools | `tools/filesystem.go` | read_file, write_file, edit_file, list_dir with workspace sandboxing |
| Shell Tool | `tools/shell.go` | exec with deny-pattern safety and workspace confinement |
| Web Tools | `tools/web.go` | web_search (Brave API), web_fetch (HTTP GET) |
| Message Tool | `tools/message.go` | Send messages to any channel/chat, with media attachments |
| Spawn Tool | `tools/spawn.go` | Launch background sub-agents with their own ReAct loops |
| Cron Tool | `tools/cron.go` | Dynamic job scheduling at runtime (add/list/remove) |
| MCP Integration | `tools/mcp.go` | Connects to MCP servers (stdio or HTTP), wraps as native tools |
| Cron Service | `cron/service.go` | robfig/cron scheduler with JSON persistence for dynamic jobs |
| Heartbeat | `heartbeat/service.go` | Periodic LLM-powered decision engine for autonomous tasks |
| Lark Channel | `channels/lark.go` | Lark webhook receiver, card rendering, image handling |
| CLI Channel | `channels/cli.go` | Interactive stdin/stdout interface for local development |
| Repo Manager | `repos/manager.go` | Git clone, pull, and periodic sync for code repositories |
| Config | `config/config.go` | JSON configuration with sensible defaults |

### 2.3 Technology Stack

| Layer | Technology |
|---|---|
| Language | Go 1.23 |
| CLI Framework | `github.com/spf13/cobra` v1.8.1 |
| Cron Scheduler | `github.com/robfig/cron/v3` v3.0.1 |
| LLM Interface | OpenAI-compatible Chat Completions API (function calling) |
| MCP Protocol | JSON-RPC 2.0 over stdio or HTTP |
| Messaging | Lark Open API (webhooks + interactive cards) |
| Persistence | Filesystem (JSONL sessions, Markdown memory, JSON cron jobs) |
| Web Search | Brave Search API |

### 2.4 Message Bus Pattern

The `bus.MessageBus` is the central nervous system. It consists of two buffered Go channels (capacity 100 each):

```go
type MessageBus struct {
    Inbound  chan InboundMessage   // channels/cron/heartbeat/subagents → Agent
    Outbound chan OutboundMessage  // Agent → Channel Manager → Channels
}
```

Every component communicates exclusively through this bus:
- **Channels** push user messages to `Inbound`
- **Cron Service** pushes scheduled task messages to `Inbound`
- **Heartbeat** pushes autonomous task messages to `Inbound`
- **Subagents** push completion announcements to `Inbound` (via `system` channel)
- **Agent Loop** consumes from `Inbound`, processes, pushes responses to `Outbound`
- **Channel Manager** consumes from `Outbound` and routes to the correct channel adapter

---

## 3. How It Works — Message Lifecycle

### 3.1 End-to-End Walkthrough

**Step 1: User Sends Message**
A user sends a message via Lark (by @mentioning the bot or in continue mode) or types into the CLI.

**Step 2: Channel Adapter Receives and Normalizes**
The channel adapter parses the platform-specific payload, extracts text content, downloads any attached images to `workspace/uploads/`, checks ACLs and trigger mode, and constructs a normalized `InboundMessage`:

```go
InboundMessage{
    Channel:   "lark",
    ChatID:    "oc_abc123...",
    User:      "ou_xyz789...",
    Text:      "What changed in the API this week?",
    Images:    []string{"uploads/1709012345_img.png"},
    Timestamp: time.Now(),
}
```

**Step 3: Message Bus Routes to Agent**
The message is pushed to `bus.Inbound`. The agent loop's `Run()` method, running in a dedicated goroutine, receives it.

**Step 4: Pre-processing**
- Commands (`/new`, `/stop`, `/help`, `/memory`, `/skills`) are handled immediately and short-circuit the pipeline.
- Auto-consolidation check: if unconsolidated messages exceed `memory_window`, older messages are consolidated into `MEMORY.md`.
- A cancellable `context.Context` is created for this session, cancelling any previously active task for the same session.
- Tool contexts (spawn, cron, message) are set with the current channel/chatID.

**Step 5: System Prompt Assembly**
`BuildSystemPrompt()` assembles the complete system prompt from multiple sources:

```
┌─────────────────────┐
│      SOUL.md         │  Core personality and instructions
├─────────────────────┤
│     AGENTS.md        │  Multi-agent coordination rules
│      USER.md         │  User/team profiles
│     TOOLS.md         │  Tool usage guidelines
│    IDENTITY.md       │  Identity details
├─────────────────────┤
│   Workspace Info     │  Platform, workspace path, repo list
│   Repo Structure     │  Actual directory tree (depth 2)
├─────────────────────┤
│    Team Memory       │  Current MEMORY.md contents
├─────────────────────┤
│   Skills Context     │  Always-on bodies + on-demand catalog
└─────────────────────┘
```

A separate `BuildRuntimeContext()` injects volatile metadata (current UTC time, channel, chatID) as a user message to avoid cache invalidation of the system prompt.

**Step 6: Session History Loading**
`Sessions.Load()` reads the JSONL session file, skips consolidated messages (using the `last_consolidated` pointer), returns the most recent `memory_window` messages, and trims leading non-user messages to avoid orphaned tool results.

**Step 7: ReAct Loop (Reason → Act → Observe)**

```
FOR each iteration (max 20):
    1. Send messages + tool schemas to LLM Provider
    2. IF response has NO tool calls:
         → Return final text answer (strip <think> tags)
    3. IF response has tool calls:
         → Send progress hint to user ("⚙️ [working] reading file.go")
         → FOR each tool call:
              Execute tool via Registry
              Append tool result to messages
         → Continue loop (next iteration)
    4. Check cancellation (ctx.Err()) at each step
```

**Step 8: Tool Execution**
The `Registry.Execute()` method looks up the tool, calls `Execute(args)`, and appends an error hint suffix to any error results to guide the LLM toward self-correction.

**Step 9: Response Delivery**
The final text response is saved to the session and pushed to `bus.Outbound`. If the `message` tool was already used during this turn, the automatic final send is skipped to avoid duplication.

**Step 10: Channel Sends Response**
The Channel Manager routes the outbound message. Lark renders it as an interactive card with markdown; CLI prints to stdout.

### 3.2 Sequence Diagram

```
User         Lark/CLI      MessageBus     AgentLoop       Provider        Tools
 │               │              │              │              │              │
 │──"message"──►│              │              │              │              │
 │               │──InboundMsg─►│              │              │              │
 │               │              │──consume────►│              │              │
 │               │              │              │              │              │
 │               │              │    [load session history]   │              │
 │               │              │    [build system prompt]    │              │
 │               │              │    [auto-consolidate?]      │              │
 │               │              │              │              │              │
 │               │              │              │───Chat()────►│              │
 │               │              │              │◄──Response───│              │
 │               │              │              │  (tool_calls)│              │
 │               │              │              │              │              │
 │               │◄─"working.."─┼──────────────┤              │              │
 │◄─"⚙️ working"─│              │              │              │              │
 │               │              │              │              │              │
 │               │              │              │──Execute()──────────────►│  │
 │               │              │              │◄──result─────────────────│  │
 │               │              │              │              │              │
 │               │              │              │───Chat()────►│  (w/ tool   │
 │               │              │              │◄──Response───│   results)  │
 │               │              │              │  (final text)│              │
 │               │              │              │              │              │
 │               │              │    [save to session]        │              │
 │               │              │◄──OutboundMsg─┤              │              │
 │               │◄──route──────│              │              │              │
 │◄──response────│              │              │              │              │
```

---

## 4. Core Capabilities

### 4.1 Multi-Provider LLM Support

The provider system supports 12+ LLM providers with automatic detection:

| Provider | Base URL | Auto-Detection |
|---|---|---|
| OpenRouter | `openrouter.ai/api/v1` | Key prefix `sk-or-`, model keyword |
| OpenAI | `api.openai.com/v1` | Key prefix `sk-proj-`, keywords `gpt`, `o1`, `o3` |
| Anthropic | `api.anthropic.com/v1` | Key prefix `sk-ant-`, keyword `claude` |
| Gemini | `generativelanguage.googleapis.com` | Key prefix `AIza`, keyword `gemini` |
| DeepSeek | `api.deepseek.com/v1` | Key prefix `sk-ds-`, keyword `deepseek` |
| Groq | `api.groq.com/openai/v1` | Key prefix `gsk_`, keyword `groq` |
| Dashscope | `dashscope.aliyuncs.com` | Keywords `qwen`, `dashscope` |
| Moonshot | `api.moonshot.cn/v1` | Keywords `moonshot`, `kimi` |
| SiliconFlow | `api.siliconflow.cn/v1` | Keyword `siliconflow` |
| Volcengine | `open.volcengineapi.com` | Keyword `volcengine` |
| Zhipu | `open.bigmodel.cn` | Keywords `zhipu`, `glm` |
| MiniMax | `api.minimax.chat/v1` | Keyword `minimax` |

**Detection cascade:**
1. Explicit provider name → direct match
2. API key prefix → auto-detect provider
3. Model name keywords → infer provider
4. Fallback → OpenRouter

**Prompt Caching:** For Anthropic and OpenRouter, the system automatically applies `cache_control: {"type": "ephemeral"}` to system messages and the last tool definition, enabling significant cost reduction on repeated system prompts.

**Model-Specific Overrides:** Known model quirks are handled automatically (e.g., Kimi k2.5 requires temperature ≥ 1.0, DeepSeek R1 defaults to temperature 0.6).

**JSON Repair:** Malformed tool call arguments from LLMs (trailing commas, single quotes, unquoted keys, missing brackets) are automatically repaired before parsing.

### 4.2 Tool System

All tools implement a clean 4-method interface:

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() json.RawMessage   // JSON Schema
    Execute(args map[string]any) (string, error)
}
```

**Full tool inventory:**

| Tool | Name | Description |
|---|---|---|
| ReadFile | `read_file` | Read file contents with optional offset/limit, line numbers |
| WriteFile | `write_file` | Write content to file, auto-creates parent dirs |
| EditFile | `edit_file` | Find-and-replace with uniqueness check + diff on failure |
| ListDir | `list_dir` | List directory entries (capped at 100) |
| Shell | `exec` | Execute shell commands with safety deny-patterns, 30s timeout |
| WebSearch | `web_search` | Brave Search API with configurable result count |
| WebFetch | `web_fetch` | HTTP GET with 15KB truncation |
| QueryAPI | `query_api` | Read-only GET to configured internal services |
| Message | `message` | Send messages with optional media to any channel/chat |
| Spawn | `spawn` | Launch background sub-agents with independent ReAct loops |
| Cron | `cron` | Add/list/remove scheduled jobs at runtime |
| MCP | `mcp_*` | Dynamically discovered tools from external MCP servers |

**Error Self-Correction:** Every error result is appended with `"[Analyze the error above and try a different approach.]"` to guide the LLM toward autonomous recovery.

**Edit Intelligence:** When `edit_file` fails to find the target string, it computes similarity against all windows in the file and returns a unified diff of the closest match with similarity percentage. This dramatically reduces edit retry failures.

### 4.3 Memory System

```
workspace/memory/
  MEMORY.md    ← Long-term facts (cumulative, updated by LLM)
  HISTORY.md   ← Chronological event log (append-only)
```

**Consolidation Flow:**

1. **Trigger:** Auto-consolidation fires when unconsolidated messages exceed `memory_window`. Also triggered by `/new` command.
2. **Message Formatting:** Old session messages are formatted with timestamps, roles, and tool usage annotations, truncated to 500 chars.
3. **LLM Call:** A dedicated consolidation LLM call with a `save_memory` tool schema forces structured output.
4. **Output:** The LLM returns:
   - `history_entry` → timestamped paragraph appended to HISTORY.md
   - `memory_update` → complete updated MEMORY.md (existing facts + new discoveries)
5. **Pointer Update:** The session's `last_consolidated` pointer advances.

### 4.4 Session Management

Sessions are stored as JSONL files, one per channel+chatID:

```
{"_type":"metadata","key":"lark:oc_abc","last_consolidated":42,"updated_at":"..."}
{"role":"user","content":"...","timestamp":"2026-02-27T10:00:00Z"}
{"role":"assistant","content":"...","timestamp":"2026-02-27T10:00:05Z"}
```

Key behaviors:
- **Consolidation pointer** tracks which messages are already in long-term memory
- **Tool result truncation** at 500 chars keeps session files lean
- **Orphan pruning** drops leading non-user messages on load
- **Archive on clear** preserves old sessions in `sessions/archive/`

### 4.5 Skills Framework

Skills are markdown files in `workspace/skills/` with optional YAML frontmatter:

```yaml
---
name: code-review
description: Perform thorough code reviews
always_on: true
---
[skill body injected into system prompt]
```

Two formats: flat files (`skills/name.md`) and directories (`skills/name/SKILL.md`).

- **Always-on skills:** Full body injected into every system prompt
- **On-demand skills:** Listed as catalog; LLM loads via `read_file()` when relevant

### 4.6 Subagent Spawning

The main agent can delegate tasks to independent background workers:

- Runs in separate goroutine with own `context.Context` for cancellation
- Own ReAct loop (max 15 iterations) with reduced tool set (no spawn, cron, message)
- Cannot spawn further sub-agents (prevents recursion)
- Results announced back via message bus as `system` channel messages
- All subagents for a session cancelled via `/stop`

### 4.7 Cron Scheduling

Three job types, both static (config) and dynamic (runtime):

| Kind | Specification | Example |
|---|---|---|
| `cron` | Standard cron expression with timezone | `CRON_TZ=Asia/Hong_Kong 0 9 * * MON-FRI` |
| `every` | Interval in seconds | `@every 3600s` |
| `at` | One-time at Unix timestamp | Self-deletes after firing |

Dynamic jobs persist to `cron_jobs.json` and survive restarts.

### 4.8 Heartbeat Service

LLM-powered periodic decision engine:

1. Every N minutes, reads `HEARTBEAT.md`
2. Presents current time + tasks to LLM via `heartbeat` tool
3. LLM decides: `skip` (nothing to do now) or `run` (execute these tasks)
4. If `run`, pushes to inbound bus triggering full agent loop

The LLM makes contextual decisions: "It's Saturday, skip the standup" or "Monday 9 AM Hong Kong, time for the weekly report."

### 4.9 MCP Integration

Connects to external MCP servers using JSON-RPC 2.0:

- **stdio transport:** Launches subprocess, communicates via stdin/stdout
- **HTTP transport:** POST requests to URL endpoint
- On startup: initialize → discover tools → wrap each as native `mcp_{server}_{tool}`
- Results truncated at 15KB

### 4.10 Git Repo Awareness

- **InitAll:** Clones missing repos or pulls existing ones on startup
- **PullLoop:** Background goroutine pulls all repos every N minutes
- **Context Injection:** Depth-2 directory tree of each repo injected into system prompt

---

## 5. Channel Integration

### 5.1 Lark Integration

**Inbound (Webhook Receiver):**
- HTTP server on `:9000` at `/lark/event`
- Parses `text`, `image`, and `post` (rich text) message types
- Downloads attached images to `workspace/uploads/`
- ACL filtering via `allow_from` OpenID whitelist

**Trigger Modes (per-chat):**
- **@mention mode** (default): Only responds when @mentioned
- **Continue mode**: Responds to all messages (toggled via `/continue` and `/atmode`)

**Outbound (Response Delivery):**
- Short messages → plain text
- Longer responses → Lark interactive cards with markdown
- Headers converted to bold (Lark cards don't support `#` headers)
- Horizontal rules → card `<hr>` elements
- Messages truncated at 25KB
- Tenant access token auto-refreshed

**Image Handling:**
- Downloaded via Lark resource API
- Saved with timestamped filenames
- Passed to LLM as base64 data URLs (OpenAI vision format)

### 5.2 CLI Channel

Simple stdin/stdout for local development and testing. Channel: `cli`, ChatID: `local`.

---

## 6. Safety and Guardrails

### 6.1 Shell Command Deny Patterns

| Pattern | Blocks |
|---|---|
| `rm -rf`, `rm -r` | Recursive deletion |
| `del /f`, `rmdir /s` | Windows destructive commands |
| `format`, `mkfs`, `diskpart` | Disk formatting |
| `dd if=` | Raw disk writes |
| `> /dev/sd*` | Direct device writes |
| `shutdown`, `reboot`, `poweroff` | System power operations |
| Fork bombs | `:(){ :|:& };:` |
| `chmod -R 777 /` | World-writable root permissions |

### 6.2 Workspace Sandboxing

All filesystem tools resolve paths against the workspace root and reject any path that escapes via traversal (`../../etc/passwd`).

### 6.3 Per-Session Cancellation

Each active task tracked via `context.Context` keyed by session. New messages cancel in-flight tasks. `/stop` cancels tasks + subagents.

### 6.4 Output Truncation

| Source | Limit |
|---|---|
| Shell output | 20,000 chars |
| Web fetch / API | 15,000 chars |
| MCP results | 15,000 chars |
| Session tool results | 500 chars |
| Lark messages | 25,000 chars |
| Directory listings | 100 entries |

---

## 7. Future Roadmap — Developer Workflow Automation

The existing architecture — message bus, tool system, cron scheduler, heartbeat engine, sub-agent spawning, and MCP integration — provides a solid foundation for evolving CCMonet from a reactive assistant into a proactive developer workflow automation platform.

### 7.1 Developer Identity and Smart Routing

**Concept:** Maintain a mapping between developer identities across platforms (GitHub username, Lark OpenID, email) to enable intelligent message routing and attribution.

**Implementation approach:**
- A `team.json` file in the workspace maps identities:
  ```json
  {
    "developers": [
      {
        "name": "Alice Chen",
        "github": "alicechen",
        "lark_id": "ou_abc123",
        "email": "alice@company.com",
        "teams": ["backend", "platform"]
      }
    ]
  }
  ```
- When the agent discusses a file, commit, or PR, it looks up the author via `git blame` and @mentions the correct developer in Lark
- Enables natural interactions: *"Who last changed the auth middleware?"* → **@Alice** in the response
- File ownership awareness through CODEOWNERS integration

### 7.2 Proactive Code Review

**Concept:** Automatically detect new pull requests and provide AI-powered code reviews, posted directly to Lark with relevant developers tagged.

**Flow:**
1. Cron job (every 5 min) polls GitHub API for new/updated PRs
2. Spawn a sub-agent per PR for parallel review
3. Sub-agent fetches diff, reads affected files, analyzes for:
   - Logic errors and edge cases
   - Security concerns (injection, auth bypass, secrets)
   - Breaking API changes
   - Test coverage gaps
4. Cross-reference changed files with CODEOWNERS and developer identity map
5. Post a Lark card with severity indicators, file-by-file analysis, and PR links

### 7.3 Intelligent Standup and Status Reports

**Daily standup** (`0 9 * * MON-FRI`):
- Query each developer's commits, open PRs, closed issues from last 24h
- Generate per-developer summary: *"Alice: merged PR #42 (auth refactor), opened PR #45 (rate limiter), 3 commits to backend/api."*
- Post combined standup card to team Lark group

**Weekly sprint report** (`0 17 * * FRI`):
- Features shipped (merged PRs)
- Open items (in-progress PRs, assigned issues)
- Stale PRs (no activity > 3 days)
- Blockers (failing CI, unreviewed PRs)
- Velocity metrics (commits, lines changed, PRs merged)

### 7.4 Incident Response

**Health monitoring:** Heartbeat or cron periodically hits configured service health endpoints.

**Alert triage on failure:**
1. Query recent deployment history and commits
2. Identify last deployer and relevant changes
3. Cross-reference with developer identity map
4. Send Lark alert to on-call developer with:
   - Service name, endpoint, error details
   - Last deployment time and deployer
   - Recent relevant commits with authors
   - Suggested action (rollback hash, deploy pipeline link)

### 7.5 Knowledge Base and Onboarding

- **Codebase indexing:** On repo pull, build/update module summaries and architectural patterns
- **Question answering:** *"How does authentication work?"* → walks through actual code paths with file:line references
- **Onboarding skills:** Dedicated `skills/onboarding/SKILL.md` walks new developers through architecture, setup, and workflows
- **Documentation drift detection:** Compare README/docs against actual code, flag outdated sections

### 7.6 CI/CD Integration

- **Chat commands:** *"Deploy backend to staging"* → triggers pipeline via API
- **Failure analysis:** Fetch build log, examine failing test code, generate root cause analysis with commit attribution
- **Flaky test detection:** Track test results over time, auto-retry intermittent failures, escalate real issues

### 7.7 Cross-Repo Dependency Tracking

- Scan `go.mod`, `package.json`, `requirements.txt` across all repos
- Maintain version matrix in `workspace/dependencies.json`
- Alert on incompatible versions and known CVEs
- Generate coordinated upgrade plans when security fixes are available

### 7.8 Meeting and Decision Logging

In `/continue` mode, the bot observes group chat discussions and can:

- **Extract decisions:** *"Decided to use PostgreSQL instead of MongoDB for analytics"*
- **Track action items:** *"Alice will update the API schema by Friday"* with assignees and deadlines
- **Generate ADRs:** Architecture Decision Records with context, decision, and consequences
- **Remind on overdue items:** Heartbeat checks for stale action items and sends reminders

---

## 8. Configuration Reference

```json
{
  "mode": "local | cloud",
  "workspace": "./workspace",
  "llm": {
    "provider": "openrouter",
    "model": "openrouter/arcee-ai/trinity-large-preview:free",
    "api_key": "sk-...",
    "base_url": "",
    "max_tokens": 8192,
    "temperature": 0.3
  },
  "repos": [
    {
      "name": "ccmonet-go",
      "path": "repos/ccmonet-go",
      "remote": "https://<PAT>@github.com/org/repo.git",
      "remote_local": "git@github.com:org/repo.git",
      "branch": "main"
    }
  ],
  "services": [
    {
      "name": "backend-api",
      "base_url": "https://api.internal.com",
      "health_path": "/health",
      "token": "bearer-token",
      "mcp_url": "http://localhost:3100/mcp",
      "mcp_cmd": "npx @modelcontextprotocol/server-filesystem /path"
    }
  ],
  "channels": [
    {
      "type": "lark",
      "enabled": true,
      "app_id": "cli_...",
      "app_secret": "...",
      "allow_from": ["ou_user1", "ou_user2"]
    }
  ],
  "heartbeat": {
    "enabled": true,
    "interval_minutes": 30
  },
  "cron": [
    {
      "name": "daily-standup",
      "schedule": "0 9 * * MON-FRI",
      "task": "Generate daily standup summary."
    }
  ],
  "max_iterations": 20,
  "memory_window": 50,
  "send_progress": true,
  "send_tool_hints": false,
  "brave_api_key": "BSA..."
}
```

**Key parameters:**

| Parameter | Default | Description |
|---|---|---|
| `mode` | `"local"` | `"local"` uses SSH remotes; `"cloud"` uses HTTPS+PAT |
| `workspace` | `"./workspace"` | Root for sessions, memory, skills, repos, uploads |
| `llm.provider` | `"openrouter"` | LLM provider (or `"auto"` for auto-detection) |
| `llm.max_tokens` | `8192` | Maximum output tokens per LLM call |
| `llm.temperature` | `0.3` | Sampling temperature |
| `max_iterations` | `20` | Maximum ReAct loop iterations |
| `memory_window` | `50` | Messages before auto-consolidation triggers |
| `send_progress` | `true` | Send "working..." progress messages to users |
| `send_tool_hints` | `false` | Include detailed tool call descriptions in progress |
| `heartbeat.interval_minutes` | `30` | Minutes between heartbeat ticks |

**Workspace directory structure:**

```
workspace/
├── SOUL.md              ← Core personality/instructions
├── HEARTBEAT.md         ← Periodic task definitions
├── AGENTS.md            ← Multi-agent rules (optional)
├── USER.md              ← Team profiles (optional)
├── TOOLS.md             ← Tool guidelines (optional)
├── IDENTITY.md          ← Identity details (optional)
├── cron_jobs.json       ← Persisted dynamic cron jobs
├── memory/
│   ├── MEMORY.md        ← Long-term consolidated facts
│   └── HISTORY.md       ← Chronological event log
├── sessions/
│   ├── lark_oc_abc.jsonl
│   └── archive/         ← Archived sessions from /new
├── skills/
│   ├── code-review.md
│   └── deploy/
│       └── SKILL.md
├── repos/
│   ├── ccmonet-go/
│   └── frontend/
└── uploads/
    └── 170901234_img.png
```

---

*End of Technical Report*
