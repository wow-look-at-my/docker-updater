package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildVersionLdflagsOverrideWins(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	version = "1234567890abcdef"
	assert.Equal(t, "1234567890abcdef", buildVersion())
}

func TestBuildVersionAlwaysNonEmpty(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })

	// Without a link-time override the result is the VCS revision (when the
	// toolchain stamped one) or the "dev" fallback -- never empty, so the
	// startup log line and dashboard footer always have something to show.
	version = ""
	assert.NotEmpty(t, buildVersion())
}
