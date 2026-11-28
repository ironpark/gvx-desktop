// Package rungv drives the gvx command line, rungv, as a child process.
//
// The desktop app never links gvx's Go SDK: gvx.New locks down the calling
// process before the guest starts, which would leave the GUI unable to open
// files, sockets or windows. Every sandbox therefore runs as a rungv process
// of its own, and the management commands are read through --format json.
package rungv

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrNotFound reports that no rungvx binary could be located.
var ErrNotFound = errors.New("rungvx binary not found")

// rungv's program names. gvx calls it rungvx since v0.23
// (gfire-sigs/gvx#250): its binary, its release archives and the start of
// what it prints. It was rungv before.
const (
	program  = "rungv"
	programX = "rungvx"
)

var programs = [...]string{program, programX}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// BinaryName is the file name this app gives rungv on this host, where it
// installs it and looks for it first. It stays rungv for a rungvx too, so
// the paths the app already knows (Settings, ~/.gvx/bin, the terminal's
// PATH) keep naming the binary in use.
func BinaryName() string { return exeName(program) }

// binaryNames are the file names rungv goes by: the app's own, then
// rungvx, the name gvx's releases and builds give it.
func binaryNames() []string { return []string{exeName(program), exeName(programX)} }

// CutProgram reports whether line is one of rungv's own messages, its
// program name (rungv or rungvx) followed by sep, and returns the rest.
func CutProgram(line, sep string) (string, bool) {
	for _, p := range programs {
		if rest, ok := strings.CutPrefix(line, p+sep); ok {
			return rest, true
		}
	}
	return "", false
}

