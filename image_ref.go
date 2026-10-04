package main

import (
	"github.com/distribution/reference"
)

// resolveImageRef determines the registry reference to poll for updates,
// independent of whether the running image still carries a repo tag.
func resolveImageRef(configImage string, repoDigests []string) (ref string, ok bool) {
	if hasRepository(configImage) {
		return configImage, true
	}
	for _, rd := range repoDigests {
		if hasRepository(rd) {
			return rd, true
		}
	}
	return "", false
}

// hasRepository reports whether ref is a pullable registry reference with a
// repository name, as opposed to a bare image ID/digest such as
// "sha256:3cb6...".
func hasRepository(ref string) bool {
	parsed, err := reference.ParseAnyReference(ref)
	if err != nil {
		return false
	}
	_, ok := parsed.(reference.Named)
	return ok
}

// repositoryOf returns the normalized repository name of a reference (e.g.
// "ghcr.io/wow-look-at-my/buildhost" or "docker.io/library/nginx").
func repositoryOf(ref string) string {
	parsed, err := reference.ParseAnyReference(ref)
	if err != nil {
		return ""
	}
	named, ok := parsed.(reference.Named)
	if !ok {
		return ""
	}
	return named.Name()
}

// imageIdentity returns a stable, tag-independent identifier for an image.
func imageIdentity(repoDigests []string, contentID, repo string) string {
	if d := matchRepoDigest(repoDigests, repo); d != "" {
		return d
	}
	return contentID
}

// matchRepoDigest returns the manifest digest ("sha256:...") from repoDigests
// whose repository matches repo, or "" if none match. When repo is empty, the
// first parseable digest is returned. Names are normalized on both sides so
// that e.g. "nginx@sha256:..." matches "docker.io/library/nginx".
func matchRepoDigest(repoDigests []string, repo string) string {
	for _, rd := range repoDigests {
		named, err := reference.ParseNormalizedNamed(rd)
		if err != nil {
			continue
		}
		canonical, ok := named.(reference.Canonical)
		if !ok {
			continue
		}
		if repo == "" || named.Name() == repo {
			return canonical.Digest().String()
		}
	}
	return ""
}
