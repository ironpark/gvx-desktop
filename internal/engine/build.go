package engine

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

const (
	// maxBuildContextBytes caps both the incoming build context stream and the
	// total extracted size of a legacy POST /build request.
	maxBuildContextBytes = 512 << 20 // 512 MiB

	// guestHostLoopback is how a gvx sandbox on the default NAT network reaches
	// the host's loopback address (127.0.0.1).
	guestHostLoopback = "10.0.2.2"
)

// buildOptions holds the parsed query parameters of POST /build.
type buildOptions struct {
	Dockerfile string
	Tags       []string
	BuildArgs  map[string]string
	Labels     map[string]string
	Target     string
	NoCache    bool
	Pull       bool
	Platform   string
	Quiet      bool
}

func parseBuildRequest(r *http.Request) (buildOptions, error) {
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("remote")) != "" {
		return buildOptions{}, errNotSupported("Remote build contexts")
	}

	opts := buildOptions{
		Dockerfile: strings.TrimSpace(q.Get("dockerfile")),
		Target:     strings.TrimSpace(q.Get("target")),
		NoCache:    queryBool(r, "nocache"),
		Pull:       queryBool(r, "pull"),
		Platform:   strings.TrimSpace(q.Get("platform")),
		Quiet:      queryBool(r, "q"),
	}

	df, err := cleanBuildDockerfile(opts.Dockerfile)
	if err != nil {
		return buildOptions{}, err
	}
	opts.Dockerfile = df

	if opts.Platform != "" {
		if err := validateImagePlatform(opts.Platform); err != nil {
			return buildOptions{}, err
		}
	}
	if strings.ContainsRune(opts.Target, 0) {
		return buildOptions{}, errInvalid("invalid target")
	}

	for _, rawTag := range q["t"] {
		tag := strings.TrimSpace(rawTag)
		if tag == "" {
			continue
		}
		if err := validateImageRef(tag); err != nil {
			return buildOptions{}, err
		}
		if strings.ContainsAny(tag, ",\"\n\r") {
			return buildOptions{}, errInvalid("invalid tag %q", tag)
		}
		if !slices.Contains(opts.Tags, tag) {
			opts.Tags = append(opts.Tags, tag)
		}
	}

	if raw := strings.TrimSpace(q.Get("buildargs")); raw != "" {
		var m map[string]*string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return buildOptions{}, errInvalid("invalid buildargs JSON: %v", err)
		}
		opts.BuildArgs = make(map[string]string, len(m))
		for k, v := range m {
			if strings.ContainsAny(k, "=\x00") || k == "" {
				return buildOptions{}, errInvalid("invalid build arg key %q", k)
			}
			if v != nil {
				if strings.ContainsRune(*v, 0) {
					return buildOptions{}, errInvalid("invalid build arg value for %q", k)
				}
				opts.BuildArgs[k] = *v
			}
		}
	}

	if raw := strings.TrimSpace(q.Get("labels")); raw != "" {
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return buildOptions{}, errInvalid("invalid labels JSON: %v", err)
		}
		opts.Labels = make(map[string]string, len(m))
		for k, v := range m {
			if strings.ContainsAny(k, "=\x00") || k == "" || strings.ContainsRune(v, 0) {
				return buildOptions{}, errInvalid("invalid label %q", k)
			}
			opts.Labels[k] = v
		}
	}

	return opts, nil
}

func cleanBuildDockerfile(raw string) (string, error) {
	if raw == "" {
		return "Dockerfile", nil
	}
	if strings.ContainsRune(raw, 0) {
		return "", errInvalid("invalid dockerfile path")
	}
	s := strings.ReplaceAll(raw, "\\", "/")
	s = strings.TrimLeft(s, "/")
	cleaned := path.Clean(s)
	if cleaned == "." || cleaned == "" {
		return "Dockerfile", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errInvalid("dockerfile path %q escapes build context", raw)
	}
	return cleaned, nil
}

