package main

import (
	"time"

	"github.com/michalswi/owrap/utils"
)

const (
	separator = "------------------------------------------------------------"
)

var version = "v0.1.1"

var defaultSystemPrompt = `
You are an experienced assistant that can request shell commands to be run.
When you want a command executed, respond ONLY as JSON:
{"action": "run_command", "command": "<shell command>"}

Otherwise respond as:
{"action": "answer", "text": "<your normal answer>"}

Never include backticks, comments, or extra keys.
`

var ollamaURL = utils.GetEnv("OLLAMA_URL", "http://localhost:11434/api/chat")
var modelName = utils.GetEnv("OLLAMA_MODEL", "gemma3:4b")
var systemPrompt = utils.GetEnv("SYSTEM_PROMPT", defaultSystemPrompt)

var sessionMessages []ChatMessage
var cachedBlocks []string
var autoAnalyze bool
var startTime time.Time
