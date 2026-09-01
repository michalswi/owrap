# OWRAP Application Context Prompt

<application_context>

## Purpose

You are working with **OWRAP**, a small Go application that wraps an Ollama-compatible chat endpoint. Treat this document as technical context for understanding, reviewing, extending, debugging, or documenting the repository.

OWRAP provides:

- An interactive terminal chat client.
- A browser-based chat UI served by the same Go binary.
- Model-requested local shell command execution.
- Optional model analysis of command output.
- Conversation statistics and JSON session save/load.
- Runtime selection or editing of system prompts.
- A beta web-only, server-owned autonomous agent with file attachments and background jobs.

The project is intentionally lightweight. It uses the Go standard library for HTTP, JSON, process execution, embedding, files, flags, and synchronization. Its only direct third-party dependency is `github.com/michalswi/color`, used for terminal colors.

## Product Boundaries

Keep these boundaries in mind when reasoning about the app:

- Ollama performs all language-model inference. OWRAP is an orchestrator, not a model runtime.
- OWRAP has no database, user accounts, authentication, authorization, or multi-node coordination.
- The browser UI is a single embedded HTML file with inline CSS and vanilla JavaScript.
- CLI state is process-global. Web chat state is held in an in-memory session map.
- Saved sessions are plain JSON files under `~/.owrap/sessions`.
- Command execution occurs with the operating-system permissions of the OWRAP process.
- The app is described as local-first, but network commands may access remote systems, `OLLAMA_URL` may point to a remote host, and the default web bind `:8080` may listen on all interfaces.

## Repository Map

- `main.go`: shared data types, Ollama client, command runner, background jobs, web server and API handlers, session file helpers, and CLI entry point.
- `agent.go`: autonomous run/event model, backend worker, limits, strict action parsing, tool execution, and cleanup.
- `autonomous.go`: autonomous start/stop/status/decision HTTP lifecycle and uploaded-file handling.
- `comm.go`: command-name allowlist.
- `help.go`: CLI slash-command dispatcher, help output, prompt editor, session restore, and prompt-history display.
- `vars.go`: version, default system prompt, environment-derived configuration, global CLI state, and web help text.
- `banner.go`: terminal banner.
- `utils/utils.go`: environment lookup and startup system-prompt loading.
- `webstatic/index.html`: complete browser UI, process-lifetime server-state restoration, local preferences, API calls, autonomous status observer, and attachment reader.
- `prompts/*.txt`: predefined role prompts and the autonomous-agent protocol prompt.
- `Makefile`: native, cross-platform, and multi-architecture Docker build targets.
- `Dockerfile`: multi-stage image that runs web mode as an unprivileged `app` user.
- `README.md`: user-facing overview, setup, examples, Docker usage, prompt catalog, and autonomous-mode guide.

## Runtime Configuration

The application reads configuration once during package initialization:

| Variable | Default | Meaning |
|---|---|---|
| `OLLAMA_URL` | `http://localhost:11434/api/chat` | Ollama-compatible chat endpoint |
| `OLLAMA_MODEL` | `qwen3.5:0.8b` | Model sent in every chat request |
| `SYSTEM_PROMPT` | unset | Optional path to the initial system-prompt file |
| `WEB_BIND` | `:8080` | Address used by web mode |

If `SYSTEM_PROMPT` is empty or unreadable, OWRAP silently uses the built-in default and names it `default`.

The only CLI flag is `-web`. Without it, the application starts interactive terminal mode. With it, the application starts the HTTP server.

## Ollama Contract

OWRAP sends a non-streaming `POST` request to `OLLAMA_URL` with this conceptual body:

```json
{
  "model": "<OLLAMA_MODEL>",
  "messages": [
    {"role": "system", "content": "<active system prompt>"},
    {"role": "user", "content": "<conversation input>"}
  ],
  "stream": false,
  "think": false,
  "format": "json"
}
```

`format: "json"` is included only in autonomous mode. `think` defaults to `false` and is changed at runtime with `/think-on` or `/think-off`. The expected response contains `message.role` and `message.content`; supported thinking models may also return `message.thinking`. OWRAP preserves reasoning separately from answer content and does not stream tokens.

The ordinary model/tool protocol uses one JSON object:

```json
{"action":"answer","text":"<answer for the user>"}
```

or:

```json
{"action":"run_command","command":"<command to execute>"}
```