// Candidates lists the places Locate looks, in order. preferred, when set,
// comes first.
func Candidates(preferred string) []string {
	var out []string
	add := func(p string) {
		if p != "" {
			out = append(out, p)
		}
	}
	inDir := func(dir string) {
		for _, name := range binaryNames() {
			add(filepath.Join(dir, name))
		}
	}
	add(ExpandHome(preferred))
	add(os.Getenv("GVX_RUNGVX"))
	add(os.Getenv("GVX_RUNGV"))
	for _, name := range binaryNames() {
		if p, err := exec.LookPath(name); err == nil {
			add(p)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		inDir(filepath.Join(home, ".gvx", "bin"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		inDir(dir)
		inDir(filepath.Join(dir, "..", "Resources")) // macOS .app bundle
		inDir(filepath.Join(dir, "bin"))
	}
	if wd, err := os.Getwd(); err == nil {
		inDir(filepath.Join(wd, "bin")) // development checkout
	}
	if runtime.GOOS != "windows" {
		inDir("/opt/homebrew/bin")
		inDir("/usr/local/bin")
	}
	return out
}

// Locate returns the first executable candidate.
func Locate(preferred string) (string, error) {
	for _, p := range Candidates(preferred) {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if runtime.GOOS != "windows" && st.Mode()&0o111 == 0 {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return filepath.Clean(p), nil
	}
	return "", ErrNotFound
}

// HomeDir is the user's home directory as ExpandHome writes it. The
// screens drawn with made-up state put a made-up one here, so that the
// pictures do not name whoever drew them.
var HomeDir = os.UserHomeDir

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := HomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

// Client runs rungv commands. The zero value is not usable; set Path.
type Client struct {
	Path     string
	CacheDir string // optional -cache-dir for every command that takes it
	// VolumeDir is the optional directory of named volumes. On rungvx v0.24+,
	// Command passes it as -volume-dir (which overrides -cache-dir) for
	// commands that accept it, and always sets VolumeDirEnv for compatibility
	// with earlier builds.
	VolumeDir string
}

// VolumeDirEnv is the variable through which rungv takes the directory of
// named volumes when no -cache-dir is given. A rungv that predates it ignores
// it.
const VolumeDirEnv = "GVX_VOLUME_DIR"

// Error is a failed rungv command with what it wrote to stderr.
type Error struct {
	Args   []string
	Code   int
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	// rungv prefixes its messages with the command name; keep the last line,
	// which carries the cause.
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
		msg = strings.TrimSpace(msg[i+1:])
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// cacheFlagCommands take -cache-dir.
var cacheFlagCommands = map[string]bool{
	"run": true, "restore": true, "pull": true, "push": true, "images": true, "inspect": true,
	"rmi": true, "tag": true, "save": true, "load": true, "image": true,
	"volume": true, "system": true, "info": true,
}

// volumeFlagCommands take -volume-dir (rungvx v0.24+). mount add is handled in
// AddMount because -volume-dir applies only to volume mounts, not mount ls/rm.
var volumeFlagCommands = map[string]bool{
	"run": true, "restore": true, "volume": true, "system": true, "info": true,
}

func (c *Client) argsFor(args []string, withVolumeDir bool) []string {
	if len(args) == 0 {
		return args
	}
	addCache := c.CacheDir != "" && cacheFlagCommands[args[0]]
	addVol := withVolumeDir && c.VolumeDir != "" && volumeFlagCommands[args[0]]
	if !addCache && !addVol {
		return args
	}
	out := make([]string, 0, len(args)+4)
	// Group commands take the flag after their verb.
	n := 1
	if (args[0] == "image" || args[0] == "volume" || args[0] == "system") && len(args) > 1 {
		n = 2
	}
	out = append(out, args[:n]...)
	if addCache {
		out = append(out, "-cache-dir", c.CacheDir)
	}
	if addVol {
		out = append(out, "-volume-dir", c.VolumeDir)
	}
	return append(out, args[n:]...)
}

// Args returns the full argument vector for a command, adding -cache-dir
// where the command accepts it.
func (c *Client) Args(args ...string) []string {
	return c.argsFor(args, false)
}

// Command builds an exec.Cmd for rungv with args. When VolumeDir is set, it
// passes -volume-dir for commands that accept it on rungvx v0.24+ and also
// sets VolumeDirEnv so older builds see it when CacheDir is unset.
func (c *Client) Command(ctx context.Context, args ...string) *exec.Cmd {
	withVol := c.VolumeDir != "" && len(args) > 0 && volumeFlagCommands[args[0]] && c.HasRunFlag(ctx, "volume-dir")
	cmd := exec.CommandContext(ctx, c.Path, c.argsFor(args, withVol)...)
	if c.VolumeDir != "" {
		cmd.Env = append(os.Environ(), VolumeDirEnv+"="+c.VolumeDir)
	}
	hideWindow(cmd)
	return cmd
}

// CacheVolumeDir reports whether dir lies in a cache folder that the host may
// purge. macOS deletes the content of ~/Library/Caches when the disk runs low,
// and named volumes there can vanish with their data.
func CacheVolumeDir(goos, home, dir string) bool {
	if goos != "darwin" || home == "" || dir == "" {
		return false
	}
	caches := filepath.Join(home, "Library", "Caches")
	rel, err := filepath.Rel(caches, filepath.Clean(dir))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ToolCommand builds an exec.Cmd for another program, such as the docker
// CLI, as Command does for rungv: with no console window on Windows.
func ToolCommand(ctx context.Context, path string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	hideWindow(cmd)
	return cmd
}

func (c *Client) output(ctx context.Context, args ...string) ([]byte, error) {
	if c.Path == "" {
		return nil, ErrNotFound
	}
	cmd := c.Command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		e := &Error{Args: args, Stderr: stderr.String(), Err: err}
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			e.Code = exit.ExitCode()
		}
		return stdout.Bytes(), e
	}
	return stdout.Bytes(), nil
}

// runFlags, restoreFlags, and checkpointFlags hold, by the path of a rungv,
// the usage text of its run, restore, and checkpoint commands.
var (
	runFlags        sync.Map
	restoreFlags    sync.Map
	checkpointFlags sync.Map
)

func commandUsage(ctx context.Context, cache *sync.Map, subcmd, binPath string) (string, bool) {
	if binPath == "" {
		return "", false
	}
	usage, ok := cache.Load(binPath)
	if !ok {
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(runCtx, binPath, subcmd, "-help")
		cmd.WaitDelay = time.Second // a rungv that does not print its usage and leave
		hideWindow(cmd)
		out, err := cmd.CombinedOutput() // the usage may come with a failing exit
		if runCtx.Err() != nil {
			return "", false
		}
		if len(out) == 0 && err != nil {
			// Skip caching launch failures (ENOENT, EACCES) and signal kills
			// (e.g., macOS arm64 SIGKILL on an unsigned binary), while still
			// caching normal exits (such as exit 2 with empty output).
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() < 0 {
				return "", false
			}
		}
		usage = string(out)
		cache.Store(binPath, usage)
	}
	return usage.(string), true
}

func usageHasFlag(usage, name string) bool {
	for line := range strings.SplitSeq(usage, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "-"+name && strings.HasPrefix(line, "  -") {
			return true
		}
	}
	return false
}

func hasCommandFlag(ctx context.Context, cache *sync.Map, subcmd, binPath, name string) bool {
	usage, ok := commandUsage(ctx, cache, subcmd, binPath)
	if !ok {
		return false
	}
	return usageHasFlag(usage, name)
}

// HasRunUsage reports whether this rungv's run -help output contains substr.
// The usage text is read once for each binary and shared with HasRunFlag.
func (c *Client) HasRunUsage(ctx context.Context, substr string) bool {
	if c == nil || substr == "" {
		return false
	}
	usage, ok := commandUsage(ctx, &runFlags, "run", c.Path)
	return ok && strings.Contains(usage, substr)
}

// HasRunFlag reports whether this rungv's run command takes the flag name,
// written without its dash: an older rungv refuses a run with a flag it
// does not know. The usage text is read once for each binary.
func (c *Client) HasRunFlag(ctx context.Context, name string) bool {
	if c == nil {
		return false
	}
	return hasCommandFlag(ctx, &runFlags, "run", c.Path, name)
}

// HasRestoreFlag reports whether this rungv's restore command takes the flag
// name, written without its dash. The usage text is read once for each binary.
func (c *Client) HasRestoreFlag(ctx context.Context, name string) bool {
	if c == nil {
		return false
	}
	return hasCommandFlag(ctx, &restoreFlags, "restore", c.Path, name)
}

// HasCheckpointFlag reports whether this rungv's checkpoint command takes the
// flag name, written without its dash (e.g. "o" for streamed gvx-ctrl/2
// checkpoints). The usage text is read once for each binary.
func (c *Client) HasCheckpointFlag(ctx context.Context, name string) bool {
	if c == nil {
		return false
	}
	return hasCommandFlag(ctx, &checkpointFlags, "checkpoint", c.Path, name)
}

// Stream runs a command and hands every line of stdout and stderr to line,
// from the goroutines reading them. It returns when the command exits.
func (c *Client) Stream(ctx context.Context, line func(string), args ...string) error {
	if c.Path == "" {
		return ErrNotFound
	}
	cmd := c.Command(ctx, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	var tail tailBuffer
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	var lineMu sync.Mutex
	read := func(r io.Reader, keep bool) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			text := sc.Text()
			if keep {
				tail.add(text)
			}
			lineMu.Lock()
			line(text)
			lineMu.Unlock()
		}
	}
	wg.Add(2)
	go read(stdout, false)
	go read(stderr, true)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		e := &Error{Args: args, Stderr: tail.String(), Err: err}
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			e.Code = exit.ExitCode()
		}
		return e
	}
	return nil
}

type tailBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (t *tailBuffer) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
	if len(t.lines) > 20 {
		t.lines = t.lines[len(t.lines)-20:]
	}
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// Version is the parsed output of rungv version.
type Version struct {
	Program string // rungv, or rungvx since gvx v0.23
	Version string // v0.16.0, (devel), …
	Commit  string
	Go      string
	Raw     string
}

// ParseVersion reads the lines rungv version prints (rungvx adds a fourth,
// its build options).
func ParseVersion(out string) Version {
	v := Version{Raw: strings.TrimSpace(out)}
	for line := range strings.SplitSeq(v.Raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case program, programX:
			v.Program, v.Version = f[0], f[1]
		case "commit":
			v.Commit = f[1]
		case "go":
			v.Go = strings.Join(f[1:], " ")
		}
	}
	return v
}

