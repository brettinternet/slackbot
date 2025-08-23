package config

import (
	"testing"
)

func TestPersonasConfigParsing(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
		hasError bool
	}{
		{
			name: "Valid YAML",
			input: `
glazer: "Gen-Z hype beast"
argue: "Argumentative lawyer"
computer: "Self-aware AI"
`,
			expected: map[string]string{
				"glazer":   "Gen-Z hype beast",
				"argue":    "Argumentative lawyer",
				"computer": "Self-aware AI",
			},
			hasError: false,
		},
		{
			name:     "Valid JSON",
			input:    `{"glazer": "Gen-Z hype beast", "argue": "Argumentative lawyer"}`,
			expected: map[string]string{
				"glazer": "Gen-Z hype beast",
				"argue":  "Argumentative lawyer",
			},
			hasError: false,
		},
		{
			name:     "Empty string",
			input:    "",
			expected: map[string]string{},
			hasError: false,
		},
		{
			name:     "Invalid format",
			input:    "invalid: yaml: format",
			expected: nil,
			hasError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := configOpts{
				PersonasConfig: tt.input,
			}

			config, err := newConfig(opts)
			if tt.hasError {
				if err == nil {
					t.Errorf("Expected error, got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if len(config.AIChat.Personas) != len(tt.expected) {
				t.Errorf("Expected %d personas, got %d", len(tt.expected), len(config.AIChat.Personas))
				return
			}

			for name, prompt := range tt.expected {
				if config.AIChat.Personas[name] != prompt {
					t.Errorf("Expected persona %s to be %q, got %q", name, prompt, config.AIChat.Personas[name])
				}
			}
		})
	}
}

func TestDefaultPersonaHandling(t *testing.T) {
	// Test with no personas config
	opts := configOpts{}
	config, err := newConfig(opts)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(config.AIChat.Personas) != 0 {
		t.Errorf("Expected empty personas map, got %d personas", len(config.AIChat.Personas))
	}
}