In normal chat, malformed or plain-text model output is displayed as an answer. In autonomous mode, valid JSON is mandatory and malformed output enters a retry flow.

The HTTP client creates context-aware requests, so Web request cancellation propagates to Ollama. It uses the default HTTP client without a fixed timeout and does not explicitly reject non-success HTTP status codes before decoding the response.

## Core Data Model

`ChatMessage` contains `role`, `content`, and optional `thinking` fields and is the shared conversation unit. Reasoning remains separate from visible answer content.

`Stats` tracks:

- User-message count.
- Assistant-message count.
- Commands run.
- Total user characters.
- Total assistant characters, including retained reasoning.
- Current context characters and estimated tokens, including retained reasoning.
- Last command.

`Session` is the disk representation: timestamp, model, messages, stats, and CLI cached blocks.

`webSession` adds browser-specific state:

- Session ID and creation time.
- Messages and stats.
- Per-session auto-analysis flag.
- Per-session thinking flag, default disabled.
- An `AutonomousRun` snapshot with immutable prompt metadata, structured goal fields, detected environment capabilities, plan, compacted summary, status, limits, result, error, and sequenced events.
- A runtime-only cancellation function for the active autonomous worker.
- Optional attachment metadata and temporary path.

Autonomous run statuses are `running`, `waiting_approval`, `waiting_input`, `completed`, `failed`, `cancelled`, and `limit_reached`. Events record model actions, tool observations, plan updates, critic verification, clarification, user feedback, answers, and failures in sequence order.

`ToolResponse` supports these action fields:

- `action`
- `command`
- `text`
- `jobId`
- `steps`
- `stepId`
- `evidenceEventIds`

Recognized action values are `answer`, `request_clarification`, `create_plan`, `complete_step`, `run_command`, `run_command_bg`, `check_job`, `get_job`, `cancel_job`, `list_jobs`, and `update_findings`.

## CLI Flow

At startup, CLI mode:

1. Prints the banner and version.
2. creates a message list beginning with the active system prompt.
3. Reads terminal input in a loop.
4. Handles local slash commands before contacting Ollama.
5. Sends ordinary input plus full conversation history to Ollama.
6. Parses the model response as `ToolResponse` JSON when possible.
7. Displays answers or executes requested commands.
8. Optionally asks Ollama for a separate analysis of command output.

CLI slash commands include:

| Command | Behavior |
|---|---|
| `/h` | Show help and the sorted command allowlist |
| `/q` | Exit immediately |
| `/dir` | Show working directory |
| `/m` | Show configured model |
| `/up` | Show process uptime |
| `/s`, `/stats` | Show conversation and command statistics |
| `/last` | Show the latest user prompt and following assistant message |
| `/myprompts` | List user messages in the current session |
| `/sysprompt` | Print the current global system prompt |
| `/editsysprompt` | Select a prompt file or enter a custom prompt |
| `/save [NAME]` | Save JSON under `~/.owrap/sessions` |
| `/load NAME` | Restore messages, stats, and cached blocks |
| `/sessions`, `/list` | List saved session files |
| `/p [DELIM]`, `/paste [DELIM]` | Read a multiline block, default delimiter `EOF` |
| `/cache` | List multiline blocks cached without sending |
| `/use N [question]` | Send a cached block, optionally with a question |
| `/auto-on`, `/auto-off` | Toggle post-command model analysis |
| `/think-on`, `/think-off` | Toggle Ollama model reasoning for subsequent requests |
| `/execfile P`, `/xf P` | Execute each nonempty, non-comment file line without analysis |

A multiline paste with no follow-up instruction is cached. A paste with a follow-up is sent as one user message.

Session loading restores active conversational context. Saved sessions can therefore be continued, although the configured model itself is not changed to the model named in the saved file.

## Web Architecture

Web mode uses `net/http.ServeMux`. The binary embeds `webstatic/*` with `go:embed`; `index.html` is served at `/`, and embedded files are exposed under `/static/`.

The Go server owns one active Web UI session and persists it to `~/.owrap/web_state.json` during the process lifetime. A newly opened or refreshed browser restores structured messages and stats through `GET /api/state`; `localStorage` is only a fallback for UI state and stores theme preferences. Starting OWRAP initializes and persists a fresh session rather than restoring the previous process's active chat. A generated session ID has the form `sess_<UnixNano>`.

