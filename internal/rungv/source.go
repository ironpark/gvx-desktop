package rungv

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// The default source of gvx: where it is cloned from, the branch built, and
// the GitHub API host that tells of it; a test points the last at a server of
// its own.
const (
	SourceURL    = "https://github.com/gfire-sigs/gvx"
	SourceBranch = "main"
)

var sourceAPIBase = "https://api.github.com"

// Source names the git repository and branch BuildFromSource clones and
// builds. Empty fields fall back to SourceURL and SourceBranch.
type Source struct {
	Repo   string
	Branch string
}

// BranchName is the branch or tag to build, defaulting to SourceBranch.
func (s Source) BranchName() string {
	if b := strings.TrimSpace(s.Branch); b != "" {
		return b
	}
	return SourceBranch
}

// CloneURL is the repository passed to git clone and git remote, defaulting
// to SourceURL. A bare GitHub "owner/repo" expands to its HTTPS URL, and a
// leading "~" expands to the user's home directory for local checkouts.
func (s Source) CloneURL() string {
	r := strings.TrimSpace(s.Repo)
	if r == "" {
		return SourceURL
	}
	if strings.HasPrefix(r, "~") {
		return ExpandHome(r)
	}
	if strings.HasPrefix(r, "github.com/") {
		return "https://" + r
	}
	if isGitHubSlug(r) {
		return "https://github.com/" + r
	}
	return r
}

// GitHubSlug returns "owner/repo" when the source points at github.com.
func (s Source) GitHubSlug() (string, bool) {
	r := strings.TrimSpace(s.Repo)
	if r == "" {
		return "gfire-sigs/gvx", true
	}
	if isGitHubSlug(r) {
		return strings.TrimSuffix(r, ".git"), true
	}
	if rest, ok := strings.CutPrefix(r, "git@github.com:"); ok {
		slug := strings.TrimSuffix(strings.Trim(rest, "/"), ".git")
		if isGitHubSlug(slug) {
			return slug, true
		}
		return "", false
	}
	if strings.HasPrefix(r, "github.com/") {
		r = "https://" + r
	}
	u, err := url.Parse(r)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return "", false
	}
	slug := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if isGitHubSlug(slug) {
		return slug, true
	}
	return "", false
}

// isGitHubSlug reports whether s looks like "owner/repo" with no scheme,
// port or path prefix.
func isGitHubSlug(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.ContainsAny(s, ":\\ \t") {
		return false
	}
	owner, repo, ok := strings.Cut(s, "/")
	return ok && owner != "" && repo != "" && !strings.Contains(repo, "/")
}

// DisplayRepo is a short label for the repository: "owner/repo" on GitHub,
// or the clone URL otherwise.
func (s Source) DisplayRepo() string {
	if slug, ok := s.GitHubSlug(); ok {
		return slug
	}
	return s.CloneURL()
}

// CommitsURL is a web page listing commits of the branch, or "" when the
// source has none (such as a local path or SSH URL on another host).
func (s Source) CommitsURL() string {
	if slug, ok := s.GitHubSlug(); ok {
		return "https://github.com/" + slug + "/commits/" + escapeRef(s.BranchName())
	}
	u := s.CloneURL()
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return strings.TrimSuffix(u, ".git")
	}
	return ""
}

// IsDefault reports whether s uses the default repository and branch.
func (s Source) IsDefault() bool {
	return strings.TrimSuffix(s.CloneURL(), ".git") == SourceURL && s.BranchName() == SourceBranch
}

// escapeRef escapes each segment of a git ref while preserving slashes.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Head is the newest commit of the source's branch.
type Head struct {
	SHA     string
	Subject string // the first line of its message
	// Ahead is how many commits the branch is past the one asked about,
	// or -1 when it could not be determined.
	Ahead int
}

// SourceHead asks for the head of src's branch, and how far it is past
// installed, the commit of the rungv in use; installed may be empty. For a
// GitHub repository it asks GitHub's API; for any other URL or local path it
// asks git ls-remote.
func SourceHead(ctx context.Context, src Source, installed string) (Head, error) {
	branch := src.BranchName()
	if slug, ok := src.GitHubSlug(); ok {
		api := sourceAPIBase + "/repos/" + slug
		ref := escapeRef(branch)
		var commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		}
		if err := getJSON(ctx, api+"/commits/"+ref, &commit); err != nil {
			return Head{}, err
		}
		if commit.SHA == "" {
			return Head{}, errors.New("the branch has no head")
		}
		h := Head{SHA: commit.SHA, Ahead: -1}
		h.Subject, _, _ = strings.Cut(commit.Commit.Message, "\n")
		switch {
		case installed == commit.SHA:
			h.Ahead = 0
		case installed != "":
			var cmp struct {
				Ahead int `json:"ahead_by"`
			}
			if err := getJSON(ctx, api+"/compare/"+url.PathEscape(installed)+"..."+ref, &cmp); err == nil {
				h.Ahead = cmp.Ahead
			}
		}
		return h, nil
	}
	return lsRemoteHead(ctx, src.CloneURL(), branch, installed)
}

