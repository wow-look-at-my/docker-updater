package main

import (
	"regexp"
	"strings"
)

// The OCI image annotations an image build stamps. Docker copies an image's
// labels onto every container created from it, so a container's labels carry
// them too.
const (
	labelImageSource   = "org.opencontainers.image.source"
	labelImageRevision = "org.opencontainers.image.revision"
	labelImageVersion  = "org.opencontainers.image.version"
)

var (
	commitSHA = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	fullSHA   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// commitOf reads the commit an image was built from and the page that shows
// it. The revision label names the commit. A version label that is a full SHA
// stands in for it, because the org's publish workflow passes github.sha as
// VERSION. The source label names the repository. Both are empty when the
// image carries no commit. The URL alone is empty when it carries no source.
func commitOf(labels map[string]string) (sha, url string) {
	sha = labels[labelImageRevision]
	if !commitSHA.MatchString(sha) {
		sha = labels[labelImageVersion]
		if !fullSHA.MatchString(sha) {
			return "", ""
		}
	}
	src := strings.TrimSuffix(strings.TrimSuffix(labels[labelImageSource], "/"), ".git")
	if !strings.HasPrefix(src, "https://") {
		return sha, ""
	}
	return sha, src + "/commit/" + sha
}