// buildctlArgs constructs the argument vector for `buildctl` inside the
// one-shot builder client sandbox.
func buildctlArgs(addr string, opts buildOptions) []string {
	args := []string{
		"--addr", addr,
		"build",
		"--progress=plain",
		"--frontend", "dockerfile.v0",
		"--local", "context=/ctx",
		"--local", "dockerfile=/ctx",
	}
	if opts.Dockerfile != "" {
		args = append(args, "--opt", "filename="+opts.Dockerfile)
	}
	if opts.Target != "" {
		args = append(args, "--opt", "target="+opts.Target)
	}
	if opts.Platform != "" {
		args = append(args, "--opt", "platform="+opts.Platform)
	}
	if opts.Pull {
		args = append(args, "--opt", "image-resolve-mode=pull")
	}
	if opts.NoCache {
		args = append(args, "--no-cache")
	}
	if len(opts.BuildArgs) > 0 {
		keys := make([]string, 0, len(opts.BuildArgs))
		for k := range opts.BuildArgs {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			args = append(args, "--opt", "build-arg:"+k+"="+opts.BuildArgs[k])
		}
	}
	if len(opts.Labels) > 0 {
		keys := make([]string, 0, len(opts.Labels))
		for k := range opts.Labels {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			args = append(args, "--opt", "label:"+k+"="+opts.Labels[k])
		}
	}

	outOpt := "type=docker,dest=/out/image.tar"
	if len(opts.Tags) > 0 {
		outOpt = "type=docker,name=" + opts.Tags[0] + ",dest=/out/image.tar"
	}
	args = append(args, "--output", outOpt)
	return args
}

const (
	hostfsMetaFile    = ".gvx_meta"
	hostfsModeDir     = 0o040000
	hostfsModeReg     = 0o100000
	hostfsModeSymlink = 0o120000
)

type hostfsMetaEntry struct {
	Mode    uint32 `json:"mode"`
	UID     uint32 `json:"uid"`
	GID     uint32 `json:"gid"`
	Symlink string `json:"symlink,omitempty"`
}

type hostfsMetaFileDoc struct {
	Version int                        `json:"version"`
	Entries map[string]hostfsMetaEntry `json:"entries"`
}

func isHostfsReservedRoot(relSlash string) bool {
	if strings.Contains(relSlash, "/") {
		return false
	}
	lower := strings.ToLower(relSlash)
	return lower == hostfsMetaFile || lower == ".gvx_meta.lock" || lower == ".gvx_meta.tmp" || strings.HasPrefix(lower, ".gvx_meta.tmp.")
}

func hasVirtualSymlinkParent(virtualSymlinks map[string]bool, relSlash string) (string, bool) {
	if len(virtualSymlinks) == 0 {
		return "", false
	}
	for dir := path.Dir(relSlash); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if virtualSymlinks[dir] {
			return dir, true
		}
	}
	return "", false
}

// extractBuildContext unpacks a tar (raw, gzip, or bzip2) build context stream
// into dstDir, rejecting path traversal, symlink escapes, special device nodes,
// and archives exceeding maxBytes.
func extractBuildContext(body io.Reader, dstDir string, maxBytes int64) error {
	return extractBuildContextForOS(body, dstDir, maxBytes, runtime.GOOS)
}

