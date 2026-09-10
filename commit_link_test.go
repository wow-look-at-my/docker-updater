package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCommitOf(t *testing.T) {
	const sha = "3d93f61589ca1c34e673d08be7fc189f851156c3"
	cases := []struct {
		name    string
		labels  map[string]string
		wantSHA string
		wantURL string
	}{
		{
			name: "revision and source",
			labels: map[string]string{
				labelImageRevision: sha,
				labelImageSource:   "https://github.com/wow-look-at-my/docker-updater",
			},
			wantSHA: sha,
			wantURL: "https://github.com/wow-look-at-my/docker-updater/commit/" + sha,
		},
		{
			name: "a .git source and a trailing slash both resolve",
			labels: map[string]string{
				labelImageRevision: "abc1234",
				labelImageSource:   "https://github.com/wow-look-at-my/x.git/",
			},
			wantSHA: "abc1234",
			wantURL: "https://github.com/wow-look-at-my/x/commit/abc1234",
		},
		{
			name: "a full-SHA version stands in for the revision",
			labels: map[string]string{
				labelImageVersion: sha,
				labelImageSource:  "https://github.com/wow-look-at-my/docker-updater",
			},
			wantSHA: sha,
			wantURL: "https://github.com/wow-look-at-my/docker-updater/commit/" + sha,
		},
		{
			name:   "a version that is not a SHA is not a commit",
			labels: map[string]string{labelImageVersion: "1.2.3", labelImageSource: "https://github.com/a/b"},
		},
		{
			name:    "a commit with no source has no page",
			labels:  map[string]string{labelImageRevision: sha},
			wantSHA: sha,
		},
		{
			name:    "a source that is not https has no page",
			labels:  map[string]string{labelImageRevision: sha, labelImageSource: "git@github.com:a/b.git"},
			wantSHA: sha,
		},
		{name: "no labels"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sha, url := commitOf(tc.labels)
			assert.Equal(t, tc.wantSHA, sha)
			assert.Equal(t, tc.wantURL, url)
		})
	}
}
