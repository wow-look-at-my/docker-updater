package main

import (
	"github.com/distribution/reference"
)

// resolveImageRef determines the registry reference to poll for updates,
// independent of whether the running image still carries a repo tag.
//
// The primary source is configImage -- the reference the container was created
// with (its Config.Image, e.g. "ghcr.io/wow-look-at-my/buildhost:latest").
// Docker's container-list view degrades to a bare image ID once the running
// image loses its repo tags, so that view is never trusted; Config.Image is
// recorded at creation time and stays stable.
//
// If Config.Image is itself a bare image ID or digest with no repository
// (e.g. the container was created directly from a raw "sha256:..." ID), the
// running image's RepoDigests are used to recover the repository
// (e.g. "ghcr.io/wow-look-at-my/buildhost@sha256:...").
//
// ok is false when no registry repository can be resolved from either source
// (e.g. a locally-built image that was never pulled from or pushed to a
// registry), meaning the container cannot be polled and must be skipped.
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
// repository name, as opposed to a bare image ID/digest such as "sha256:3cb6...".
// A bare digest is never a valid registry repository and must never be handed
// to a pull/manifest request. ParseAnyReference resolves a bare digest to a
// digest-only reference (which is not Named), while real references are Named.
func hasRepository(ref string) bool {
	parsed, err := reference.ParseAnyReference(ref)
	if err != nil {
		return false
	}
	_, ok := parsed.(reference.Named)
	return ok
}

// repositoryOf returns the normalized repository name of a reference
// (e.g. "ghcr.io/wow-look-at-my/buildhost" or "docker.io/library/nginx"), or
// "" if ref has no repository (a bare image ID/digest). ParseAnyReference is
// used rather than ParseNormalizedNamed because the latter misparses a bare
// digest like "sha256:abc..." as the repository "sha256" with a tag.
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

// imageIdentity returns a stable, tag-independent identifier for an image: the
// registry manifest digest from RepoDigests for the given repository when
// available, otherwise the image's content-addressable ID. Comparing this
// across a fresh pull reveals whether the tag now points at a different image,
// without requiring the running image to be tagged.
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