func lsRemoteHead(ctx context.Context, repo, branch, installed string) (Head, error) {
	git, err := FindTool("git")
	if err != nil {
		return Head{}, fmt.Errorf("%w: %w", ErrNoToolchain, err)
	}
	cmd := ToolCommand(ctx, git, "ls-remote", repo, "refs/heads/"+branch, "refs/tags/"+branch, branch)
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && len(exit.Stderr) > 0 {
			return Head{}, fmt.Errorf("git ls-remote: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return Head{}, fmt.Errorf("git ls-remote: %w", err)
	}
	var sha string
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] != "" {
			sha = f[0]
			if f[1] == "refs/heads/"+branch || f[1] == "refs/tags/"+branch {
				break
			}
		}
	}
	if sha == "" {
		return Head{}, fmt.Errorf("branch or tag %q not found in %s", branch, repo)
	}
	ahead := -1
	if installed == sha {
		ahead = 0
	}
	return Head{SHA: sha, Ahead: ahead}, nil
}

func getJSON(ctx context.Context, url string, v any) error {
	resp, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}

// ErrNoToolchain reports that git or Go, which a build from source needs,
// was not found.
var ErrNoToolchain = errors.New("building from source needs git and Go")

// ErrToolNotFound reports a command that is nowhere FindTool looks.
var ErrToolNotFound = errors.New("command not found")

