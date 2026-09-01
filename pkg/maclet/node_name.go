package maclet

import (
	"os"
	"strings"
	"unicode"
)

// defaultNodeName follows the usual node-agent convention of using the local
// hostname while ensuring the result is valid as a Kubernetes DNS name.
func defaultNodeName() string {
	hostname, err := os.Hostname()
	if err != nil {
		return fallbackNodeName
	}
	if name := nodeNameFromHostname(hostname); name != "" {
		return name
	}
	return fallbackNodeName
}

func nodeNameFromHostname(hostname string) string {
	labels := strings.Split(strings.ToLower(strings.TrimSpace(hostname)), ".")
	normalized := make([]string, 0, len(labels))
	for _, raw := range labels {
		var label strings.Builder
		lastSeparator := false
		for _, char := range raw {
			switch {
			case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
				label.WriteRune(char)
				lastSeparator = false
			case char == '-' || char == '_' || unicode.IsSpace(char):
				if label.Len() > 0 && !lastSeparator {
					label.WriteByte('-')
					lastSeparator = true
				}
			default:
				if label.Len() > 0 && !lastSeparator {
					label.WriteByte('-')
					lastSeparator = true
				}
			}
		}
		value := strings.Trim(label.String(), "-")
		if value == "" {
			continue
		}
		if len(value) > 63 {
			value = strings.TrimRight(value[:63], "-")
		}
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	name := strings.Join(normalized, ".")
	if len(name) > 253 {
		name = strings.TrimRight(name[:253], ".-")
	}
	return name
}
