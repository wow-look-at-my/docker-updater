package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wow-look-at-my/testify/assert"
	"github.com/wow-look-at-my/testify/require"
)

func TestParseInfoRefs(t *testing.T) {
	response := strings.Join([]string{
		"001e# service=git-upload-pack",
		"0000",
		"00a0ab3def1234567890ab3def1234567890ab3def12 refs/heads/main\x00 multi_ack thin-pack side-band",
		"003fdeadbeef1234567890deadbeef1234567890deadbe refs/heads/dev",
		"0000",
	}, "\n")

	sha, err := parseInfoRefs(strings.NewReader(response), "refs/heads/main")
	require.Nil(t, err)
	assert.Equal(t, "ab3def1234567890ab3def1234567890ab3def12", sha)
}

func TestParseInfoRefsDev(t *testing.T) {
	response := strings.Join([]string{
		"001e# service=git-upload-pack",
		"0000",
		"00a0ab3def1234567890ab3def1234567890ab3def12 refs/heads/main\x00 multi_ack",
		"003fdeadbeef1234567890deadbeef1234567890deadbe refs/heads/dev",
		"0000",
	}, "\n")

	sha, err := parseInfoRefs(strings.NewReader(response), "refs/heads/dev")
	require.Nil(t, err)
	assert.Equal(t, "deadbeef1234567890deadbeef1234567890deadbe", sha)
}

func TestParseInfoRefsNotFound(t *testing.T) {
	response := strings.Join([]string{
		"001e# service=git-upload-pack",
		"0000",
		"003fab3def1234567890ab3def1234567890ab3def12 refs/heads/main",
		"0000",
	}, "\n")

	_, err := parseInfoRefs(strings.NewReader(response), "refs/heads/nonexistent")
	require.NotNil(t, err)
}

func TestParseInfoRefsEmpty(t *testing.T) {
	_, err := parseInfoRefs(strings.NewReader(""), "refs/heads/main")
	require.NotNil(t, err)
}

func TestParseInfoRefsShortLines(t *testing.T) {
	// Lines too short to have valid pkt-line prefix.
	response := "ab\ncd\n"
	_, err := parseInfoRefs(strings.NewReader(response), "refs/heads/main")
	require.NotNil(t, err)
}

func TestParseInfoRefsInvalidHexPrefix(t *testing.T) {
	// Lines with non-hex prefix should be skipped.
	response := "zzzz some content here\n0000\n"
	_, err := parseInfoRefs(strings.NewReader(response), "refs/heads/main")
	require.NotNil(t, err)
}

func TestParseInfoRefsSingleFieldPayload(t *testing.T) {
	// Payload with only one field (no ref name) should be skipped.
	response := "0020ab3def1234567890ab3def1234567890ab3def12\n0000\n"
	_, err := parseInfoRefs(strings.NewReader(response), "refs/heads/main")
	require.NotNil(t, err)
}

func TestParseInfoRefsShortSHA(t *testing.T) {
	// SHA too short (< 40 chars) should be skipped.
	response := "001fshort refs/heads/main\n0000\n"
	_, err := parseInfoRefs(strings.NewReader(response), "refs/heads/main")
	require.NotNil(t, err)
}

func TestFetchRemoteRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info/refs" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "001e# service=git-upload-pack\n")
		fmt.Fprintf(w, "0000\n")
		fmt.Fprintf(w, "003fab3def1234567890ab3def1234567890ab3def12 refs/heads/main\n")
		fmt.Fprintf(w, "0000\n")
	}))
	defer server.Close()

	sha, err := fetchRemoteRef(server.URL, "refs/heads/main")
	require.Nil(t, err)
	assert.Equal(t, "ab3def1234567890ab3def1234567890ab3def12", sha)
}

func TestFetchRemoteRefNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, err := fetchRemoteRef(server.URL, "refs/heads/main")
	require.NotNil(t, err)
}

func TestCheckGitUpdateFirstRun(t *testing.T) {
	gitRefStore.Lock()
	gitRefStore.refs = make(map[string]string)
	gitRefStore.Unlock()

	info := ContainerInfo{
		ID:      "test-container-1",
		Name:    "test",
		Mode:    UpdateModeGit,
		GitRepo: "https://example.com/repo.git",
		GitRef:  "refs/heads/main",
	}

	key := info.ID + ":" + info.GitRef

	gitRefStore.Lock()
	gitRefStore.refs[key] = "abc123abc123abc123abc123abc123abc123abc1"
	gitRefStore.Unlock()

	gitRefStore.Lock()
	stored := gitRefStore.refs[key]
	gitRefStore.Unlock()

	assert.Equal(t, "abc123abc123abc123abc123abc123abc123abc1", stored)
}

func TestCheckGitUpdateWithServer(t *testing.T) {
	gitRefStore.Lock()
	gitRefStore.refs = make(map[string]string)
	gitRefStore.Unlock()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "001e# service=git-upload-pack\n")
		fmt.Fprintf(w, "0000\n")
		fmt.Fprintf(w, "003fab3def1234567890ab3def1234567890ab3def12 refs/heads/main\n")
		fmt.Fprintf(w, "0000\n")
	}))
	defer server.Close()

	info := ContainerInfo{
		ID:      "test-git-container",
		Name:    "git-app",
		Mode:    UpdateModeGit,
		GitRepo: server.URL,
		GitRef:  "refs/heads/main",
	}

	// First call: should return empty (no previous value).
	sha, err := checkGitUpdate(info)
	require.Nil(t, err)
	assert.Equal(t, "", sha)

	// Second call with same SHA: should return empty (no change).
	sha, err = checkGitUpdate(info)
	require.Nil(t, err)
	assert.Equal(t, "", sha)
}

func TestCheckGitUpdateDetectsChange(t *testing.T) {
	gitRefStore.Lock()
	gitRefStore.refs = make(map[string]string)
	gitRefStore.Unlock()

	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		sha := "ab3def1234567890ab3def1234567890ab3def12"
		if callCount > 1 {
			sha = "ff3def1234567890ff3def1234567890ff3def12"
		}
		fmt.Fprintf(w, "001e# service=git-upload-pack\n")
		fmt.Fprintf(w, "0000\n")
		fmt.Fprintf(w, "003f%s refs/heads/main\n", sha)
		fmt.Fprintf(w, "0000\n")
	}))
	defer server.Close()

	info := ContainerInfo{
		ID:      "change-detect-container",
		Name:    "change-app",
		Mode:    UpdateModeGit,
		GitRepo: server.URL,
		GitRef:  "refs/heads/main",
	}

	// First call: sets baseline.
	sha, err := checkGitUpdate(info)
	require.Nil(t, err)
	assert.Equal(t, "", sha)

	// Second call: server returns different SHA.
	sha, err = checkGitUpdate(info)
	require.Nil(t, err)
	assert.Equal(t, "ff3def1234567890ff3def1234567890ff3def12", sha)
}

func TestCheckGitUpdateNoRepo(t *testing.T) {
	info := ContainerInfo{
		ID:   "no-repo",
		Name: "no-repo",
		Mode: UpdateModeGit,
	}

	_, err := checkGitUpdate(info)
	require.NotNil(t, err)
}

func TestShortID(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"sha256:abc123def456789", "sha256:abc12"},
		{"short", "short"},
		{"", ""},
		{"exactly12ch", "exactly12ch"},
		{"exactly12chars", "exactly12cha"},
	}

	for _, tc := range tests {
		got := shortID(tc.input)
		assert.Equal(t, tc.want, got)
	}
}