// FindTool looks for a command where a terminal would find it. An app
// started from the Dock or a launcher has a short PATH, without the
// directories a shell's profile adds — Homebrew's among them — so the
// usual homes of developer tools are looked in too, and at last the login
// shell is asked. Asking the shell takes a while, and its answer is kept
// for askAgain, so that a screen may call FindTool each time it is drawn.
func FindTool(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	file := name
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	for _, d := range toolDirs() {
		p := filepath.Join(d, file)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p := askShell(name); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("%w: %s", ErrToolNotFound, name)
}

// toolDirs are the directories developer tools are installed in that the
// PATH an app is started with may leave out. On macOS that is most of
// them: an app started from the Dock has the system's PATH alone. Windows
// and the Linux desktops hand an app the user's PATH, so there these only
// catch what was installed since the session began, or by a tool that
// leaves the PATH to the user's shell profile.
func toolDirs() []string {
	var dirs []string
	home, _ := os.UserHomeDir()
	if home != "" {
		dirs = append(dirs,
			filepath.Join(home, "go", "bin"), filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"),
			filepath.Join(home, ".nix-profile", "bin"), filepath.Join(home, "sdk", "go", "bin"),
			// Docker Desktop, Rancher Desktop and OrbStack put their CLIs here.
			filepath.Join(home, ".docker", "bin"), filepath.Join(home, ".rd", "bin"), filepath.Join(home, ".orbstack", "bin"))
	}
	if runtime.GOOS == "windows" {
		if home != "" {
			dirs = append(dirs, filepath.Join(home, "scoop", "shims"))
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			dirs = append(dirs, filepath.Join(local, "Microsoft", "WinGet", "Links")) // winget's portable packages
		}
		if data := os.Getenv("ProgramData"); data != "" {
			dirs = append(dirs, filepath.Join(data, "chocolatey", "bin"))
		}
		for _, v := range []string{"ProgramFiles", "ProgramW6432"} {
			if pf := os.Getenv(v); pf != "" {
				dirs = append(dirs, filepath.Join(pf, "Go", "bin"), filepath.Join(pf, "Git", "cmd"),
					filepath.Join(pf, "Docker", "Docker", "resources", "bin"))
			}
		}
		return dirs
	}
	if home != "" {
		dirs = append(dirs, filepath.Join("/etc/profiles/per-user", filepath.Base(home), "bin")) // nix-darwin, home-manager
	}
	return append(dirs, "/opt/homebrew/bin", "/usr/local/bin", "/usr/local/go/bin",
		"/home/linuxbrew/.linuxbrew/bin", "/run/current-system/sw/bin", "/nix/var/nix/profiles/default/bin", "/snap/bin",
		"/Applications/Docker.app/Contents/Resources/bin")
}

// askAgain is how long the login shell's answer about a command stands.
const askAgain = 30 * time.Second

var shellAnswers = struct {
	sync.Mutex
	at   map[string]time.Time
	path map[string]string
}{at: map[string]time.Time{}, path: map[string]string{}}

// askShell asks the user's login shell where a command is, which knows
// the PATH their profile sets up; "" when it does not know either.
func askShell(name string) string {
	shell := os.Getenv("SHELL")
	if runtime.GOOS == "windows" || shell == "" || strings.ContainsAny(name, " \t'\"$`;&|") {
		return ""
	}
	shellAnswers.Lock()
	defer shellAnswers.Unlock()
	if at, ok := shellAnswers.at[name]; ok && time.Since(at) < askAgain {
		return shellAnswers.path[name]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell, "-lc", "command -v "+name).Output()
	p := strings.TrimSpace(lastLine(string(out)))
	if err != nil || !filepath.IsAbs(p) {
		p = ""
	}
	shellAnswers.at[name], shellAnswers.path[name] = time.Now(), p
	return p
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// BuildFromSource builds rungv from the head of src's branch and puts it at
// target, replacing what is there. dir is a directory of the caller's to
// keep the clone in between builds: it is fetched into, and whatever else is
// in it is thrown away. phase hears what the build is about to do, and line
// each line git and Go print. It returns the commit built. On macOS the
// caller still signs the binary, with Sign.
func BuildFromSource(ctx context.Context, src Source, dir, target string, phase func(Phase), line func(string)) (string, error) {
	cloneURL := src.CloneURL()
	branch := src.BranchName()
	git, err := FindTool("git")
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoToolchain, err)
	}
	goBin, err := FindTool("go")
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoToolchain, err)
	}
	run := func(in string, env []string, name string, args ...string) error {
		line("$ " + filepath.Base(name) + " " + strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = in
		cmd.Env = append(os.Environ(), env...)
		hideWindow(cmd)
		out, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return err
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			// git redraws its progress in place with carriage returns.
			text := sc.Text()
			if i := strings.LastIndexByte(strings.TrimRight(text, "\r"), '\r'); i >= 0 {
				text = text[i+1:]
			}
			line(strings.TrimRight(text, "\r"))
		}
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("%s %s: %w", filepath.Base(name), args[0], err)
		}
		return nil
	}

	phase(PhaseFetch)
	head := "FETCH_HEAD"
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		// Not a clone, or a broken one: start over. The whole history comes
		// along, since Go names the build after the tags it finds in it.
		if err := os.RemoveAll(dir); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if err := run(filepath.Dir(dir), nil, git, "clone", "--progress", "--branch", branch, cloneURL, dir); err != nil {
			return "", err
		}
		head = "HEAD"
	} else {
		cur, rerr := ToolCommand(ctx, git, "-C", dir, "remote", "get-url", "origin").Output()
		if rerr != nil {
			if err := run(dir, nil, git, "remote", "add", "origin", cloneURL); err != nil {
				return "", err
			}
		} else if strings.TrimSpace(string(cur)) != cloneURL {
			if err := run(dir, nil, git, "remote", "set-url", "origin", cloneURL); err != nil {
				return "", err
			}
		}
		if err := run(dir, nil, git, "fetch", "--progress", "--force", "--tags", "origin", branch); err != nil {
			return "", err
		}
	}
	// The clone is this function's own, so that nothing in it is worth
	// keeping: a tree with changes would also be built as +dirty.
	if err := run(dir, nil, git, "checkout", "--force", "--detach", head); err != nil {
		return "", err
	}
	if err := run(dir, nil, git, "clean", "-fdx"); err != nil {
		return "", err
	}
	revCmd := exec.CommandContext(ctx, git, "-C", dir, "rev-parse", "HEAD")
	hideWindow(revCmd)
	sha, err := revCmd.Output()
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(sha))

	phase(PhaseBuild)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	fresh := filepath.Join(filepath.Dir(target), ".rungv-built-"+commit[:min(12, len(commit))])
	defer os.Remove(fresh)
	// Go and git are on the PATH of the build whatever this process's is.
	path := "PATH=" + filepath.Dir(goBin) + string(os.PathListSeparator) + filepath.Dir(git) + string(os.PathListSeparator) + os.Getenv("PATH")
	pkg := sourcePackage(dir)
	script := filepath.Join(dir, "scripts", "repro-build.sh")
	if _, serr := os.Stat(script); serr == nil && runtime.GOOS != "windows" {
		// gvx's own way to build, which pins what makes a build repeatable.
		err = run(dir, []string{path, "GO=" + goBin}, "/bin/sh", script, fresh, pkg)
	} else {
		err = run(dir, []string{path, "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS="}, goBin, "build",
			"-mod=readonly", "-trimpath", "-buildvcs=true", "-ldflags=-s -w -buildid=", "-o", fresh, pkg)
	}
	if err != nil {
		return "", err
	}
	if err := os.Chmod(fresh, 0o755); err != nil {
		return "", err
	}
	phase(PhaseInstall)
	if err := replaceFile(fresh, target); err != nil {
		return commit, err
	}
	ForgetFeatures(target)
	if abs, err := filepath.Abs(target); err == nil && abs != target {
		ForgetFeatures(abs)
	}
	return commit, nil
}

// sourcePackage is the package of rungv's command in the gvx checkout at
// dir: ./cmd/rungvx since gvx v0.23, ./cmd/rungv before.
func sourcePackage(dir string) string {
	if st, err := os.Stat(filepath.Join(dir, "cmd", programX)); err == nil && st.IsDir() {
		return "./cmd/" + programX
	}
	return "./cmd/" + program
}

// Phase is a step of BuildFromSource.
type Phase int

const (
	PhaseFetch Phase = iota + 1
	PhaseBuild
	PhaseInstall
)