func extractBuildContextForOS(body io.Reader, dstDir string, maxBytes int64, goos string) error {
	if maxBytes <= 0 {
		maxBytes = maxBuildContextBytes
	}
	if body == nil {
		return errInvalid("empty build context")
	}

	limitedBody := &cappedReader{r: body, left: maxBytes}
	br := bufio.NewReader(limitedBody)
	magic, _ := br.Peek(3)

	var tarStream io.Reader = br
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return errInvalid("invalid gzip build context: %v", err)
		}
		defer gz.Close()
		tarStream = gz
	case len(magic) >= 3 && magic[0] == 'B' && magic[1] == 'Z' && magic[2] == 'h':
		tarStream = bzip2.NewReader(br)
	}

	cleanRoot := filepath.Clean(dstDir)
	winMode := goos == "windows"
	var (
		metaEntries     map[string]hostfsMetaEntry
		virtualSymlinks map[string]bool
	)
	if winMode {
		metaEntries = map[string]hostfsMetaEntry{}
		virtualSymlinks = map[string]bool{}
	}

	tr := tar.NewReader(tarStream)
	var extracted int64

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, errBuildContextTooLarge) {
				return errInvalid("build context exceeds size limit (%d bytes)", maxBytes)
			}
			return errInvalid("invalid tar build context: %v", err)
		}

		// Skip PAX global/extended headers handled by archive/tar.
		if hdr.Typeflag == tar.TypeXHeader || hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeGNULongName || hdr.Typeflag == tar.TypeGNULongLink {
			continue
		}

		rel, err := safeTarRelPath(hdr.Name)
		if err != nil {
			return err
		}
		if rel == "." {
			continue
		}
		relSlash := filepath.ToSlash(rel)
		if isHostfsReservedRoot(relSlash) {
			continue
		}
		if parent, ok := hasVirtualSymlinkParent(virtualSymlinks, relSlash); ok {
			return errInvalid("tar entry %q traverses symlink %q", hdr.Name, parent)
		}
		target := filepath.Join(cleanRoot, rel)
		if !isPathWithinRoot(cleanRoot, target) {
			return errInvalid("tar entry %q escapes build context", hdr.Name)
		}
		if err := ensureNoSymlinkParent(cleanRoot, filepath.Dir(target)); err != nil {
			return err
		}

		mode := os.FileMode(hdr.Mode) & 0o777
		switch hdr.Typeflag {
		case tar.TypeDir:
			if mode == 0 {
				mode = 0o755
			}
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return err
			}
			if winMode {
				delete(virtualSymlinks, relSlash)
				if mode != 0o755 {
					metaEntries[relSlash] = hostfsMetaEntry{Mode: hostfsModeDir | uint32(mode)}
				} else {
					delete(metaEntries, relSlash)
				}
			}
		case tar.TypeReg:
			if hdr.Size < 0 || extracted+hdr.Size > maxBytes {
				return errInvalid("build context exceeds size limit (%d bytes)", maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if mode == 0 {
				mode = 0o644
			}
			// Remove any pre-existing file/symlink at target before creating.
			_ = os.Remove(target)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o200)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(f, io.LimitReader(tr, maxBytes-extracted+1))
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			extracted += n
			if extracted > maxBytes {
				return errInvalid("build context exceeds size limit (%d bytes)", maxBytes)
			}
			if winMode {
				delete(virtualSymlinks, relSlash)
				if mode != 0o644 {
					metaEntries[relSlash] = hostfsMetaEntry{Mode: hostfsModeReg | uint32(mode)}
				} else {
					delete(metaEntries, relSlash)
				}
			} else {
				_ = os.Chmod(target, mode|0o200)
			}
		case tar.TypeSymlink:
			link := hdr.Linkname
			if link == "" || strings.ContainsRune(link, 0) {
				return errInvalid("invalid symlink target for %q", hdr.Name)
			}
			if filepath.IsAbs(link) || strings.HasPrefix(link, "/") || strings.HasPrefix(link, "\\") {
				return errInvalid("symlink %q -> %q escapes build context", hdr.Name, link)
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(link)))
			if !isPathWithinRoot(cleanRoot, resolved) {
				return errInvalid("symlink %q -> %q escapes build context", hdr.Name, link)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if winMode {
				// Store as a hostfs virtual symlink in .gvx_meta rather than
				// calling os.Symlink, which fails on Windows without Developer
				// Mode / admin privileges and is refused as an NTFS reparse
				// point by rungv's hostfs.
				virtualSymlinks[relSlash] = true
				metaEntries[relSlash] = hostfsMetaEntry{
					Mode:    hostfsModeSymlink | 0o777,
					Symlink: strings.ReplaceAll(link, "\\", "/"),
				}
			} else if err := os.Symlink(link, target); err != nil {
				return err
			}
		case tar.TypeLink:
			linkRel, err := safeTarRelPath(hdr.Linkname)
			if err != nil {
				return errInvalid("hard link %q -> %q escapes build context", hdr.Name, hdr.Linkname)
			}
			linkTarget := filepath.Join(cleanRoot, linkRel)
			if !isPathWithinRoot(cleanRoot, linkTarget) {
				return errInvalid("hard link %q -> %q escapes build context", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			linkRelSlash := filepath.ToSlash(linkRel)
			if winMode && virtualSymlinks[linkRelSlash] {
				virtualSymlinks[relSlash] = true
				metaEntries[relSlash] = metaEntries[linkRelSlash]
				continue
			}
			if err := os.Link(linkTarget, target); err != nil {
				return err
			}
			if winMode {
				delete(virtualSymlinks, relSlash)
				if entry, ok := metaEntries[linkRelSlash]; ok {
					metaEntries[relSlash] = entry
				} else {
					delete(metaEntries, relSlash)
				}
			}
		default:
			return errInvalid("unsupported tar entry type %q for %q", string(hdr.Typeflag), hdr.Name)
		}
	}

	if winMode && len(metaEntries) > 0 {
		raw, err := json.Marshal(hostfsMetaFileDoc{
			Version: 2,
			Entries: metaEntries,
		})
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(cleanRoot, hostfsMetaFile), append(raw, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

var errBuildContextTooLarge = errors.New("build context too large")

type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		var one [1]byte
		n, err := c.r.Read(one[:])
		if n > 0 {
			return 0, errBuildContextTooLarge
		}
		if err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

func safeTarRelPath(name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", errInvalid("tar entry contains NUL byte")
	}
	s := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(s, "/") {
		return "", errInvalid("tar entry %q has absolute path", name)
	}
	// Reject Windows drive letters like C:...
	if len(s) >= 2 && s[1] == ':' && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z') {
		return "", errInvalid("tar entry %q has drive prefix", name)
	}
	cleaned := path.Clean(s)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errInvalid("tar entry %q escapes build context", name)
	}
	return filepath.FromSlash(cleaned), nil
}

