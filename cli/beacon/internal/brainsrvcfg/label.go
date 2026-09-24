package brainsrvcfg

// label.go is afferent's copy of the shared scope-label contract (PLAN §1).
// It is a port of brainsrv's internal/ingest/beacon/label.go and must stay
// byte-for-byte equivalent in behaviour: both sides pin it with the same
// test vector, testdata/labels.json, which is a byte-identical copy of
// brainsrv's testdata/beacon/labels.json. Change it only together with the
// canonical vector in brainsrv.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxLabel is the longest label the sanitizer emits; lossy labels are cut to
// lossyKeep bytes plus "_" plus an 8-hex sha256 suffix (55+1+8 = 64).
const (
	maxLabel  = 64
	lossyKeep = 55
)

// NoRepoLabel is the repo label used when a project names no repository,
// path or ID. brainsrv uses the same label for events without a repository.
const NoRepoLabel = "_norepo"

// HarnessLabels maps known harness names (lowercased) to fixed labels, so
// common harnesses read cleanly in scope paths instead of carrying the lossy
// hash suffix (claude-code → claude_code, not claude_code_<h8>). Unknown
// harnesses fall back to Label. Part of the shared test vector.
var HarnessLabels = map[string]string{
	"antigravity_cli":  "antigravity_cli",
	"antigravity-cli":  "antigravity_cli",
	"claude":           "claude",
	"claude_code":      "claude_code",
	"claude-code":      "claude_code",
	"claude_cowork":    "claude_cowork",
	"claude-cowork":    "claude_cowork",
	"cline":            "cline",
	"codex":            "codex",
	"codex_cli":        "codex_cli",
	"codex-cli":        "codex_cli",
	"copilot_cli":      "copilot_cli",
	"copilot-cli":      "copilot_cli",
	"cursor":           "cursor",
	"custom_agent":     "custom_agent",
	"deepseek_harness": "deepseek_harness",
	"devin":            "devin",
	"devin-cli":        "devin_cli",
	"devin-desktop":    "devin_desktop",
	"factory":          "factory",
	"gemini":           "gemini",
	"gemini_cli":       "gemini_cli",
	"gemini-cli":       "gemini_cli",
	"grok":             "grok",
	"hermes":           "hermes",
	"kimi_code":        "kimi_code",
	"kimi-code":        "kimi_code",
	"kiro":             "kiro",
	"muse_code":        "muse_code",
	"openclaw_gateway": "openclaw_gateway",
	"opencode":         "opencode",
	"pi_cli":           "pi_cli",
	"prime_agent":      "prime_agent",
	"vercel_fx":        "vercel_fx",
	"windsurf":         "windsurf",
}

// HarnessLabel returns the scope label for a harness name: the allowlist
// entry when known, else the generic Label rule.
func HarnessLabel(name string) string {
	if l, ok := HarnessLabels[strings.ToLower(strings.TrimSpace(name))]; ok {
		return l
	}
	return Label(name)
}

// RepoLabel returns the scope label for a repository identifier (a remote
// URL, an scp-style SSH remote, or a local path). It is Label; the name
// documents intent at call sites.
func RepoLabel(s string) string { return Label(s) }

// Label sanitizes s into one scope label matching [a-z0-9_]+ (so any output
// is a valid single brainsrv scope segment), per PLAN §1:
//
//	base  = basename(strip ".git", strip URL scheme/host/trailing "/")(s)
//	l     = lowercase(base); non-[a-z0-9_] → "_"; collapse "__+"; trim "_"
//	l     = "_empty" if l == ""
//	lossy = l != lowercase(base) || len(l) > 64
//	lossy → l[:55] + "_" + hex(sha256(base))[:8]
//
// Deterministic and stateless: any sanitization change adds the suffix, so
// distinct inputs like "My.Repo" and "my_repo" never share a label.
func Label(s string) string {
	base := baseName(s)
	lower := strings.ToLower(base)

	var b strings.Builder
	b.Grow(len(lower))
	prevUnderscore := false
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		// Every other rune (including '_' itself) becomes one '_', runs
		// collapsed.
		if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	l := strings.Trim(b.String(), "_")
	if l == "" {
		l = "_empty"
	}
	if l != lower || len(l) > maxLabel {
		sum := sha256.Sum256([]byte(base))
		if len(l) > lossyKeep {
			l = l[:lossyKeep]
		}
		l = l + "_" + hex.EncodeToString(sum[:])[:8]
	}
	return l
}

// baseName reduces a repo identifier to its last path segment: surrounding
// whitespace, a URL scheme+host ("https://host/…"), an scp-style SSH host
// ("git@host:…"), trailing separators and a ".git" suffix are removed.
// A path ending in a bare ".git" directory names its parent.
func baseName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			s = rest[j:]
		} else {
			s = "" // scheme + host only: no repository segment
		}
	} else if at, colon := strings.IndexByte(s, '@'), strings.IndexByte(s, ':'); at >= 0 && colon > at &&
		!strings.ContainsAny(s[:colon], `/\`) {
		s = s[colon+1:] // scp-like user@host:path
	}
	s = strings.TrimRight(s, `/\`)
	seg := s
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		seg = s[i+1:]
	}
	if strings.EqualFold(seg, ".git") {
		// "/src/proj/.git" names proj.
		parent := strings.TrimRight(s[:len(s)-len(seg)], `/\`)
		seg = parent
		if i := strings.LastIndexAny(parent, `/\`); i >= 0 {
			seg = parent[i+1:]
		}
	}
	if len(seg) > 4 && strings.EqualFold(seg[len(seg)-4:], ".git") {
		seg = seg[:len(seg)-4]
	}
	return seg
}
