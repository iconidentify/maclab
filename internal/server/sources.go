package server

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/iconidentify/maclab/internal/api"
)

var reHex40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// parseSource understands what people paste: GitHub repo, branch (tree/...),
// commit and pull request URLs, or any git URL with #ref or @ref.
func parseSource(in string) (api.Source, error) {
	s := api.Source{Input: strings.TrimSpace(in)}
	raw := s.Input
	if raw == "" {
		return s, fmt.Errorf("empty source")
	}
	if strings.HasPrefix(raw, "github.com/") {
		raw = "https://" + raw
	}
	if i := strings.LastIndex(raw, "#"); i > 0 {
		s.Ref = raw[i+1:]
		raw = raw[:i]
	}
	u, err := url.Parse(raw)
	if err == nil && (u.Host == "github.com" || u.Host == "www.github.com") {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) < 2 {
			return s, fmt.Errorf("%q is not a GitHub repository URL", in)
		}
		owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
		s.Repo = "https://github.com/" + owner + "/" + repo
		rest := parts[2:]
		switch {
		case len(rest) >= 2 && (rest[0] == "tree" || rest[0] == "commits"):
			s.Ref = strings.Join(rest[1:], "/") // branch names may contain slashes
		case len(rest) >= 2 && rest[0] == "commit":
			s.Ref = rest[1]
		case len(rest) >= 2 && rest[0] == "pull":
			s.Ref = "refs/pull/" + rest[1] + "/head"
		case len(rest) >= 2 && rest[0] == "releases" && rest[1] == "tag" && len(rest) >= 3:
			s.Ref = strings.Join(rest[2:], "/")
		}
		return s, nil
	}
	// Plain git URL, optionally repo@ref.
	if i := strings.LastIndex(raw, "@"); i > strings.Index(raw, "://")+3 && !strings.Contains(raw[i:], "/") {
		s.Ref = raw[i+1:]
		raw = raw[:i]
	}
	s.Repo = raw
	return s, nil
}

// resolve pins the source to a commit with a single `git ls-remote`.
func resolve(ctx context.Context, s api.Source) (api.Source, error) {
	if reHex40.MatchString(s.Ref) {
		s.SHA = s.Ref
		return s, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	args := []string{"ls-remote", "--", s.Repo}
	want := s.Ref
	if want == "" {
		want = "HEAD"
	}
	cmd := exec.CommandContext(ctx, "git", append(args, want, "refs/heads/"+want, "refs/tags/"+want, "refs/tags/"+want+"^{}")...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return s, fmt.Errorf("git ls-remote %s: %s", s.Repo, msg)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) == 2 {
			refs[f[1]] = f[0]
		}
	}
	for _, name := range []string{want, "refs/heads/" + want, "refs/tags/" + want + "^{}", "refs/tags/" + want} {
		if sha, ok := refs[name]; ok {
			s.SHA = sha
			return s, nil
		}
	}
	if regexp.MustCompile(`^[0-9a-f]{7,39}$`).MatchString(s.Ref) {
		return s, fmt.Errorf("short commit %s: give the full 40-character sha, or a branch", s.Ref)
	}
	return s, fmt.Errorf("%s has no branch or tag %q", s.Repo, want)
}

// shortRepo is owner/repo for GitHub, else the URL.
func shortRepo(repo string) string {
	return strings.TrimPrefix(repo, "https://github.com/")
}