HTTP endpoints:

| Endpoint | Purpose |
|---|---|
| `POST /api/chat` | Normal chat, slash commands, tool actions, and jobs; rejected while an autonomous run is active |
| `GET`, `DELETE /api/state` | Restore or clear the shared active Web UI session |
| `GET /api/prompt` | Active prompt, prompt name, model, and app version |
| `GET /api/prompts/list` | Available `.txt` prompt files and current selection |
| `POST /api/prompts/update` | Select a prompt file or submit custom prompt text |
| `GET /api/help` | Web command help |
| `POST /api/command` | Execute a caller-supplied command directly |
| `POST /api/autonomous/start` | Start autonomous mode and optionally save an attachment |
| `POST /api/autonomous/stop` | Stop autonomous mode and clean its temporary directory |
| `GET /api/autonomous/status` | Return the current run snapshot and sequenced events |
| `POST /api/autonomous/decision` | Continue from user feedback or accept a candidate answer |
| `GET /api/health` | Return HTTP 200 if the OWRAP server is running |
| `GET /api/ollama/status` | Probe Ollama's `/api/tags` endpoint with a two-second timeout |

Web chat recognizes `/auto-on`, `/auto-off`, `/think-on`, `/think-off`, `/last`, `/stats`, `/s`, `/allowedcomm`, `/save`, `/load`, `/sessions`, and `/list`. Web session save/load uses the same `~/.owrap/sessions` format as CLI mode.

The UI polls Ollama status every ten seconds, shows prompt/model/session statistics, supports dark/light theme state, displays reusable prompt history, and renders returned reasoning in a collapsed disclosure. For autonomous work, JavaScript starts a run once and polls `/api/autonomous/status` every second; it observes sequenced events and submits stop, approval, revision-feedback, or clarification decisions but does not drive model iterations. The backend captures the selected default, predefined, or custom prompt for the run and appends the domain-neutral JSON protocol from `prompts/autonomous_agent.txt`. Earlier chat and autonomous messages remain visible but are excluded from the run's token-budgeted event context. An `answer` is independently verified before changing the run to `waiting_approval`; `request_clarification` changes it to `waiting_input`. Normal chat remains disabled in both states. Malformed JSON and unsupported actions share one three-attempt failure counter, while tool failures become observations so the agent can choose another action. The thinking controls use a green selected state and status badge when enabled and amber when disabled. Returned reasoning persists in `~/.owrap/web_state.json`, but thinking mode resets to disabled whenever the application starts.

## Prompt System

The built-in default prompt tells the model to answer normally unless the user explicitly asks it to execute a shell command. It requires the `answer` or `run_command` JSON schema.

Predefined files provide these roles:

- `apps_developer.txt`: senior full-stack application developer.
- `cloud_engineer.txt`: Azure and Google Cloud engineer.
- `japanese_teacher.txt`: Japanese-language teacher.
- `local_network_recon.txt`: authorized local network reconnaissance assistant.
- `web_recon.txt`: authorized OWASP-oriented web reconnaissance assistant.
- `shell_command_assistant.txt`: command-first local shell assistant.
- `prompt_engineer.txt`: system-prompt design specialist.
- `autonomous_agent.txt`: strict JSON autonomous worker.

The prompt directory is read from the current working directory at runtime. It is not embedded with the web UI. Distributions must therefore keep `prompts/` beside the running application, especially for autonomous mode.

Prompt selection for normal chat is global, not stored independently in each web session. An autonomous run captures an immutable copy of the selected prompt at startup and does not mutate the global prompt.

In CLI mode, `/editsysprompt` updates the global prompt variables, but the already-created in-memory `messages` slice retains its original system message. Without rebuilding that slice, later model requests may continue to use the old prompt even though `/sysprompt` reports the new one.

## Command Execution

The nominal command-name allowlist contains:

```text
ansible, arp, awk, cat, chmod, curl, cut, date, df, dig, echo, ffuf,
find, for, grep, head, host, httpx, ip, jq, ls, mkdir, nc, netcat,
nmap, nslookup, openssl, ping, pwd, sed, sh, sort, subfinder, tail,
telnet, terraform, touch, traceroute, tree, uniq, uptime, wc, wget,
while, whois, xargs
```

The simple-command path:

