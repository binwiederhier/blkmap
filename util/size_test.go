package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		{"0", 0, false},
		{"512", 512, false},
		{"4k", 4096, false},
		{"4K", 4096, false},
		{"4Ki", 4096, false},
		{"4KiB", 4096, false},
		{"64K", 65536, false},
		{"1M", 1 << 20, false},
		{"10G", 10 << 30, false},
		{"2T", 2 << 40, false},
		{" 1M ", 1 << 20, false},
		{"", 0, true},
		{"-1", 0, true},
		{"1.5G", 0, true},
		{"1X", 0, true},
		{"K", 0, true},
		{"99999999999999999999", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			n, err := ParseSize(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, n)
		})
	}
}

func TestFormatSize(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "0", FormatSize(0))
	assert.Equal(t, "512", FormatSize(512))
	assert.Equal(t, "4K", FormatSize(4096))
	assert.Equal(t, "64K", FormatSize(65536))
	assert.Equal(t, "1M", FormatSize(1<<20))
	assert.Equal(t, "10G", FormatSize(10<<30))
	assert.Equal(t, "2T", FormatSize(2<<40))
	assert.Equal(t, "1536K", FormatSize(1536*1024))
	assert.Equal(t, "4097", FormatSize(4097))
}

func TestFormatSizeApprox(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "0", FormatSizeApprox(0))
	assert.Equal(t, "900", FormatSizeApprox(900))
	assert.Equal(t, "4.0K", FormatSizeApprox(4096))
	assert.Equal(t, "1.5M", FormatSizeApprox(1536*1024))
	assert.Equal(t, "50M", FormatSizeApprox(51520*1024))
	assert.Equal(t, "291M", FormatSizeApprox(298368*1024))
	assert.Equal(t, "1.7G", FormatSizeApprox(1764288*1024))
	assert.Equal(t, "10G", FormatSizeApprox(10<<30))
	assert.Equal(t, "2.0T", FormatSizeApprox(2<<40))
}