func isPathWithinRoot(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func ensureNoSymlinkParent(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return nil
	}
	cur := root
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errInvalid("path %q traverses a symlink", rel)
		}
	}
	return nil
}

// buildStreamWriter formats legacy Docker POST /build JSON progress frames and
// flushes each line as it is written.
type buildStreamWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
	quiet   bool
}

func newBuildStreamWriter(w http.ResponseWriter, quiet bool) *buildStreamWriter {
	return &buildStreamWriter{
		w:     w,
		rc:    http.NewResponseController(w),
		quiet: quiet,
	}
}

func (b *buildStreamWriter) writeObj(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		b.w.Header().Set("Content-Type", "application/json")
		b.w.WriteHeader(http.StatusOK)
		b.started = true
	}
	_, _ = b.w.Write(raw)
	_ = b.rc.Flush()
}

func (b *buildStreamWriter) stream(text string) {
	if b.quiet || text == "" {
		return
	}
	b.writeObj(struct {
		Stream string `json:"stream"`
	}{Stream: text})
}

func (b *buildStreamWriter) aux(imageID string) {
	if imageID == "" {
		return
	}
	b.writeObj(struct {
		Aux struct {
			ID string `json:"ID"`
		} `json:"aux"`
	}{Aux: struct {
		ID string `json:"ID"`
	}{ID: imageID}})
}

func (b *buildStreamWriter) err(code int, msg string) {
	if code == 0 {
		code = 1
	}
	b.writeObj(struct {
		ErrorDetail struct {
			Code    int    `json:"code,omitempty"`
			Message string `json:"message"`
		} `json:"errorDetail"`
		Error string `json:"error"`
	}{
		ErrorDetail: struct {
			Code    int    `json:"code,omitempty"`
			Message string `json:"message"`
		}{Code: code, Message: msg},
		Error: msg,
	})
}

// buildLineBuffer splits raw output chunks into newline-terminated lines and
// hands each line to emit, keeping the last non-empty lines for failure errors.
type buildLineBuffer struct {
	mu    sync.Mutex
	carry bytes.Buffer
	tail  []string
	emit  func(line string)
}

func (l *buildLineBuffer) write(data []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.carry.Write(data)
	for {
		b := l.carry.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return
		}
		line := strings.TrimRight(string(b[:i]), "\r")
		l.carry.Next(i + 1)
		l.recordAndEmitLocked(line)
	}
}

func (l *buildLineBuffer) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.carry.Len() == 0 {
		return
	}
	line := strings.TrimRight(l.carry.String(), "\r")
	l.carry.Reset()
	l.recordAndEmitLocked(line)
}

func (l *buildLineBuffer) recordAndEmitLocked(line string) {
	if trimmed := strings.TrimSpace(line); trimmed != "" {
		l.tail = append(l.tail, trimmed)
		if len(l.tail) > 12 {
			l.tail = l.tail[len(l.tail)-12:]
		}
	}
	if l.emit != nil {
		l.emit(line + "\n")
	}
}

func (l *buildLineBuffer) lastError() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.tail) - 1; i >= 0; i-- {
		if strings.HasPrefix(l.tail[i], "error:") || strings.HasPrefix(l.tail[i], "ERROR:") {
			return l.tail[i]
		}
	}
	if len(l.tail) > 0 {
		return l.tail[len(l.tail)-1]
	}
	return ""
}

