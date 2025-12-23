package main

import (
	"time"

	"github.com/michalswi/owrap/utils"
)

const (
	separator = "------------------------------------------------------------"
)

var version = "v0.2.0"

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
var modelName = utils.GetEnv("OLLAMA_MODEL", "gemma3:4b")
var systemPrompt = utils.LoadSystemPrompt(defaultSystemPrompt)

var sessionMessages []ChatMessage
var cachedBlocks []string
var autoAnalyze bool
var startTime time.Time
