package providerfactory

import (
	"fmt"

	"github.com/rchandnaWUSTL/tfpilot/internal/config"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider/anthropic"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider/copilot"
	"github.com/rchandnaWUSTL/tfpilot/internal/provider/openai"
)

// AuthMode is set by the --auth CLI flag. When non-empty it overrides the
// ModelProvider field from config.yaml.
type AuthMode string

const (
	AuthDefault AuthMode = ""
	AuthCopilot AuthMode = "copilot"
)

// New returns a Provider based on the auth flag and config. The returned
// Provider is ready but NOT yet authenticated — callers must invoke
// Authenticate() once before the first SendMessage.
func New(cfg *config.Config, auth AuthMode) (provider.Provider, error) {
	if auth == AuthCopilot {
		return copilot.New(copilot.Options{}), nil
	}

	switch cfg.ModelProvider {
	case "", "anthropic":
		return anthropic.New(anthropic.Options{}), nil
	case "openai":
		return openai.New(openai.Options{
			Name:    "openai",
			BaseURL: cfg.OpenAIBaseURL,
		}), nil
	default:
		return nil, fmt.Errorf("unknown model_provider %q in config.yaml; expected anthropic or openai", cfg.ModelProvider)
	}
}

// ModelFor returns the effective model name given config and auth mode. Honors
// the --auth=copilot / default-Claude override so the banner announces the
// correct model.
func ModelFor(cfg *config.Config, auth AuthMode) string {
	if auth == AuthCopilot && cfg.Model == config.DefaultAnthropicModel {
		return "gpt-4o"
	}
	return cfg.Model
}