- Trims whitespace, surrounding backticks, and one leading `$`.
- Tokenizes with `strings.Fields`; it does not implement shell quoting.
- Checks the first executable name against `allowedCommands`.
- Supports no pipe or one pipe only.
- Checks both sides of a normal one-pipe pipeline.
- Special-cases `tee` on the right side and supports `tee -a`.
- Supports one `>` or `>>` stdout redirection.
- Executes programs directly with `exec.Command`.
- Captures stdout and stderr and returns them as text.

A multiline request is split into lines and each line is sent through the simple-command path. `/execfile` also invokes the simple path per line.

A command containing `&&`, `||`, or `;` takes a different path and is executed with `bash -c`. Background commands are also executed with `bash -c` and have a hard-coded one-hour timeout.

Background jobs have IDs like `job_<UnixNano>` and statuses `running`, `completed`, `failed`, `timeout`, or `cancelled`. Their output and errors remain in an in-memory global job store. Jobs can be listed by session, checked, fetched in full, or cancelled.

## Autonomous Mode

Autonomous mode is beta and web-only. Its intended lifecycle is:

1. The browser submits a required objective, optional expected output, constraints, completion criteria, and optional file attachment.
2. The server creates or reuses a web session and captures the current global prompt in a new `AutonomousRun`.
3. Attachment content is written under `~/.owrap/autonomous_files/<session-id>/`.
4. The run is persisted before its backend worker starts.
5. The worker composes the captured prompt with `prompts/autonomous_agent.txt`, the detected runtime capabilities, the current plan, the persisted summary, and recent token-budgeted events.
6. The agent creates or updates a plan, runs commands, starts or checks run-owned jobs, records findings, asks for clarification, or returns a candidate answer.
7. Clarification pauses in `waiting_input`; user feedback starts another worker.
8. A separate critic verifies a candidate against the goal, plan, and evidence before it pauses in `waiting_approval`; revision feedback starts another worker, while acceptance completes the run.
9. Stop, failure, cancellation, and limits terminate the worker, cancel run-owned jobs, and remove temporary attachments.

Autonomous state management includes:

- A complete persisted event log plus a 4096-token recent-event context budget.
- Semantic compaction of older events into a persisted summary, with a 768-token summary instruction budget.
- Runtime detection of installed and unavailable allowlisted commands, operating system, and working directory.
- Structured plans with evidence references and independent candidate-answer verification.
- A maximum of 30 iterations and a 30-minute overall deadline.
- Two-minute model-call and foreground-command deadlines.
- Tool observations truncated to 32 KiB before entering model context.
- Up to three consecutive protocol or model failures.
- Strict single-object JSON decoding with unknown fields rejected.
- A completion gate that requires an observation when the goal explicitly names an allowlisted command.
- Per-run cancellation for foreground commands and background jobs.

The server worker owns the loop. Closing or refreshing the browser does not stop progress while OWRAP remains running; the UI reconstructs it from persisted status and events.

The autonomous prompt is designed for larger local models and insists on a single raw JSON object. It encourages different approaches after failures and a comprehensive final report only when the goal is complete or proven impossible.

## Persistence and Lifecycle

CLI and web saves use JSON files in `~/.owrap/sessions`. An omitted name produces `owrap_YYYYMMDD_HHMM.json`; a supplied name receives `.json` if needed. Files are sorted alphabetically when listed.

Starting OWRAP creates a fresh active Web conversation, removes stale autonomous attachments, and replaces `~/.owrap/web_state.json`. Previous conversations remain available only when explicitly saved under `~/.owrap/sessions` and restored with `/load`. Browser refreshes during the same process still restore the active chat and autonomous run. Background job process metadata remains memory-only.

Autonomous attachments are temporary and terminal run cleanup removes the session directory. A waiting-approval or waiting-input run retains its attachment until it continues, is accepted where applicable, or is stopped.

## Build and Deployment

The module path is `github.com/michalswi/owrap`, and `go.mod` declares Go `1.25.5`.

Common build targets:

```sh
make build
make build-mac
make build-linux
make go-build
make docker-build
```

The Docker build uses Go on Alpine, creates a stripped static-style binary with `CGO_ENABLED=0`, and copies the binary plus `prompts/` into an Alpine runtime image. The runtime installs a limited subset of command-line tools, runs as the unprivileged `app` user, and starts `./owrap -web`.

