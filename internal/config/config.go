package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DefaultAnthropicModel is the model string written to a fresh config.yaml. It
// is also the sentinel the provider factory checks when deciding whether to
// override to an OpenAI-compatible model under --auth=copilot.
const DefaultAnthropicModel = "claude-sonnet-4-6"

type Config struct {
	Model          string `yaml:"model"`
	MaxTokens      int    `yaml:"max_tokens"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	Readonly       bool   `yaml:"readonly"`
	ModelProvider  string `yaml:"model_provider"`
	OpenAIBaseURL  string `yaml:"openai_base_url"`
	Watch          bool   `yaml:"-"`
	Mode           string `yaml:"-"`
}

var defaults = Config{
	Model:          DefaultAnthropicModel,
	MaxTokens:      16384,
	TimeoutSeconds: 10,
	Readonly:       true,
	ModelProvider:  "anthropic",
}

func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := writeDefaults(path); err != nil {
			return nil, fmt.Errorf("creating config: %w", err)
		}
		c := defaults
		return &c, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	c := defaults
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &c, nil
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tfpilot", "config.yaml"), nil
}

func writeDefaults(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(defaults)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