// imageBuild serves legacy POST /build (DOCKER_BUILDKIT=0, Docker SDKs,
// testcontainers) by running buildctl in a one-shot internal sandbox against
// the engine's lazy BuildKit builder, loading the resulting image tar into
// rungv, and applying all requested tags.
func (e *Engine) imageBuild(w http.ResponseWriter, r *http.Request) {
	opts, err := parseBuildRequest(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	baseTmp := os.TempDir()
	if e.cfg.Dir != "" {
		baseTmp = e.path("builds")
		if err := os.MkdirAll(baseTmp, 0o700); err != nil {
			writeError(w, r, err)
			return
		}
	}
	workDir, err := os.MkdirTemp(baseTmp, "b-*")
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer os.RemoveAll(workDir)

	ctxDir := filepath.Join(workDir, "ctx")
	outDir := filepath.Join(workDir, "out")
	if err := os.MkdirAll(ctxDir, 0o700); err != nil {
		writeError(w, r, err)
		return
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		writeError(w, r, err)
		return
	}

	if err := extractBuildContext(r.Body, ctxDir, maxBuildContextBytes); err != nil {
		writeError(w, r, err)
		return
	}

	sw := newBuildStreamWriter(w, opts.Quiet)

	backendSock, release, err := e.acquireBuilder(r.Context())
	if err != nil {
		writeError(w, r, fmt.Errorf("start builder: %w", err))
		return
	}
	defer release()

	var (
		addr         string
		unixForwards []string
	)
	if e.runner.Features().UnixForward && backendSock != "" {
		const guestBuildkitSock = "/run/buildkit/buildkitd.sock"
		unixForwards = []string{guestBuildkitSock + "=" + backendSock}
		addr = "unix://" + guestBuildkitSock
	} else {
		// Expose the builder over an ephemeral loopback TCP port for the duration
		// of this build so the one-shot buildctl sandbox reaches it via 10.0.2.2.
		bridgeLn, bridgePort, closeBridge, err := e.openBuilderTCPBridge(r.Context(), backendSock)
		if err != nil {
			writeError(w, r, fmt.Errorf("open builder bridge: %w", err))
			return
		}
		defer closeBridge()
		_ = bridgeLn
		addr = fmt.Sprintf("tcp://%s:%d", guestHostLoopback, bridgePort)
	}
	args := buildctlArgs(addr, opts)

	suffix := randomHex64()[:12]
	jobID := builderBuildctlPrefix + suffix
	lines := &buildLineBuffer{emit: sw.stream}
	job := e.registerBuildJob(jobID, func(o sandbox.Output) {
		lines.write(o.Data)
	})
	defer func() {
		e.unregisterBuildJob(jobID)
		e.runner.Forget(jobID)
	}()

	spec := sandbox.Spec{
		ID:            jobID,
		Name:          "gvxd-buildctl-" + suffix,
		Image:         BuilderImage,
		Pull:          "never",
		SetEntrypoint: true,
		Entrypoint:    "buildctl",
		Command:       rungv.FormatArgs(args),
		UnixForwards:  unixForwards,
		Mounts: []sandbox.Mount{
			{Type: sandbox.MountBind, Source: ctxDir, Target: "/ctx", ReadOnly: true},
			{Type: sandbox.MountBind, Source: outDir, Target: "/out"},
		},
	}

	if err := e.runner.Start(spec); err != nil {
		sw.err(1, fmt.Sprintf("start buildctl: %v", err))
		return
	}

	var exitState sandbox.State
	select {
	case exitState = <-job.done:
	case <-r.Context().Done():
		_ = e.runner.Stop(jobID, 5*time.Second)
		lines.flush()
		sw.err(1, "build cancelled")
		return
	}
	lines.flush()

	if exitState.ExitCode != 0 {
		msg := lines.lastError()
		if msg == "" {
			msg = exitState.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("build failed with exit code %d", exitState.ExitCode)
		}
		sw.err(exitState.ExitCode, msg)
		return
	}

	tarPath := filepath.Join(outDir, "image.tar")
	// Best-effort read of the config digest from manifest.json; LoadImages is
	// the authoritative validator of the archive.
	imgID, _ := extractTarImageID(tarPath)

	f, err := os.Open(tarPath)
	if err != nil {
		sw.err(1, fmt.Sprintf("open built image archive: %v", err))
		return
	}
	var loadedRef string
	loadErr := e.runner.LoadImages(r.Context(), f, func(line string) {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Loaded image: "); ok {
			loadedRef = strings.TrimSpace(rest)
		} else if rest, ok := strings.CutPrefix(line, "Loaded image ID: "); ok && imgID == "" {
			imgID = strings.TrimSpace(rest)
		}
	})
	_ = f.Close()
	if loadErr != nil {
		sw.err(1, fmt.Sprintf("load built image: %v", loadErr))
		return
	}

	// Resolve the loaded image ID and apply all requested tags.
	sourceRef := loadedRef
	if sourceRef == "" && len(opts.Tags) > 0 {
		sourceRef = opts.Tags[0]
	}
	if sourceRef == "" {
		sourceRef = imgID
	}
	if sourceRef != "" {
		if ins, err := e.imageConfig(r.Context(), sourceRef); err == nil {
			if ins.ID != "" {
				imgID = ins.ID
			}
			if ins.ResolvedRef != "" {
				sourceRef = ins.ResolvedRef
			}
		}
	}

	if err := e.tagBuiltImage(r.Context(), sourceRef, imgID, opts.Tags); err != nil {
		sw.err(1, fmt.Sprintf("tag built image: %v", err))
		return
	}

	if imgID != "" {
		sw.aux(imgID)
		if opts.Quiet {
			sw.writeObj(struct {
				Stream string `json:"stream"`
			}{Stream: imgID + "\n"})
		} else {
			sw.stream(fmt.Sprintf("Successfully built %s\n", shortHex(imgID)))
		}
	}
	for _, tag := range opts.Tags {
		sw.stream(fmt.Sprintf("Successfully tagged %s\n", tag))
	}

	e.emit(Event{
		Type:       "image",
		Action:     "load",
		ID:         imgID,
		Attributes: map[string]string{"name": firstOrEmpty(opts.Tags)},
	})
	e.notify()
}