// Version runs rungv version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	out, err := c.output(ctx, "version")
	if err != nil {
		return Version{}, err
	}
	return ParseVersion(string(out)), nil
}

// Info mirrors gvx.SystemInfo, the JSON of rungv info.
type Info struct {
	OS                string            `json:"OS"`
	Architecture      string            `json:"Architecture"`
	GoVersion         string            `json:"GoVersion"`
	NumCPU            int               `json:"NumCPU"`
	PageSize          int               `json:"PageSize"`
	DefaultBackend    string            `json:"DefaultBackend"`
	AvailableBackends []string          `json:"AvailableBackends"`
	BackendWarnings   []string          `json:"BackendWarnings,omitempty"`
	HostLockdown      string            `json:"HostLockdown"`
	LayerCacheDir     string            `json:"LayerCacheDir"`
	VolumeDir         string            `json:"VolumeDir"`
	Images            int               `json:"Images"`
	Volumes           int               `json:"Volumes"`
	BackendDetails    map[string]string `json:"BackendDetails,omitempty"`
}

// Info runs rungv info --format json.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	out, err := c.output(ctx, "info", "--format", "json")
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(out, &info)
	return info, err
}

// Image is one row of rungv images --format json.
type Image struct {
	CreatedAt    string `json:"CreatedAt"`
	CreatedSince string `json:"CreatedSince"`
	Digest       string `json:"Digest"`
	ID           string `json:"ID"`
	Repository   string `json:"Repository"`
	Size         string `json:"Size"`
	Tag          string `json:"Tag"`
}

