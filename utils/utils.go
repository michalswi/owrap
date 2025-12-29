package utils

import (
	"os"
	"path/filepath"
	"strings"
)

func GetEnv(key, defaultValue string) string {
	value := os.Getenv(key)
	if len(value) == 0 {
		return defaultValue
	}
	return value
}

func LoadSystemPrompt(defaultSystemPrompt string) string {
	prompt, _ := LoadSystemPromptWithName(defaultSystemPrompt)
	return prompt
}

// LoadSystemPromptWithName returns the system prompt along with a human-friendly name.
// If SYSTEM_PROMPT is unset or unreadable, it falls back to the default and names it "default".
func LoadSystemPromptWithName(defaultSystemPrompt string) (string, string) {
	path := strings.TrimSpace(GetEnv("SYSTEM_PROMPT", ""))
	if path == "" {
		return defaultSystemPrompt, "default"
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return defaultSystemPrompt, "default"
	}
	return string(data), filepath.Base(path)
}