The Docker image supports `linux/amd64` and `linux/arm64` through `docker buildx`. Commands present in the source allowlist are not necessarily installed in the image; the repository explicitly calls out `ffuf`, `subfinder`, `httpx`, `terraform`, and `ansible` as unavailable there.

## Verified Limitations and Risks

Do not overstate the current guarantees. Account for these implementation realities when proposing changes or answering questions:

1. **Allowlist bypasses exist.** Commands containing `&&`, `||`, or `;` bypass per-command allowlist checks through `bash -c`. Background jobs do the same. The allowlisted `sh` command can also invoke arbitrary shell content. Therefore the current implementation is not a strict command sandbox.
2. **Direct command API has no authentication.** Any client that can reach `POST /api/command` can request command execution as the OWRAP process user.
3. **File writes are broad.** Redirection, `tee`, attachments, prompt filenames, and session names are not confined by a robust filesystem sandbox. Treat path traversal and arbitrary writable paths as review concerns.
4. **Normal Web prompt state is globally coupled.** Concurrent normal-chat sessions can change one another's selected prompt. Autonomous runs avoid mid-run prompt changes by capturing an immutable prompt at startup.
5. **“Local” is a deployment intention, not an enforced network boundary.** The default bind may expose the server beyond loopback, and many allowlisted tools make outbound network requests.
6. **No HTTP hardening layer is present.** There is no authentication, CSRF defense, request-body limit, general server timeout configuration, or TLS termination in the app.
7. **Normal Ollama calls can hang.** Autonomous calls have a two-minute deadline, but normal chat still uses no fixed model timeout.
8. **Prompt/tool mismatch exists.** The autonomous and shell-assistant prompts recommend `uname`, but `uname` is absent from the simple-command allowlist.
9. **CLI prompt editing is not fully applied to existing context.** Displayed global prompt state can diverge from the system message actually sent in the CLI conversation.
10. **Statistics are approximate.** Some raw fallback responses and command-output messages are appended without incrementing assistant counters.
11. **Autonomous runs do not survive application restart.** Action intent is persisted for process-lifetime recovery and audit, but startup intentionally creates a fresh chat instead of replaying interrupted work.
12. **Build metadata has a naming mismatch.** The Makefile injects `main.Version`, while the source declares lowercase `version`; the linker override therefore does not target that variable.
13. **README security wording is stronger than the implementation.** README says model commands are allowlisted, but shell and background paths mean that is not universally true.

## Guidance for Models Working on This Repository

When asked to modify or analyze OWRAP:

1. Preserve the two-mode product: CLI by default and embedded web UI with `-web`.
2. Keep Ollama compatibility and the existing `ChatMessage` conversation shape unless a migration is explicitly requested.
3. Treat command execution, HTTP exposure, path handling, uploaded content, and session concurrency as trust boundaries.
4. Never assume the allowlist alone is a sandbox; trace the exact execution path.
5. Distinguish process-global configuration from per-session state.
6. Keep CLI and web behavior aligned where they advertise the same feature.
7. Keep normal chat tolerant of plain-text model responses unless requirements change.
8. Keep autonomous actions strict and machine-readable; update the Go schema, prompt, worker, observer UI, and lifecycle tests together when changing the protocol.
9. Preserve conversation save-file compatibility when practical.
10. Remember that web assets are embedded but prompt files are runtime filesystem dependencies.
11. Prefer focused standard-library solutions consistent with this small codebase.
12. Add tests proportionate to risk, especially before changing command execution or autonomous state transitions.
13. Update `README.md`, prompt files, and this document when user-visible behavior or model contracts change.
14. Do not claim a security property unless every command path and HTTP entry point enforces it.

## Compact Mental Model

Think of OWRAP as five cooperating layers:

1. **Interface layer:** terminal loop or browser JavaScript; the browser observes and controls autonomous runs.
2. **Conversation layer:** system prompt plus `ChatMessage` history and stats.
3. **Model layer:** non-streaming Ollama `/api/chat` calls using a small JSON action protocol.
4. **Action layer:** synchronous commands, background jobs, analysis calls, and autonomous state transitions.
5. **Persistence layer:** active Web state and saved sessions under `~/.owrap`, in-memory job state, and browser-local UI preferences.

A normal request moves from interface to conversation to Ollama, then either returns an answer or enters command execution. In autonomous mode, a backend worker repeats strict action/observation steps until approval, manual stop, failure, cancellation, or a configured limit ends the run.

</application_context>
