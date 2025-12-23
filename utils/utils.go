package utils

import (
	"os"
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
	path := strings.TrimSpace(GetEnv("SYSTEM_PROMPT", ""))
	if path == "" {
		return defaultSystemPrompt
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return defaultSystemPrompt
	}
	return string(data)
}
