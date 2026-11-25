package engine

import (
	"strings"
)

// NormalizeMicroVMImagePattern trims and canonicalizes user-entered micro-VM
// image rule text. It returns "" when raw is empty or contains whitespace.
func NormalizeMicroVMImagePattern(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return ""
	}
	base, digest, hasDigest := strings.Cut(s, "@")
	repo, tag, hasTag := splitImageRepoTag(base)
	repo = strings.ToLower(repo)
	var b strings.Builder
	b.WriteString(repo)
	if hasTag {
		b.WriteByte(':')
		b.WriteString(tag)
	}
	if hasDigest {
		b.WriteByte('@')
		b.WriteString(strings.ToLower(digest))
	}
	return b.String()
}

// MatchMicroVMImage reports whether ref matches any pattern in patterns.
//
// Matching rules:
//   - Docker Hub shorthand is canonicalized on both sides ("postgres:16" ==
//     "library/postgres:16" == "docker.io/library/postgres:16").
//   - A repository-only pattern without a tag (e.g. "postgres" or "ghcr.io/org/app")
//     matches any tag or digest of that repository.
//   - A tagged pattern (e.g. "postgres:16") matches only that tag; an untagged
//     candidate reference defaults to ":latest".
//   - Wildcards ('*') match any sequence of characters in repository or tag
//     segments (e.g. "ghcr.io/org/*", "postgres:16*").
func MatchMicroVMImage(patterns []string, ref string) bool {
	if len(patterns) == 0 {
		return false
	}
	cRepo, cTag, cDigest := canonicalImageParts(ref, true)
	if cRepo == "" {
		return false
	}
	for _, pat := range patterns {
		if matchMicroVMImageOne(pat, cRepo, cTag, cDigest) {
			return true
		}
	}
	return false
}

func matchMicroVMImageOne(pattern, cRepo, cTag, cDigest string) bool {
	pRepo, pTag, pDigest := canonicalImageParts(pattern, false)
	if pRepo == "" {
		return false
	}
	if pRepo == "*" && pTag == "" && pDigest == "" {
		return true
	}
	if pDigest != "" {
		return cDigest != "" && globMatch(pRepo, cRepo) && globMatch(pDigest, cDigest)
	}
	if !globMatch(pRepo, cRepo) {
		return false
	}
	if pTag == "" {
		return true
	}
	if cTag == "" {
		return false
	}
	return globMatch(pTag, cTag)
}

func splitImageRepoTag(s string) (repo, tag string, hasTag bool) {
	lastSlash := strings.LastIndexByte(s, '/')
	lastColon := strings.LastIndexByte(s, ':')
	if lastColon > lastSlash {
		return s[:lastColon], s[lastColon+1:], true
	}
	return s, "", false
}

func canonicalImageParts(raw string, defaultLatest bool) (repo, tag, digest string) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return "", "", ""
	}
	base, dig, hasDigest := strings.Cut(s, "@")
	if hasDigest {
		digest = strings.ToLower(strings.TrimSpace(dig))
	}
	r, t, hasTag := splitImageRepoTag(base)
	r = strings.ToLower(strings.TrimSpace(r))
	if r == "" {
		return "", "", ""
	}
	if r != "*" {
		first, rest, hasSlash := strings.Cut(r, "/")
		switch {
		case !hasSlash:
			r = "docker.io/library/" + r
		case first == "docker.io" || first == "index.docker.io" || first == "registry-1.docker.io":
			if !strings.Contains(rest, "/") {
				r = "docker.io/library/" + rest
			} else {
				r = "docker.io/" + rest
			}
		case strings.ContainsAny(first, ".:") || first == "localhost":
			// Explicit registry host; keep as-is.
		default:
			r = "docker.io/" + r
		}
	}
	if hasTag {
		tag = t
	} else if defaultLatest && !hasDigest {
		tag = "latest"
	}
	return r, tag, digest
}

// globMatch matches s against pattern where '*' matches zero or more of any
// rune (including '/').
func globMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.ContainsRune(pattern, '*') {
		return pattern == s
	}
	p := []rune(pattern)
	t := []rune(s)
	pi, ti := 0, 0
	starIdx, matchIdx := -1, 0
	for ti < len(t) {
		if pi < len(p) && p[pi] == t[ti] {
			pi++
			ti++
		} else if pi < len(p) && p[pi] == '*' {
			starIdx = pi
			matchIdx = ti
			pi++
		} else if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			ti = matchIdx
		} else {
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