// Ref is the reference that names the image: repository:tag, or the ID
// for a dangling one.
func (i Image) Ref() string {
	if i.Repository == "" || i.Repository == "<none>" {
		return i.ID
	}
	if i.Tag == "" || i.Tag == "<none>" {
		return i.Repository + "@" + i.Digest
	}
	return i.Repository + ":" + i.Tag
}

// Key identifies a row, unique even for an image with several tags.
func (i Image) Key() string { return i.ID + "|" + i.Repository + "|" + i.Tag }

// Volume is one row of rungv volume ls --format json.
type Volume struct {
	Driver     string `json:"Driver"`
	Name       string `json:"Name"`
	Mountpoint string `json:"Mountpoint"`
	CreatedAt  string `json:"CreatedAt"`
	Size       string `json:"Size"`
	InUse      bool   `json:"InUse"`
}

// DiskUsage is one row of rungv system df --format json.
type DiskUsage struct {
	Type        string `json:"Type"`
	TotalCount  int    `json:"TotalCount"`
	Active      int    `json:"Active"`
	Size        string `json:"Size"`
	Reclaimable string `json:"Reclaimable"`
}

func decodeLines[T any](out []byte) ([]T, error) {
	var rows []T
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var row T
		if err := dec.Decode(&row); err != nil {
			if errors.Is(err, io.EOF) {
				return rows, nil
			}
			return rows, err
		}
		rows = append(rows, row)
	}
}

// Images lists the cached images.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	out, err := c.output(ctx, "images", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeLines[Image](out)
}

// Volumes lists the local volumes.
func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	out, err := c.output(ctx, "volume", "ls", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeLines[Volume](out)
}

