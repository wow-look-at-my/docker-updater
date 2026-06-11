package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// digestOf builds a valid "sha256:<64-hex>" string from a single hex char.
func digestOf(c string) string {
	return "sha256:" + strings.Repeat(c, 64)
}

func TestHasRepository(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{"tagged ghcr", "ghcr.io/wow-look-at-my/buildhost:latest", true},
		{"tagged hub", "nginx:latest", true},
		{"bare hub name", "nginx", true},
		{"name with digest", "ghcr.io/wow-look-at-my/buildhost@" + digestOf("a"), true},
		{"bare image id", digestOf("3"), false},
		{"bare hex without prefix", strings.Repeat("3", 64), false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasRepository(tt.ref))
		})
	}
}

func TestResolveImageRef(t *testing.T) {
	repoDigest := "ghcr.io/wow-look-at-my/buildhost@" + digestOf("a")

	t.Run("config image is the primary source", func(t *testing.T) {
		ref, ok := resolveImageRef("ghcr.io/wow-look-at-my/buildhost:latest", []string{repoDigest})
		assert.True(t, ok)
		assert.Equal(t, "ghcr.io/wow-look-at-my/buildhost:latest", ref)
	})

	t.Run("bare config image falls back to repo digest", func(t *testing.T) {
		ref, ok := resolveImageRef(digestOf("3"), []string{repoDigest})
		assert.True(t, ok)
		assert.Equal(t, repoDigest, ref)
	})

	t.Run("empty config image falls back to repo digest", func(t *testing.T) {
		ref, ok := resolveImageRef("", []string{repoDigest})
		assert.True(t, ok)
		assert.Equal(t, repoDigest, ref)
	})

	t.Run("no repository anywhere is unresolvable", func(t *testing.T) {
		ref, ok := resolveImageRef(digestOf("3"), nil)
		assert.False(t, ok)
		assert.Equal(t, "", ref)
	})

	t.Run("bare config and bare repo digests are unresolvable", func(t *testing.T) {
		ref, ok := resolveImageRef("", []string{digestOf("3")})
		assert.False(t, ok)
		assert.Equal(t, "", ref)
	})
}

func TestRepositoryOf(t *testing.T) {
	assert.Equal(t, "ghcr.io/wow-look-at-my/buildhost", repositoryOf("ghcr.io/wow-look-at-my/buildhost:latest"))
	assert.Equal(t, "ghcr.io/wow-look-at-my/buildhost", repositoryOf("ghcr.io/wow-look-at-my/buildhost@"+digestOf("a")))
	assert.Equal(t, "docker.io/library/nginx", repositoryOf("nginx:latest"))
	assert.Equal(t, "", repositoryOf(digestOf("3")))
}

func TestImageIdentity(t *testing.T) {
	repo := "ghcr.io/wow-look-at-my/buildhost"
	repoDigest := repo + "@" + digestOf("a")

	t.Run("prefers matching repo digest over content id", func(t *testing.T) {
		assert.Equal(t, digestOf("a"), imageIdentity([]string{repoDigest}, digestOf("9"), repo))
	})

	t.Run("falls back to content id when no repo digests", func(t *testing.T) {
		assert.Equal(t, digestOf("9"), imageIdentity(nil, digestOf("9"), repo))
	})

	t.Run("falls back to content id when no repo digest matches", func(t *testing.T) {
		other := "ghcr.io/other/image@" + digestOf("b")
		assert.Equal(t, digestOf("9"), imageIdentity([]string{other}, digestOf("9"), repo))
	})

	t.Run("matches normalized hub names", func(t *testing.T) {
		assert.Equal(t, digestOf("c"), imageIdentity([]string{"nginx@" + digestOf("c")}, digestOf("9"), "docker.io/library/nginx"))
	})
}