func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// tagBuiltImage ensures every requested tag points at the newly loaded image.
func (e *Engine) tagBuiltImage(ctx context.Context, sourceRef, imgID string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	src := sourceRef
	if src == "" {
		src = imgID
	}
	for i, tag := range tags {
		target := tag
		if strings.LastIndex(target, ":") <= strings.LastIndex(target, "/") {
			target += ":latest"
		}
		if i == 0 && src != "" && normalizeImageRef(src) == normalizeImageRef(target) {
			continue
		}
		if err := e.runner.TagImage(ctx, src, target); err != nil {
			if imgID != "" && imgID != src {
				if err2 := e.runner.TagImage(ctx, imgID, target); err2 == nil {
					continue
				}
			}
			return err
		}
	}
	return nil
}

// extractTarImageID reads manifest.json from a Docker save tarball and returns
// the image config digest ("sha256:..."), or "" if not present.
func extractTarImageID(tarPath string) (string, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		if path.Clean(hdr.Name) != "manifest.json" {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 4<<20))
		if err != nil {
			return "", err
		}
		var manifest []struct {
			Config string `json:"Config"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil || len(manifest) == 0 {
			return "", nil
		}
		return imageIDFromManifestConfig(manifest[0].Config), nil
	}
}

func imageIDFromManifestConfig(cfgPath string) string {
	cfgPath = strings.TrimSpace(cfgPath)
	if cfgPath == "" {
		return ""
	}
	if hexPart, ok := strings.CutPrefix(cfgPath, "blobs/sha256/"); ok && isHexString(hexPart) {
		return "sha256:" + hexPart
	}
	base := path.Base(cfgPath)
	base = strings.TrimSuffix(base, ".json")
	base = strings.TrimPrefix(base, "sha256:")
	if len(base) >= 32 && isHexString(base) {
		return "sha256:" + base
	}
	return ""
}

// openBuilderTCPBridge listens on 127.0.0.1:0 and forwards each accepted TCP
// connection to the builder's Unix socket until the returned cleanup function
// is called.
func (e *Engine) openBuilderTCPBridge(ctx context.Context, backendSock string) (net.Listener, int, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port

	var (
		mu     sync.Mutex
		closed bool
		conns  = map[net.Conn]struct{}{}
	)
	track := func(c net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			_ = c.Close()
			return false
		}
		conns[c] = struct{}{}
		return true
	}
	untrack := func(c net.Conn) {
		mu.Lock()
		delete(conns, c)
		mu.Unlock()
	}

	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			if !track(client) {
				continue
			}
			go func(c net.Conn) {
				defer func() {
					untrack(c)
					_ = c.Close()
				}()
				backend, err := dialBuilderBackend(ctx, backendSock, 5*time.Second)
				if err != nil {
					return
				}
				if !track(backend) {
					return
				}
				defer func() {
					untrack(backend)
					_ = backend.Close()
				}()
				pipeHalfClose(c, backend)
			}(client)
		}
	}()

	cleanup := func() {
		mu.Lock()
		closed = true
		list := make([]net.Conn, 0, len(conns))
		for c := range conns {
			list = append(list, c)
		}
		clear(conns)
		mu.Unlock()
		_ = ln.Close()
		for _, c := range list {
			_ = c.Close()
		}
	}
	return ln, port, cleanup, nil
}
