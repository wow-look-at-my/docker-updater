package main

import (
	"github.com/wow-look-at-my/go-containers/set"
	"strings"
)

// parseBaseImageFromDockerfile extracts the base image of the *final* build
// stage from a Dockerfile's text.
//
// A multi-stage Dockerfile has several FROM lines; only the final stage
// produces the image the container actually runs, so that is the one whose base
// must be watched for updates. We therefore return the FROM of the last stage,
// resolving the two ways a later stage can reference an earlier one:
//
//   - `FROM <image> AS <name>` names a stage. A later `FROM <name>` that refers
//     to an earlier named stage is NOT a registry image -- it is resolved back
//     to that stage's own base, transitively. Watching `<name>` (a build-local
//     alias) would be meaningless.
//   - `--platform=...` and other flags on the FROM line are ignored; the image
//     token is the first non-flag argument.
//
// Comments (lines starting with #, including the `# syntax=` / `# escape=`
// parser directives) and blank lines are skipped, and line continuations
// (trailing backslash) are joined so a FROM split across lines is still seen.
//
// Returns "" if no FROM is found (an empty or comment-only Dockerfile) or the
// final stage's base resolves to nothing. `scratch` is returned verbatim; the
// caller treats a non-registry base (scratch, or an unresolved stage alias) as
// "cannot watch" and skips.
func parseBaseImageFromDockerfile(dockerfile string) string {
	type stage struct {
		name string // lowercased AS name, "" if unnamed
		base string // the image/alias the FROM referenced
	}
	var stages []stage

	for _, line := range joinContinuations(dockerfile) {
		line = stripInlineComment(line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}

		// Drop flags like --platform=linux/amd64; the image is the first
		// non-flag token after FROM.
		args := fields[1:]
		base := ""
		var rest []string
		for i, a := range args {
			if strings.HasPrefix(a, "--") {
				continue
			}
			base = a
			rest = args[i+1:]
			break
		}
		if base == "" {
			continue
		}

		name := ""
		// Look for `AS <name>` in the remaining tokens.
		for i := 0; i+1 < len(rest); i++ {
			if strings.EqualFold(rest[i], "AS") {
				name = strings.ToLower(rest[i+1])
				break
			}
		}
		stages = append(stages, stage{name: strings.ToLower(name), base: base})
	}

	if len(stages) == 0 {
		return ""
	}

	// Resolve the final stage's base, following AS-name references back to the
	// earlier stage they point at (transitively), so a `FROM builder` final
	// stage watches builder's own registry base rather than the alias.
	byName := make(map[string]stage, len(stages))
	for _, s := range stages {
		if s.name != "" {
			byName[s.name] = s
		}
	}

	base := stages[len(stages)-1].base
	seen := set.New[string]()
	for {
		key := strings.ToLower(base)
		if seen.Contains(key) {
			// Cycle (malformed Dockerfile); stop and return what we have.
			return base
		}
		seen.Add(key)
		ref, ok := byName[key]
		if !ok {
			return base
		}
		base = ref.base
	}
}

// joinContinuations folds Dockerfile line continuations (a line ending in a
// backslash continues onto the next) into single logical lines.
func joinContinuations(text string) []string {
	var out []string
	var buf strings.Builder
	cont := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasSuffix(trimmed, "\\") {
			buf.WriteString(strings.TrimSuffix(trimmed, "\\"))
			buf.WriteByte(' ')
			cont = true
			continue
		}
		buf.WriteString(line)
		out = append(out, buf.String())
		buf.Reset()
		cont = false
	}
	if cont || buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

// stripInlineComment removes a Dockerfile comment. A `#` only starts a comment
// at the start of a line (after optional whitespace); Dockerfile does not honor
// trailing inline comments mid-instruction, so we only drop whole-line
// comments and parser directives.
func stripInlineComment(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return ""
	}
	return line
}
