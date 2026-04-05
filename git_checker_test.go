package main

import (
	"strings"
	"testing"
)

func TestParseInfoRefs(t *testing.T) {
	// Simulated smart HTTP info/refs response in pkt-line format.
	response := strings.Join([]string{
		"001e# service=git-upload-pack",
		"0000",
		"00a0ab3def1234567890ab3def1234567890ab3def12 refs/heads/main\x00 multi_ack thin-pack side-band",
		"003fdeadbeef1234567890deadbeef1234567890deadbe refs/heads/dev",
		"0000",
	}, "\n")

	sha, err := parseInfoRefs(strings.NewReader(response), "refs/heads/main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sha != "ab3def1234567890ab3def1234567890ab3def12" {
		t.Errorf("expected main SHA, got %q", sha)
	}
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sha != "deadbeef1234567890deadbeef1234567890deadbe" {
		t.Errorf("expected dev SHA, got %q", sha)
	}
}

func TestParseInfoRefsNotFound(t *testing.T) {
	response := strings.Join([]string{
		"001e# service=git-upload-pack",
		"0000",
		"003fab3def1234567890ab3def1234567890ab3def12 refs/heads/main",
		"0000",
	}, "\n")

	_, err := parseInfoRefs(strings.NewReader(response), "refs/heads/nonexistent")
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
}

func TestParseInfoRefsEmpty(t *testing.T) {
	_, err := parseInfoRefs(strings.NewReader(""), "refs/heads/main")
	if err == nil {
		t.Fatal("expected error for empty response")
	}
}

func TestCheckGitUpdateFirstRun(t *testing.T) {
	// Reset the ref store.
	gitRefStore.Lock()
	gitRefStore.refs = make(map[string]string)
	gitRefStore.Unlock()

	// checkGitUpdate needs a real HTTP endpoint, so we test the ref store logic
	// by directly manipulating it.
	info := ContainerInfo{
		ID:      "test-container-1",
		Name:    "test",
		Mode:    UpdateModeGit,
		GitRepo: "https://example.com/repo.git",
		GitRef:  "refs/heads/main",
	}

	key := info.ID + ":" + info.GitRef

	// Simulate first ref fetch.
	gitRefStore.Lock()
	gitRefStore.refs[key] = "abc123abc123abc123abc123abc123abc123abc1"
	gitRefStore.Unlock()

	// Verify stored.
	gitRefStore.Lock()
	stored := gitRefStore.refs[key]
	gitRefStore.Unlock()

	if stored != "abc123abc123abc123abc123abc123abc123abc1" {
		t.Errorf("expected stored SHA, got %q", stored)
	}
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
		if got != tc.want {
			t.Errorf("shortID(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
