package nest

import (
	"testing"
)

func TestExtractCameraDeviceID(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Full path format",
			input:    "enterprises/735ef91b-89b7-4f32-b8b9-5d54479be0bf/devices/AVPHwEtYJ6eztR1d7sSETV5BsnYWz3hdoMQAUOJjydZjayoQXdcmffuK0DAyjXFv2wQcEWgCSaaoc-3DzgvFvdmWuvUMuA",
			expected: "AVPHwEtYJ6eztR1d7sSETV5BsnYWz3hdoMQAUOJjydZjayoQXdcmffuK0DAyjXFv2wQcEWgCSaaoc-3DzgvFvdmWuvUMuA",
		},
		{
			name:     "Already extracted device ID (short)",
			input:    "abc123",
			expected: "abc123",
		},
		{
			name:     "Already extracted device ID (86 chars - actual Nest format)",
			input:    "AVPHwEtYJ6eztR1d7sSETV5BsnYWz3hdoMQAUOJjydZjayoQXdcmffuK0DAyjXFv2wQcEWgCSaaoc-3DzgvFvdmWuvUMuA",
			expected: "AVPHwEtYJ6eztR1d7sSETV5BsnYWz3hdoMQAUOJjydZjayoQXdcmffuK0DAyjXFv2wQcEWgCSaaoc-3DzgvFvdmWuvUMuA",
		},
		{
			name:     "Another extracted device ID",
			input:    "AVPHwEufu2MyjnYW-PNCTfaQ6a8_rsBjAsST2oOzuiYvEChb4PqsWjzLbSl7gV5K7dL3zz3P5tOhRaEZTGkdMpE5znK1tw",
			expected: "AVPHwEufu2MyjnYW-PNCTfaQ6a8_rsBjAsST2oOzuiYvEChb4PqsWjzLbSl7gV5K7dL3zz3P5tOhRaEZTGkdMpE5znK1tw",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractCameraDeviceID(tt.input)
			if result != tt.expected {
				t.Errorf("extractCameraDeviceID(%q) = %q, expected %q", tt.input, result, tt.expected)
			}
		})
	}
}