// DiskUsage reports the totals of images and volumes.
func (c *Client) DiskUsage(ctx context.Context) ([]DiskUsage, error) {
	out, err := c.output(ctx, "system", "df", "--format", "json")
	if err != nil {
		return nil, err
	}
	return decodeLines[DiskUsage](out)
}

// InspectImage returns the indented JSON of rungv inspect.
func (c *Client) InspectImage(ctx context.Context, ref string) (string, error) {
	out, err := c.output(ctx, "inspect", ref)
	if err != nil {
		return "", err
	}
	var v any
	if json.Unmarshal(out, &v) == nil {
		if pretty, err := json.MarshalIndent(v, "", "  "); err == nil {
			return string(pretty), nil
		}
	}
	return string(out), nil
}

// Pull downloads ref, handing progress lines to line.
func (c *Client) Pull(ctx context.Context, ref string, line func(string)) error {
	args := []string{"pull"}
	if c.Features(ctx).KeepBlobs {
		args = append(args, "-keep-blobs")
	}
	args = append(args, ref)
	return c.Stream(ctx, line, args...)
}

// RemoveImage removes a tag or an image.
func (c *Client) RemoveImage(ctx context.Context, ref string, force bool) error {
	args := []string{"rmi"}
	if force {
		args = append(args, "-f")
	}
	_, err := c.output(ctx, append(args, ref)...)
	return err
}

// PruneImages deletes dangling images, or every image with all.
func (c *Client) PruneImages(ctx context.Context, all bool) (string, error) {
	args := []string{"image", "prune", "-f"}
	if all {
		args = append(args, "-a")
	}
	out, err := c.output(ctx, args...)
	return strings.TrimSpace(string(out)), err
}

// CreateVolume formats a named volume. driver is ext4lite (default) or hostfs.
func (c *Client) CreateVolume(ctx context.Context, name, driver string) error {
	args := []string{"volume", "create"}
	if driver != "" {
		args = append(args, "-d", driver)
	}
	if name != "" {
		args = append(args, name)
	}
	_, err := c.output(ctx, args...)
	return err
}

// RemoveVolume deletes an unlocked volume.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	_, err := c.output(ctx, "volume", "rm", name)
	return err
}

// PruneVolumes deletes every unlocked volume.
func (c *Client) PruneVolumes(ctx context.Context) (string, error) {
	out, err := c.output(ctx, "volume", "prune", "-f")
	return strings.TrimSpace(string(out)), err
}

// CheckpointOptions configures `rungvx checkpoint` either against a pre-opened
// -checkpoint-dir or as a gvx-ctrl/2 streamed bundle via -o/--output.
type CheckpointOptions struct {
	Dir          string // -checkpoint-dir (pre-opened dir; mutually exclusive with Output)
	Output       string // -o (gvx-ctrl/2 checkpoint_stream destination dir, .tar, or "-")
	LeaveRunning bool   // -leave-running
	Compress     bool   // -z (zstd frame compression; valid only with Output)
	Force        bool   // --force (checkpoint even while exec sessions are active)
}

// CheckpointArgs builds the argument vector for `rungvx checkpoint`.
func CheckpointArgs(t Target, o CheckpointOptions) ([]string, error) {
	if !t.Valid() {
		return nil, errors.New("rungvx checkpoint: control target has no socket")
	}
	dir := strings.TrimSpace(o.Dir)
	out := strings.TrimSpace(o.Output)
	if dir != "" && out != "" {
		return nil, errors.New("rungvx checkpoint: Dir and Output are mutually exclusive")
	}
	if o.Compress && out == "" {
		return nil, errors.New("rungvx checkpoint: Compress requires Output")
	}
	args := []string{"checkpoint", "-control-socket", t.Socket}
	if t.TokenFile != "" {
		args = append(args, "--token-file", t.TokenFile)
	}
	if out != "" {
		args = append(args, "-o", out)
		if o.Compress {
			args = append(args, "-z")
		}
	} else if dir != "" {
		args = append(args, "-checkpoint-dir", dir)
	}
	if o.LeaveRunning {
		args = append(args, "-leave-running")
	}
	if o.Force {
		args = append(args, "--force")
	}
	return args, nil
}

