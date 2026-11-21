package config

import (
	"strings"
	"testing"
)

func TestParseConfigRejectsInvalidKeyRateLimit(t *testing.T) {
	for name, block := range map[string]string{
		"bad duration":  "min-interval: soon",
		"max below min": "min-interval: 2s\n            max-interval: 1s",
		"negative cap":  "daily-max-requests: -1",
		"jitter":        "jitter-percent: 2",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfigBytes([]byte(`config-version: 8
api-keys:
  opencode-go:
    - name: go
      keys:
        - api-key: fixture
          rate-limit:
            ` + block + `
`))
			if err == nil || !strings.Contains(err.Error(), "opencode-go-api-key[0].rate-limit.") {
				t.Fatalf("ParseConfigBytes() error = %v, want the key's rate-limit path", err)
			}
		})
	}
}
