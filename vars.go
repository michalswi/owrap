package main

import (
	"time"

	"github.com/michalswi/owrap/utils"
)

const (
	separator = "------------------------------------------------------------"
)

var version = "v0.5.2"

var defaultSystemPrompt = `
You are a helpful, general-purpose assistant.

Default behavior: answer the user's question.

Only when the user explicitly and strictly asks you to execute a shell command (e.g., "execute this command:", "run:", "run this command", "run the following"), respond ONLY as JSON:
{"action": "run_command", "command": "<shell command>"}

In all other cases, respond ONLY as JSON:
{"action": "answer", "text": "<your normal answer>"}

Never include backticks, comments, or extra keys.
`

var ollamaURL = utils.GetEnv("OLLAMA_URL", "http://localhost:11434/api/chat")
var modelName = utils.GetEnv("OLLAMA_MODEL", "qwen3.5:0.8b")
var systemPrompt, systemPromptName = utils.LoadSystemPromptWithName(defaultSystemPrompt)
var webBindDefault = utils.GetEnv("WEB_BIND", ":8080")

var sessionMessages []ChatMessage
var cachedBlocks []string
var autoAnalyze bool
var thinkingEnabled bool
var startTime time.Time

const webHelpText = `Web UI commands:
/myprompts     Show all your prompts from current session (click to reuse)
/auto-on       Enable automatic analysis after commands
/auto-off      Disable automatic analysis after commands
/think-on      Enable model reasoning for subsequent requests
/think-off     Disable model reasoning (default)
/save [NAME]   Save current session to ~/.owrap/sessions (auto-named if NAME omitted)
/load NAME     Load a saved session by name
/sessions      List all saved sessions in ~/.owrap/sessions
/last          Show last prompt and assistant reply
/stats         Show live session stats (also visible in UI)
/allowedcomm   Show the allowlisted shell commands
`