// Checkpoint asks the sandbox listening on socket to write its bundle.
func (c *Client) Checkpoint(ctx context.Context, socket, dir string, leaveRunning bool) error {
	return c.CheckpointTarget(ctx, Target{Socket: socket}, dir, leaveRunning)
}

// CheckpointTarget asks the sandbox at t to write its bundle, passing
// --token-file when t carries one.
func (c *Client) CheckpointTarget(ctx context.Context, t Target, dir string, leaveRunning bool) error {
	return c.CheckpointWithOptions(ctx, t, CheckpointOptions{
		Dir:          dir,
		LeaveRunning: leaveRunning,
	})
}

// CheckpointWithOptions runs `rungvx checkpoint` with full gvx-ctrl/2 options
// (including streamed checkpoints via -o/--output and -z compression).
func (c *Client) CheckpointWithOptions(ctx context.Context, t Target, o CheckpointOptions) error {
	if c == nil || c.Path == "" {
		return ErrNotFound
	}
	args, err := CheckpointArgs(t, o)
	if err != nil {
		return err
	}
	_, err = c.output(ctx, args...)
	return err
}

// Login stores registry credentials. The password goes over stdin.
func (c *Client) Login(ctx context.Context, server, user, password string) error {
	args := []string{"login", "-u", user, "--password-stdin"}
	if server != "" {
		args = append(args, server)
	}
	cmd := c.Command(ctx, args...)
	cmd.Stdin = strings.NewReader(password)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return nil
}

// Diagnose turns a failure message of rungv into advice a person can act
// on, or "" when it knows none.
func Diagnose(msg string, exitCode int) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "address already in use"):
		return "A published host port is already used by another program. Pick a free port or stop the program holding it."
	case strings.Contains(m, "in use") && strings.Contains(m, "volume"):
		return "The volume is mounted by another running sandbox. A volume can be used by one sandbox at a time."
	case strings.Contains(m, "entitlement") || strings.Contains(m, "hypervisor") && runtime.GOOS == "darwin":
		return "rungvx must be signed with the com.apple.security.hypervisor entitlement. Use Settings → Sign rungvx."
	case strings.Contains(m, "windows hypervisor platform") || strings.Contains(m, "whpx"):
		return "Enable the \"Windows Hypervisor Platform\" optional feature, or allow the systrap fallback for trusted workloads."
	case strings.Contains(m, "memory limit"):
		return "The guest passed its memory limit and was stopped. Raise the memory limit in the sandbox settings."
	case strings.Contains(m, "execution engine stopped the guest"):
		return "The execution engine failed under the guest (a virus scanner may have ended a helper process)."
	case strings.Contains(m, "manifest unknown") || strings.Contains(m, "not found") && strings.Contains(m, "image"):
		return "The image reference was not found in its registry. Check the name and tag."
	case strings.Contains(m, "unauthorized") || strings.Contains(m, "401"):
		return "The registry refused the request. Sign in to the registry first."
	case strings.Contains(m, "no such host") || strings.Contains(m, "dial tcp"):
		return "The registry could not be reached. Check the network or the proxy settings."
	case exitCode == 137:
		return "The guest was killed (exit 137): it passed its memory limit or the engine stopped it."
	}
	return ""
}

// FormatArgs quotes args for display as one shell command line.
func FormatArgs(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(Quote(a))
	}
	return b.String()
}

// Quote quotes s for a POSIX shell when it needs it.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=,@%+~", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SplitArgs splits a command line into words, honouring single and double
// quotes and backslash escapes, as a POSIX shell does without expansion.
func SplitArgs(s string) ([]string, error) {
	var (
		words   []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return words, fmt.Errorf("unterminated %c quote", quote)
	}
	if escaped {
		cur.WriteRune('\\')
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
