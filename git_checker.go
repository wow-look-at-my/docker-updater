package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gitRefStore tracks the last-seen commit SHA for each container+ref pair.
var gitRefStore = struct {
	sync.Mutex
	refs map[string]string
}{refs: make(map[string]string)}

// checkGitUpdate checks a git remote for new commits on the tracked ref using
// the smart HTTP protocol. Returns the new SHA if changed, empty if not.
func checkGitUpdate(info ContainerInfo) (string, error) {
	if info.GitRepo == "" {
		return "", fmt.Errorf("container %s has git mode but no docker-updater.git-repo label", info.Name)
	}

	sha, err := fetchRemoteRef(info.GitRepo, info.GitRef)
	if err != nil {
		return "", err
	}

	key := info.ID + ":" + info.GitRef

	gitRefStore.Lock()
	prev := gitRefStore.refs[key]
	gitRefStore.refs[key] = sha
	gitRefStore.Unlock()

	// First check — no previous value to compare against.
	if prev == "" {
		return "", nil
	}

	if sha != prev {
		return sha, nil
	}

	return "", nil
}

// fetchRemoteRef queries a git remote via the smart HTTP info/refs endpoint
// and returns the SHA for the given ref.
func fetchRemoteRef(repoURL, ref string) (string, error) {
	url := strings.TrimSuffix(repoURL, "/") + "/info/refs?service=git-upload-pack"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request for %s: %w", url, err)
	}
	req.Header.Set("User-Agent", "docker-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching refs from %s: %w", repoURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	return parseInfoRefs(resp.Body, ref)
}

// parseInfoRefs parses git smart HTTP pkt-line format to find a specific ref.
func parseInfoRefs(r io.Reader, targetRef string) (string, error) {
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := scanner.Text()

		// Skip empty lines and flush packets.
		if line == "" || line == "0000" {
			continue
		}

		// Each pkt-line starts with a 4-char hex length prefix.
		if len(line) < 4 {
			continue
		}

		// Check if this is a valid pkt-line (starts with hex length).
		if _, err := strconv.ParseUint(line[:4], 16, 16); err != nil {
			// Could be a comment or service declaration line, skip.
			continue
		}

		payload := line[4:]

		// Skip the service declaration line.
		if strings.HasPrefix(payload, "# ") {
			continue
		}

		// Payload format: "<sha> <ref>\0<capabilities>" or "<sha> <ref>"
		payload = strings.SplitN(payload, "\x00", 2)[0]
		parts := strings.Fields(payload)
		if len(parts) < 2 {
			continue
		}

		sha := parts[0]
		refName := parts[1]

		// Validate SHA looks like a hex hash.
		if len(sha) < 40 {
			continue
		}
		if _, err := hex.DecodeString(sha[:40]); err != nil {
			continue
		}

		if refName == targetRef {
			return sha, nil
		}
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading refs: %w", err)
	}

	return "", fmt.Errorf("ref %s not found in %s", targetRef, "remote")
}
