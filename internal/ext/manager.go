package ext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/keychain"
	"github.com/ironpark/gvx-desktop/internal/localproxy"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
	"github.com/ironpark/gvx-desktop/internal/uiblock"
	"github.com/ironpark/gvx-desktop/pkg/extsdk"
)

const (
	maxExtensionCrashes = 3
	maxBuildLogLines    = 2000
	idleSweepInterval   = 250 * time.Millisecond
)

// HostInfo describes the GVX Desktop host environment exposed to extensions via GET /v1/host/info.
type HostInfo struct {
	AppVersion   string `json:"app_version"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	Locale       string `json:"locale"`
	Theme        string `json:"theme"`
	DockerMode   string `json:"docker_mode"` // "sandbox" | "dind" | "off"
	DockerReady  bool   `json:"docker_ready"`
	RungvVersion string `json:"rungv_version,omitempty"`
}

// UIHost provides host desktop UI callbacks invoked by extension Host API requests.
type UIHost interface {
	PickFolder(ctx context.Context, extID string) (dir string, ok bool, err error)
	OpenTerminal(targetType, targetID, shell string) error
	OpenURL(rawURL string) error
	CopyText(text string) error
	ShowToast(level, message string)
	Changed()
}

// Config configures an extension host Manager.
type Config struct {
	DataDir             string
	Keychain            keychain.Keychain
	KeychainNote        string
	DeveloperMode       func() bool
	HostInfo            func() HostInfo
	Supervisor          *sandbox.Supervisor
	DockerBackendDial   func(ctx context.Context) (net.Conn, error)
	DockerEngineHandler func() http.Handler
	ImageBuilder        ImageBuilder
	ImagePuller         ImagePuller
	UIHost              UIHost
}

type extRecord struct {
	opMu     sync.Mutex
	policyMu sync.RWMutex // strictly ordered after opMu; serializes grant/folder revocations with in-flight Docker/HostAPI lifecycle ops

	id          string
	manifest    *Manifest
	manifestErr string
	source      SourceSpec
	state       ExtensionState
	errMsg      string
	badge       BadgeInfo

	storage  *Storage
	vault    *Vault
	settings *SettingsStore

	hostAPI     *hostAPIServer
	dockerProxy *scopedDockerProxy
	runtime     extRuntime
	stopWatch   context.CancelFunc

	buildLogs      []BuildLogEntry
	crashes        int
	idleStopped    bool
	lastActivity   time.Time
	activeViews    int
	inFlight       int
	pendingRestart bool
}

func (r *extRecord) appendBuildLog(line string) {
	entry := BuildLogEntry{Time: time.Now(), Line: line}
	r.buildLogs = append(r.buildLogs, entry)
	if over := len(r.buildLogs) - maxBuildLogLines; over > 0 {
		r.buildLogs = append([]BuildLogEntry(nil), r.buildLogs[over:]...)
	}
}

func (r *extRecord) sourceDir(dataDir string) string {
	if r.source.Kind == SourceLocal && r.source.LocalPath != "" {
		return r.source.LocalPath
	}
	return SourceCheckoutDir(dataDir, r.id)
}

// Manager orchestrates extension discovery, permission approval, image builds/pulls,
// sandbox/binary execution, Host APIs, scoped Docker proxies, and graceful shutdown.
type Manager struct {
	cfg         Config
	grants      *GrantStore
	gestures    *gestureStore
	installMu   sync.Mutex // strictly ordered before rec.opMu -> m.mu; never acquired by Close()
	sandboxesMu sync.Mutex

	mu       sync.RWMutex
	exts     map[string]*extRecord
	started  bool
	closed   bool
	baseCtx  context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	changeCh chan struct{}
}

// NewManager creates an extension host Manager rooted at cfg.DataDir.
func NewManager(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, errors.New("ext.Config.DataDir is required")
	}
	grants, _ := LoadGrantStore(GrantsPath(cfg.DataDir))
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		cfg:      cfg,
		grants:   grants,
		gestures: newGestureStore(nil),
		exts:     make(map[string]*extRecord),
		baseCtx:  ctx,
		cancel:   cancel,
		changeCh: make(chan struct{}, 1),
	}, nil
}

// Changes returns a coalesced notification channel signaled whenever extension
// state, routes, or build logs change.
func (m *Manager) Changes() <-chan struct{} {
	return m.changeCh
}

func (m *Manager) signalChange() {
	if m == nil || m.changeCh == nil {
		return
	}
	select {
	case m.changeCh <- struct{}{}:
	default:
	}
}

// Start discovers installed extensions under DataDir and launches approved extensions.
func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("extension manager is closed")
	}
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	if ctx != context.Background() || m.baseCtx == nil || m.baseCtx.Err() != nil {
		if m.cancel != nil {
			m.cancel()
		}
		m.baseCtx, m.cancel = context.WithCancel(ctx)
	}
	baseCtx := m.baseCtx
	m.mu.Unlock()

	if err := m.discoverExtensions(); err != nil {
		return err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("extension manager is closed")
	}
	var toStart []*extRecord
	for _, rec := range m.exts {
		if rec.state.CanRun() && rec.manifest != nil && rec.manifest.Runtime.IdleTimeoutSec == 0 {
			rec.state = StateStarting
			toStart = append(toStart, rec)
		}
	}
	m.wg.Add(1 + len(toStart))
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		m.idleReaperLoop(baseCtx)
	}()

	for _, rec := range toStart {
		go func(r *extRecord) {
			defer m.wg.Done()
			if !m.lockActiveRecord(r) {
				return
			}
			defer r.opMu.Unlock()
			m.mu.RLock()
			canRun := r.state.CanRun()
			m.mu.RUnlock()
			if !canRun {
				return
			}
			_ = m.startLocked(baseCtx, r)
		}(rec)
	}
	m.notifyChanged()
	return nil
}

func (m *Manager) discoverExtensions() error {
	if m.isClosed() {
		return nil
	}
	extRoot := ExtensionsDir(m.cfg.DataDir)
	if err := CheckNoSymlinkWithin(m.cfg.DataDir, extRoot); err != nil {
		return err
	}
	entries, err := os.ReadDir(extRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if m.isClosed() {
			return nil
		}
		id := e.Name()
		if strings.HasPrefix(id, ".") || !ValidID(id) {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			rec := m.newBaseRecord(id)
			rec.manifestErr = fmt.Sprintf("extension directory %q must not be a symlink", id)
			rec.state = StateInvalid
			rec.errMsg = rec.manifestErr
			m.mu.Lock()
			if !m.closed {
				if _, exists := m.exts[id]; !exists {
					m.exts[id] = rec
				}
			}
			m.mu.Unlock()
			continue
		}
		if !e.IsDir() {
			continue
		}
		rec := m.loadRecordFromDisk(id)
		m.mu.Lock()
		if !m.closed {
			if _, exists := m.exts[id]; !exists {
				m.exts[id] = rec
			}
		}
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) loadRecordFromDisk(id string) *extRecord {
	rec := m.newBaseRecord(id)
	if err := CheckNoSymlinkWithin(m.cfg.DataDir, ExtensionDir(m.cfg.DataDir, id)); err != nil {
		rec.manifestErr = err.Error()
		rec.state = StateInvalid
		rec.errMsg = rec.manifestErr
		return rec
	}
	if sp, err := LoadSourceWithin(m.cfg.DataDir, SourcePath(m.cfg.DataDir, id)); err == nil {
		rec.source = sp
	}
	manifestPath := ManifestSnapshotPath(m.cfg.DataDir, id)
	raw, err := ReadFileNoSymlinkWithin(m.cfg.DataDir, manifestPath)
	if err != nil {
		rec.manifestErr = fmt.Sprintf("read %s: %v", ManifestName, err)
		rec.state = StateInvalid
		rec.errMsg = rec.manifestErr
		return rec
	}
	var man *Manifest
	if strings.HasPrefix(id, "gvx-") {
		man, err = ParseBundledManifest(raw)
	} else {
		man, err = ParseManifest(raw)
	}
	if err != nil {
		rec.manifestErr = err.Error()
		rec.state = StateInvalid
		rec.errMsg = err.Error()
		return rec
	}
	if man.ID != id {
		rec.manifestErr = fmt.Sprintf("manifest id %q does not match directory %q", man.ID, id)
		rec.state = StateInvalid
		rec.errMsg = rec.manifestErr
		return rec
	}
	rec.manifest = man
	rec.settings.MigrateLegacySecrets(man)
	m.recomputeStaticStateLocked(rec)
	return rec
}

func (m *Manager) newBaseRecord(id string) *extRecord {
	vault := NewVault(m.cfg.Keychain, m.cfg.DataDir, id)
	return &extRecord{
		id:       id,
		storage:  NewStorageInDir(m.cfg.DataDir, id, DefaultStorageQuota),
		vault:    vault,
		settings: NewSettingsStoreInDir(m.cfg.DataDir, id, vault),
	}
}

func (m *Manager) recomputeStaticStateLocked(rec *extRecord) {
	if rec.manifestErr != "" || rec.manifest == nil {
		rec.state = StateInvalid
		rec.errMsg = rec.manifestErr
		return
	}
	entry := m.grants.Get(rec.id)
	if entry.Disabled {
		rec.state = StateDisabled
		rec.errMsg = ""
		return
	}
	waiting, denied, _ := NeedsApproval(rec.manifest, entry, rec.source.Commit)
	if waiting || denied {
		rec.state = StateWaiting
		rec.errMsg = ""
		return
	}
	if rec.manifest.Runtime.Type == RuntimeBinary && (m.cfg.DeveloperMode == nil || !m.cfg.DeveloperMode()) {
		rec.state = StateDisabled
		rec.errMsg = "Developer Mode is required for binary extensions"
		return
	}
	rec.state = StateDiscovered
	rec.errMsg = ""
}

// InstallGit clones a Git extension repository (without building) and marks it StateWaiting.
func (m *Manager) InstallGit(repo, ref string) (ExtensionInfo, error) {
	if m.isClosed() {
		return ExtensionInfo{}, errors.New("extension manager is closed")
	}
	m.installMu.Lock()
	defer m.installMu.Unlock()
	if m.isClosed() {
		return ExtensionInfo{}, errors.New("extension manager is closed")
	}
	raw := strings.TrimSpace(repo)
	if ref := strings.TrimSpace(ref); ref != "" {
		// Only append @ref if the repo slug/path doesn't already include @ref.
		if idx := strings.LastIndexByte(raw, '@'); idx < 0 {
			raw += "@" + ref
		}
	}
	repoURL, parsedRef, err := ParseGitSource(raw)
	if err != nil {
		return ExtensionInfo{}, err
	}
	ctx := m.managerContext()
	prep, err := prepareGitInstall(ctx, m.cfg.DataDir, repoURL, parsedRef, false, nil)
	if err != nil {
		return ExtensionInfo{}, err
	}
	defer prep.cleanup(m.cfg.DataDir)
	return m.registerInstalled(prep.man, prep.spec, func() error {
		return commitGitInstall(m.cfg.DataDir, prep)
	})
}

// InstallLocal validates a local extension directory (without building) and marks it StateWaiting.
func (m *Manager) InstallLocal(dir string) (ExtensionInfo, error) {
	if m.isClosed() {
		return ExtensionInfo{}, errors.New("extension manager is closed")
	}
	m.installMu.Lock()
	defer m.installMu.Unlock()
	if m.isClosed() {
		return ExtensionInfo{}, errors.New("extension manager is closed")
	}
	prep, err := prepareLocalInstall(dir)
	if err != nil {
		return ExtensionInfo{}, err
	}
	return m.registerInstalled(prep.man, prep.spec, func() error {
		return commitLocalInstall(m.cfg.DataDir, prep)
	})
}

// InstallImage stages a prebuilt OCI image extension (without pulling) and marks it StateWaiting.
func (m *Manager) InstallImage(ref string) (ExtensionInfo, error) {
	if m.isClosed() {
		return ExtensionInfo{}, errors.New("extension manager is closed")
	}
	m.installMu.Lock()
	defer m.installMu.Unlock()
	if m.isClosed() {
		return ExtensionInfo{}, errors.New("extension manager is closed")
	}
	ref = strings.TrimSpace(ref)
	if err := ValidateImageRef(ref); err != nil {
		return ExtensionInfo{}, err
	}
	id := deriveImageExtensionID(ref)
	tomlData := fmt.Sprintf(`schema_version = 1
id = %q
version = "0.1.0"
publisher = "oci"
name = %q

[runtime]
type = "sandbox"
image = %q
`, id, id, ref)
	man, err := ParseManifest([]byte(tomlData))
	if err != nil {
		return ExtensionInfo{}, err
	}
	sp := SourceSpec{
		Kind:     SourceImage,
		ImageRef: ref,
	}
	return m.registerInstalled(man, sp, func() error {
		if err := WriteFileAtomicWithin(m.cfg.DataDir, ManifestSnapshotPath(m.cfg.DataDir, man.ID), []byte(tomlData), 0o600); err != nil {
			return err
		}
		return SaveSourceWithin(m.cfg.DataDir, SourcePath(m.cfg.DataDir, man.ID), sp)
	})
}

func deriveImageExtensionID(ref string) string {
	base := ref
	if i := strings.IndexByte(base, '@'); i >= 0 {
		base = base[:i]
	}
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.IndexByte(base, ':'); i >= 0 {
		base = base[:i]
	}
	base = strings.ToLower(base)
	var b strings.Builder
	for i := 0; i < len(base); i++ {
		c := base[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-")
	for strings.Contains(id, "--") {
		id = strings.ReplaceAll(id, "--", "-")
	}
	if strings.HasPrefix(id, "gvx-") {
		id = "img-" + id
	}
	if len(id) > 64 {
		id = strings.TrimRight(id[:64], "-")
	}
	if !ValidID(id) {
		return "oci-extension"
	}
	return id
}

func (m *Manager) registerInstalled(man *Manifest, sp SourceSpec, commitDisk func() error) (ExtensionInfo, error) {
	for {
		m.mu.Lock()
		rec := m.exts[man.ID]
		existed := rec != nil
		if m.closed {
			m.mu.Unlock()
			return ExtensionInfo{}, errors.New("extension manager is closed")
		}
		if rec == nil {
			rec = m.newBaseRecord(man.ID)
			m.exts[man.ID] = rec
		}
		m.mu.Unlock()

		rec.opMu.Lock()
		m.mu.Lock()
		if m.closed {
			if m.exts[man.ID] == rec && (!existed || (rec.manifest == nil && rec.manifestErr == "")) {
				delete(m.exts, man.ID)
			}
			m.mu.Unlock()
			rec.opMu.Unlock()
			return ExtensionInfo{}, errors.New("extension manager is closed")
		}
		if m.exts[man.ID] != rec {
			m.mu.Unlock()
			rec.opMu.Unlock()
			continue
		}
		m.mu.Unlock()

		m.stopLocked(rec, 5*time.Second)

		if m.isClosed() {
			var changed bool
			m.mu.Lock()
			if m.exts[man.ID] == rec {
				if !existed || (rec.manifest == nil && rec.manifestErr == "") {
					delete(m.exts, man.ID)
				} else {
					changed = m.markStoppedOnErrorLocked(rec, nil)
				}
			}
			m.mu.Unlock()
			rec.opMu.Unlock()
			if changed {
				m.notifyChanged()
			}
			return ExtensionInfo{}, errors.New("extension manager is closed")
		}

		if commitDisk != nil {
			if err := commitDisk(); err != nil {
				var changed bool
				m.mu.Lock()
				if m.exts[man.ID] == rec {
					if !existed || (rec.manifest == nil && rec.manifestErr == "") {
						delete(m.exts, man.ID)
					} else {
						changed = m.markStoppedOnErrorLocked(rec, err)
					}
				}
				m.mu.Unlock()
				rec.opMu.Unlock()
				if !existed {
					_ = removeAllNoSymlinkWithin(m.cfg.DataDir, ExtensionDir(m.cfg.DataDir, man.ID))
				}
				if changed {
					m.notifyChanged()
				}
				return ExtensionInfo{}, err
			}
		}

		m.mu.Lock()
		if m.closed {
			if m.exts[man.ID] == rec && (!existed || (rec.manifest == nil && rec.manifestErr == "")) {
				delete(m.exts, man.ID)
			}
			m.mu.Unlock()
			rec.opMu.Unlock()
			if !existed {
				_ = removeAllNoSymlinkWithin(m.cfg.DataDir, ExtensionDir(m.cfg.DataDir, man.ID))
			}
			return ExtensionInfo{}, errors.New("extension manager is closed")
		}
		if m.exts[man.ID] != rec {
			m.mu.Unlock()
			rec.opMu.Unlock()
			continue
		}
		rec.manifest = man
		rec.manifestErr = ""
		rec.source = sp
		rec.crashes = 0
		rec.idleStopped = false
		rec.errMsg = ""
		rec.settings.MigrateLegacySecrets(man)
		m.recomputeStaticStateLocked(rec)
		info := m.buildInfoLocked(rec)
		m.mu.Unlock()
		rec.opMu.Unlock()

		m.notifyChanged()
		return info, nil
	}
}

// Approve approves permissions for id and builds/starts the extension.
func (m *Manager) Approve(id string) error {
	return m.AllowAndBuild(id)
}

// AllowAndBuild approves an extension's manifest permissions, builds or pulls its
// container image if applicable, and starts the extension runtime.
func (m *Manager) AllowAndBuild(id string) error {
	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return err
	}
	defer rec.opMu.Unlock()

	if err := m.grants.LoadError(); err != nil {
		return fmt.Errorf("cannot approve extension: %w", err)
	}
	if rec.manifestErr != "" || rec.manifest == nil {
		return fmt.Errorf("extension %q manifest is invalid: %s", id, rec.manifestErr)
	}
	for _, p := range []string{
		ExtensionDir(m.cfg.DataDir, id),
		ExtensionDataDir(m.cfg.DataDir, id),
		FilesDir(m.cfg.DataDir, id),
		LogsDir(m.cfg.DataDir, id),
	} {
		if err := CheckNoSymlinkWithin(m.cfg.DataDir, p); err != nil {
			return err
		}
	}

	m.stopLocked(rec, 5*time.Second)
	if err := m.grants.Approve(id, rec.manifest, rec.source.Commit); err != nil {
		return m.failAfterStop(rec, err)
	}

	m.mu.Lock()
	rec.crashes = 0
	rec.idleStopped = false
	rec.errMsg = ""
	m.mu.Unlock()

	if err := m.buildOrPullLocked(m.managerContext(), rec); err != nil {
		m.mu.Lock()
		if m.closed {
			if rec.state == StateBuilding || rec.state.IsActive() {
				rec.state = StateDiscovered
				rec.errMsg = ""
			}
		} else {
			rec.state = StateFailed
			rec.errMsg = err.Error()
		}
		m.mu.Unlock()
		m.notifyChanged()
		return err
	}

	if m.grants.Get(id).Disabled {
		m.mu.Lock()
		if m.closed && (rec.state == StateBuilding || rec.state.IsActive()) {
			rec.state = StateDiscovered
			rec.errMsg = ""
		} else {
			rec.state = StateDisabled
		}
		m.mu.Unlock()
		m.notifyChanged()
		return nil
	}

	if rec.manifest.Runtime.Type == RuntimeBinary && (m.cfg.DeveloperMode == nil || !m.cfg.DeveloperMode()) {
		const msg = "Developer Mode is required for binary extensions"
		m.mu.Lock()
		rec.state = StateDisabled
		rec.errMsg = msg
		m.mu.Unlock()
		m.notifyChanged()
		return errors.New(msg)
	}

	return m.startLocked(m.managerContext(), rec)
}

// Rebuild stops id if running, rebuilds or re-pulls its image, and restarts it.
func (m *Manager) Rebuild(id string) error {
	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return err
	}
	defer rec.opMu.Unlock()

	if rec.manifestErr != "" || rec.manifest == nil {
		return fmt.Errorf("extension %q manifest is invalid", id)
	}
	entry := m.grants.Get(id)
	if waiting, denied, _ := NeedsApproval(rec.manifest, entry, rec.source.Commit); waiting || denied {
		return fmt.Errorf("extension %q requires permission approval before rebuild", id)
	}

	m.stopLocked(rec, 5*time.Second)
	m.mu.Lock()
	rec.crashes = 0
	rec.idleStopped = false
	rec.errMsg = ""
	m.mu.Unlock()

	if err := m.buildOrPullLocked(m.managerContext(), rec); err != nil {
		m.mu.Lock()
		if m.closed {
			if rec.state == StateBuilding || rec.state.IsActive() {
				rec.state = StateDiscovered
				rec.errMsg = ""
			}
		} else {
			rec.state = StateFailed
			rec.errMsg = err.Error()
		}
		m.mu.Unlock()
		m.notifyChanged()
		return err
	}
	if entry.Disabled {
		m.mu.Lock()
		if m.closed && (rec.state == StateBuilding || rec.state.IsActive()) {
			rec.state = StateDiscovered
			rec.errMsg = ""
		} else {
			rec.state = StateDisabled
		}
		m.mu.Unlock()
		m.notifyChanged()
		return nil
	}
	return m.startLocked(m.managerContext(), rec)
}

// Update fetches the latest commit for a Git-installed extension, commits the
// updated checkout, and rebuilds/restarts it when no new permissions were added.
// If the updated manifest requests new ungranted permissions, the extension enters
// StateWaiting and needsApproval is returned as true.
func (m *Manager) Update(id string) (updated bool, needsApproval bool, err error) {
	return m.updateGit(id, false)
}

func (m *Manager) updateGit(id string, allowLocal bool) (updated bool, needsApproval bool, err error) {
	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return false, false, err
	}
	defer rec.opMu.Unlock()

	m.mu.Lock()
	src := rec.source
	m.mu.Unlock()

	if src.Kind != SourceGit || strings.TrimSpace(src.Repo) == "" {
		return false, false, fmt.Errorf("extension %q was not installed from a Git repository", id)
	}

	ctx := m.managerContext()
	var hasUpdate bool
	if allowLocal {
		_, hasUpdate, err = checkGitUpdate(ctx, src.Repo, src.Ref, src.Commit)
	} else {
		_, hasUpdate, err = CheckGitUpdate(ctx, src.Repo, src.Ref, src.Commit)
	}
	if err != nil {
		return false, false, err
	}
	if !hasUpdate {
		return false, false, nil
	}

	prep, err := prepareGitInstall(ctx, m.cfg.DataDir, src.Repo, src.Ref, allowLocal, nil)
	if err != nil {
		return false, false, err
	}
	defer prep.cleanup(m.cfg.DataDir)

	if prep.man.ID != id {
		return false, false, fmt.Errorf("updated manifest id %q does not match installed extension %q", prep.man.ID, id)
	}

	m.stopLocked(rec, 5*time.Second)

	if err := commitGitInstall(m.cfg.DataDir, prep); err != nil {
		return false, false, err
	}

	m.mu.Lock()
	rec.manifest = prep.man
	rec.manifestErr = ""
	rec.source = prep.spec
	rec.crashes = 0
	rec.idleStopped = false
	rec.errMsg = ""
	m.mu.Unlock()

	entry := m.grants.Get(id)
	_, _, asked := NeedsApproval(prep.man, entry, prep.spec.Commit)
	if len(asked) > 0 || entry.ApprovedManifestHash == "" {
		m.mu.Lock()
		rec.state = StateWaiting
		m.mu.Unlock()
		m.notifyChanged()
		return true, true, nil
	}

	if err := m.grants.Approve(id, prep.man, prep.spec.Commit); err != nil {
		return true, false, err
	}

	if err := m.buildOrPullLocked(ctx, rec); err != nil {
		m.mu.Lock()
		if m.closed {
			if rec.state == StateBuilding || rec.state.IsActive() {
				rec.state = StateDiscovered
				rec.errMsg = ""
			}
		} else {
			rec.state = StateFailed
			rec.errMsg = err.Error()
		}
		m.mu.Unlock()
		m.notifyChanged()
		return true, false, err
	}
	if entry.Disabled {
		m.mu.Lock()
		if m.closed && (rec.state == StateBuilding || rec.state.IsActive()) {
			rec.state = StateDiscovered
			rec.errMsg = ""
		} else {
			rec.state = StateDisabled
		}
		m.mu.Unlock()
		m.notifyChanged()
		return true, false, nil
	}
	return true, false, m.startLocked(ctx, rec)
}

func (m *Manager) buildOrPullLocked(ctx context.Context, rec *extRecord) error {
	if rec.manifest == nil || rec.manifest.Runtime.Type != RuntimeSandbox {
		return nil
	}
	m.mu.Lock()
	if m.closed || m.exts[rec.id] != rec {
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return errors.New("extension manager is closed")
		}
		return fmt.Errorf("unknown extension %q", rec.id)
	}
	rec.state = StateBuilding
	rec.buildLogs = nil
	m.mu.Unlock()
	m.notifyChanged()

	onLine := func(line string) {
		m.mu.Lock()
		rec.appendBuildLog(line)
		m.mu.Unlock()
		m.signalChange()
	}

	if rec.manifest.Runtime.Dockerfile != "" && (rec.source.Kind == SourceGit || rec.source.Kind == SourceLocal) {
		srcDir := rec.sourceDir(m.cfg.DataDir)
		res, err := BuildFromDir(ctx, m.cfg.ImageBuilder, srcDir, rec.manifest, rec.source.Commit, onLine)
		if err != nil {
			return err
		}
		m.mu.Lock()
		rec.source.ImageID = res.ImageID
		if len(res.Tags) > 0 {
			rec.source.ImageRef = res.Tags[0]
		}
		rec.source.BuiltAt = time.Now().UTC().Format(time.RFC3339)
		sp := rec.source
		m.mu.Unlock()
		return SaveSourceWithin(m.cfg.DataDir, SourcePath(m.cfg.DataDir, rec.id), sp)
	}

	imgRef := rec.source.ImageRef
	if imgRef == "" {
		imgRef = rec.manifest.Runtime.Image
	}
	if imgRef == "" {
		return errors.New("sandbox extension has neither dockerfile nor image")
	}
	if err := AcquireImage(ctx, m.cfg.ImagePuller, imgRef, onLine); err != nil {
		return err
	}
	m.mu.Lock()
	rec.source.ImageRef = imgRef
	sp := rec.source
	m.mu.Unlock()
	return SaveSourceWithin(m.cfg.DataDir, SourcePath(m.cfg.DataDir, rec.id), sp)
}

// Deny records permission denial for id and transitions it to StateWaiting.
func (m *Manager) Deny(id string) error {
	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return err
	}
	defer rec.opMu.Unlock()

	m.stopLocked(rec, 5*time.Second)
	rec.policyMu.Lock()
	denyErr := m.grants.Deny(id, rec.manifest)
	if denyErr == nil {
		m.enforceOwnedChildSandboxMounts(id, false)
		m.enforceOwnedDockerContainerMounts(id, false)
	}
	rec.policyMu.Unlock()
	if denyErr != nil {
		return m.failAfterStop(rec, denyErr)
	}
	m.mu.Lock()
	rec.idleStopped = false
	rec.errMsg = ""
	m.recomputeStaticStateLocked(rec)
	m.mu.Unlock()
	m.notifyChanged()
	return nil
}

// RevokePermissions stops id and clears its granted permissions while preserving AllowedFolders and Disabled.
func (m *Manager) RevokePermissions(id string) error {
	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return err
	}
	defer rec.opMu.Unlock()

	m.stopLocked(rec, 5*time.Second)
	rec.policyMu.Lock()
	revErr := m.grants.RevokePermissions(id)
	if revErr == nil {
		m.enforceOwnedChildSandboxMounts(id, false)
		m.enforceOwnedDockerContainerMounts(id, false)
	}
	rec.policyMu.Unlock()
	if revErr != nil {
		return m.failAfterStop(rec, revErr)
	}
	m.mu.Lock()
	rec.idleStopped = false
	rec.errMsg = ""
	m.recomputeStaticStateLocked(rec)
	m.mu.Unlock()
	m.notifyChanged()
	return nil
}

// Enable clears the disabled flag, resets crash backoff, and starts id if approved.
func (m *Manager) Enable(id string) error {
	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return err
	}
	defer rec.opMu.Unlock()

	if rec.manifestErr != "" || rec.manifest == nil {
		return fmt.Errorf("extension %q manifest is invalid", id)
	}
	if err := m.grants.SetDisabled(id, false); err != nil {
		return err
	}
	m.mu.Lock()
	rec.crashes = 0
	rec.idleStopped = false
	rec.errMsg = ""
	m.mu.Unlock()

	entry := m.grants.Get(id)
	if waiting, denied, _ := NeedsApproval(rec.manifest, entry, rec.source.Commit); waiting || denied {
		m.mu.Lock()
		rec.state = StateWaiting
		m.mu.Unlock()
		m.notifyChanged()
		return fmt.Errorf("extension %q requires permission approval", id)
	}
	if rec.manifest.Runtime.Type == RuntimeBinary && (m.cfg.DeveloperMode == nil || !m.cfg.DeveloperMode()) {
		const msg = "Developer Mode is required for binary extensions"
		m.mu.Lock()
		rec.state = StateDisabled
		rec.errMsg = msg
		m.mu.Unlock()
		m.notifyChanged()
		return errors.New(msg)
	}
	return m.startLocked(m.managerContext(), rec)
}

// Disable stops id cleanly and sets it to StateDisabled.
func (m *Manager) Disable(id string) error {
	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return err
	}
	defer rec.opMu.Unlock()

	m.stopLocked(rec, 5*time.Second)
	if err := m.grants.SetDisabled(id, true); err != nil {
		return m.failAfterStop(rec, err)
	}
	m.mu.Lock()
	rec.idleStopped = false
	rec.errMsg = ""
	rec.state = StateDisabled
	m.mu.Unlock()
	m.notifyChanged()
	return nil
}

// Uninstall stops id, removes its installed files and grants, and optionally purges data and keychain secrets.
func (m *Manager) Uninstall(id string, purgeData bool) error {
	if m.isClosed() {
		return errors.New("extension manager is closed")
	}
	m.installMu.Lock()
	defer m.installMu.Unlock()

	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return err
	}
	defer rec.opMu.Unlock()

	m.stopLocked(rec, 5*time.Second)
	rec.policyMu.Lock()
	m.cleanupOwnedSandboxes(id, false)
	m.enforceOwnedDockerContainerMounts(id, false)
	rec.policyMu.Unlock()

	if err := removeAllNoSymlinkWithin(m.cfg.DataDir, ExtensionDir(m.cfg.DataDir, id)); err != nil {
		return m.failAfterStop(rec, err)
	}
	if err := m.grants.Remove(id); err != nil {
		return m.failAfterStop(rec, err)
	}
	if purgeData {
		if rec.vault != nil {
			if err := rec.vault.Purge(); err != nil {
				return m.failAfterStop(rec, err)
			}
		}
		if err := removeAllNoSymlinkWithin(m.cfg.DataDir, ExtensionDataDir(m.cfg.DataDir, id)); err != nil {
			return m.failAfterStop(rec, err)
		}
	}

	m.mu.Lock()
	delete(m.exts, id)
	m.mu.Unlock()
	m.notifyChanged()
	return nil
}

// RemoveAllowedFolder removes dir from id's granted allowed_folders and restarts
// a running sandbox extension so the bind mount is removed.
func (m *Manager) RemoveAllowedFolder(id, dir string) error {
	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return err
	}
	defer rec.opMu.Unlock()

	rec.policyMu.Lock()
	removed, err := m.grants.RemoveAllowedFolder(id, dir)
	if err == nil && removed {
		m.enforceOwnedChildSandboxMounts(id, true)
		m.enforceOwnedDockerContainerMounts(id, true)
	}
	rec.policyMu.Unlock()
	if err != nil {
		return err
	}
	if removed {
		m.mu.RLock()
		man := rec.manifest
		active := rec.state.IsActive()
		m.mu.RUnlock()
		restartSandbox := man != nil && man.Runtime.Type == RuntimeSandbox && active
		if restartSandbox {
			m.stopLocked(rec, 5*time.Second)
			if err := m.startLocked(m.managerContext(), rec); err != nil {
				return err
			}
		}
	}
	m.notifyChanged()
	return nil
}

// SetSetting validates and saves a setting value for id, notifying a running extension.
func (m *Manager) SetSetting(id, key string, val any) error {
	rec, err := m.acquireActiveRecord(id)
	if err != nil {
		return err
	}
	m.mu.RLock()
	man := rec.manifest
	settings := rec.settings
	m.mu.RUnlock()
	if man == nil {
		rec.opMu.Unlock()
		return fmt.Errorf("extension %q has no valid manifest", id)
	}
	var decl SettingDecl
	found := false
	for _, s := range man.Settings {
		if s.Key == key {
			decl = s
			found = true
			break
		}
	}
	if !found {
		rec.opMu.Unlock()
		return fmt.Errorf("unknown setting %q for extension %q", key, id)
	}
	text, err := settingValueToString(val)
	if err != nil {
		rec.opMu.Unlock()
		return err
	}
	if err := settings.Set(decl, text); err != nil {
		rec.opMu.Unlock()
		return err
	}
	masked, err := settings.ForHost(man)
	if err == nil {
		m.mu.RLock()
		var (
			rt   extRuntime
			hapi *hostAPIServer
		)
		if !m.closed && m.exts[id] == rec {
			rt = rec.runtime
			hapi = rec.hostAPI
		}
		m.mu.RUnlock()
		if rt != nil {
			if c := rt.Client(); c != nil {
				sctx, cancel := context.WithTimeout(m.managerContext(), 5*time.Second)
				_ = c.SettingsChanged(sctx, masked)
				cancel()
			}
		}
		if hapi != nil {
			hapi.cfg.events.publish("settings.changed", masked)
		}
	}
	rec.opMu.Unlock()
	m.notifyChanged()
	return nil
}

func settingValueToString(val any) (string, error) {
	if val == nil {
		return "", nil
	}
	switch v := val.(type) {
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	default:
		return fmt.Sprint(v), nil
	}
}

// SubscribeView opens an SSE stream to viewID on extension id, waking an idle-stopped
// extension if needed, and delivers validated uiblock.Update frames on the returned channel.
func (m *Manager) SubscribeView(ctx context.Context, id, viewID, locale string) (<-chan uiblock.Update, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if locale == "" {
		locale = "en"
	}
	rec, man, err := m.getRecordManifest(id)
	if err != nil {
		return nil, err
	}
	if man == nil || !hasView(man, viewID) {
		return nil, fmt.Errorf("extension %q has no view %q", id, viewID)
	}
	client, release, err := m.ensureRunningClient(ctx, rec, true)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		release()
		return nil, errors.New("extension manager is closed")
	}
	if m.exts[id] != rec {
		m.mu.Unlock()
		release()
		return nil, fmt.Errorf("unknown extension %q", id)
	}
	switch rec.state {
	case StateDisabled, StateWaiting, StateFailed, StateInvalid:
		state := rec.state
		m.mu.Unlock()
		release()
		return nil, fmt.Errorf("extension %q is not runnable (state=%s)", id, state)
	}
	var initialClient *extClient
	if rec.runtime != nil && (rec.state == StateRunning || rec.state == StateDegraded) {
		if c := rec.runtime.Client(); c != nil {
			initialClient = c
		}
	}
	if initialClient == nil && rec.runtime != nil && rec.runtime.Client() == client {
		initialClient = client
	}
	baseCtx := m.baseCtx
	m.wg.Add(1)
	m.mu.Unlock()

	pathStr := "/v1/views/" + url.PathEscape(viewID) + "/stream?locale=" + url.QueryEscape(locale)
	ch := make(chan uiblock.Update, 16)
	go func() {
		defer m.wg.Done()
		defer close(ch)
		defer release()

		if baseCtx == nil {
			baseCtx = context.Background()
		}
		streamCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-baseCtx.Done():
				cancel()
			case <-streamCtx.Done():
			}
		}()

		send := func(upd uiblock.Update) error {
			select {
			case ch <- upd:
				return nil
			case <-streamCtx.Done():
				return streamCtx.Err()
			}
		}

		backoff := 50 * time.Millisecond
		const maxBackoff = 2 * time.Second
		curClient := initialClient

		for {
			if streamCtx.Err() != nil {
				return
			}
			if curClient != nil {
				needReconnect, err := curClient.streamViewOnce(streamCtx, pathStr, send, func() {
					backoff = 50 * time.Millisecond
				})
				if streamCtx.Err() != nil {
					return
				}
				if err == nil && !needReconnect {
					return
				}
				var httpErr *httpStatusError
				if errors.As(err, &httpErr) && !needReconnect {
					// If the runtime restarted and rotated its Bearer token (401 on a reused socket),
					// check whether the active runtime client changed before treating 401 as terminal.
					if httpErr.code == http.StatusUnauthorized {
						nextClient, needWake, terminal := m.subscribeClientStatus(id, rec)
						if terminal {
							return
						}
						if needWake {
							woken, wRel, wErr := m.ensureRunningClient(streamCtx, rec, true)
							if wRel != nil {
								wRel()
							}
							if wErr == nil && woken != nil {
								curClient = woken
								backoff = 50 * time.Millisecond
								continue
							}
						} else if nextClient != curClient {
							curClient = nextClient
							backoff = 50 * time.Millisecond
							continue
						}
					}
					_ = send(uiblock.Update{Error: httpErr.Error()})
					return
				}
				if err != nil && !needReconnect {
					return
				}
			}

			nextClient, needWake, terminal := m.subscribeClientStatus(id, rec)
			if terminal {
				return
			}
			if needWake {
				woken, wRel, wErr := m.ensureRunningClient(streamCtx, rec, true)
				if wRel != nil {
					wRel()
				}
				if wErr == nil && woken != nil {
					curClient = woken
					backoff = 50 * time.Millisecond
					continue
				}
			} else if nextClient != nil && nextClient != curClient {
				curClient = nextClient
				backoff = 50 * time.Millisecond
				continue
			}

			timer := time.NewTimer(backoff)
			select {
			case <-streamCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}

			nextClient, needWake, terminal = m.subscribeClientStatus(id, rec)
			if terminal {
				return
			}
			if needWake {
				woken, wRel, wErr := m.ensureRunningClient(streamCtx, rec, true)
				if wRel != nil {
					wRel()
				}
				if wErr == nil {
					curClient = woken
				} else {
					curClient = nil
				}
			} else {
				curClient = nextClient
			}
		}
	}()
	return ch, nil
}

func (m *Manager) subscribeClientStatus(id string, rec *extRecord) (client *extClient, needWake bool, terminal bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed || m.exts[id] != rec {
		return nil, false, true
	}
	switch rec.state {
	case StateDisabled, StateWaiting, StateFailed, StateInvalid:
		return nil, false, true
	}
	if rec.runtime != nil && (rec.state == StateRunning || rec.state == StateDegraded) {
		return rec.runtime.Client(), false, false
	}
	if rec.runtime == nil && (rec.idleStopped || rec.state == StateDiscovered) {
		return nil, true, false
	}
	return nil, false, false
}

// SendViewEvent delivers a UI event to viewID on extension id with a short-lived X-GVX-Gesture-ID.
func (m *Manager) SendViewEvent(id, viewID string, ev uiblock.Event) error {
	rec, man, err := m.getRecordManifest(id)
	if err != nil {
		return err
	}
	if man == nil || !hasView(man, viewID) {
		return fmt.Errorf("extension %q has no view %q", id, viewID)
	}
	ctx := m.managerContext()
	client, release, err := m.ensureRunningClient(ctx, rec, false)
	if err != nil {
		return err
	}
	defer release()

	gid, err := m.gestures.mint(id)
	if err != nil {
		return err
	}
	defer m.gestures.revoke(gid)

	callErr := client.SendViewEvent(ctx, viewID, ev, gid)
	m.handlePendingSandboxRestart(rec)
	return callErr
}

// RunCommand executes a command palette entry on extension id with a short-lived X-GVX-Gesture-ID.
func (m *Manager) RunCommand(id, cmdID, locale string) error {
	rec, man, err := m.getRecordManifest(id)
	if err != nil {
		return err
	}
	if man == nil || !hasCommand(man, cmdID) {
		return fmt.Errorf("extension %q has no command %q", id, cmdID)
	}
	ctx := m.managerContext()
	client, release, err := m.ensureRunningClient(ctx, rec, false)
	if err != nil {
		return err
	}
	defer release()

	gid, err := m.gestures.mint(id)
	if err != nil {
		return err
	}
	defer m.gestures.revoke(gid)

	callErr := client.RunCommand(ctx, cmdID, locale, gid)
	m.handlePendingSandboxRestart(rec)
	return callErr
}

// InvokeTool invokes a contributed MCP tool on extension id (without a gesture token).
func (m *Manager) InvokeTool(ctx context.Context, id, toolID string, args json.RawMessage) (json.RawMessage, error) {
	if ctx == nil {
		ctx = m.managerContext()
	}
	rec, man, err := m.getRecordManifest(id)
	if err != nil {
		return nil, err
	}
	if man == nil || !hasTool(man, toolID) {
		return nil, fmt.Errorf("extension %q has no tool %q", id, toolID)
	}
	client, release, err := m.ensureRunningClient(ctx, rec, false)
	if err != nil {
		return nil, err
	}
	defer release()

	return client.InvokeTool(ctx, toolID, args, "en")
}

// NotifyHostEvent broadcasts an SSE event (host.theme, host.locale, docker.state, etc.)
// to all connected extensions.
func (m *Manager) NotifyHostEvent(eventType string, data any) {
	_ = m.notifyHostEventCount(eventType, data)
}

func (m *Manager) notifyHostEventCount(eventType string, data any) int {
	m.mu.RLock()
	hubs := make([]*hostEventHub, 0, len(m.exts))
	for _, rec := range m.exts {
		if rec.hostAPI != nil {
			hubs = append(hubs, rec.hostAPI.cfg.events)
		}
	}
	m.mu.RUnlock()
	delivered := 0
	for _, h := range hubs {
		delivered += h.publishCount(eventType, data)
	}
	return delivered
}

// List returns ExtensionInfo snapshots for all known extensions sorted by ID.
func (m *Manager) List() []ExtensionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ExtensionInfo, 0, len(m.exts))
	for _, rec := range m.exts {
		out = append(out, m.buildInfoLocked(rec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// KeepImages returns OCI image references belonging to active/installed non-disabled
// sandbox extensions so engine image pruning preserves them.
func (m *Manager) KeepImages() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	seen := map[string]bool{}
	var out []string
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}

	for _, rec := range m.exts {
		if rec.state == StateInvalid || rec.state == StateWaiting || rec.manifest == nil || rec.manifest.Runtime.Type != RuntimeSandbox {
			continue
		}
		entry := m.grants.Get(rec.id)
		if entry.Disabled {
			continue
		}
		if waiting, denied, _ := NeedsApproval(rec.manifest, entry, rec.source.Commit); waiting || denied {
			continue
		}
		if rec.source.ImageRef != "" {
			add(rec.source.ImageRef)
		}
		if rec.source.Kind != SourceImage && rec.manifest.Runtime.Dockerfile != "" && rec.source.ImageID != "" {
			add(ExtensionImageTag(rec.id, rec.manifest.Version, rec.source.Commit))
		}
	}
	sort.Strings(out)
	return out
}

// LocalProxyRoutes returns active localproxy HTTP routes for running extensions with
// localproxy permission and [contributes.web_ui].
func (m *Manager) LocalProxyRoutes() []localproxy.Route {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []localproxy.Route
	for _, rec := range m.exts {
		if rec.state != StateRunning && rec.state != StateDegraded {
			continue
		}
		if rec.manifest == nil || rec.manifest.WebUI == nil || rec.runtime == nil {
			continue
		}
		if !m.grants.HasPermission(rec.id, PermLocalProxy, rec.manifest.Permissions) {
			continue
		}
		target := rec.runtime.WebUITarget()
		if target == "" {
			continue
		}
		out = append(out, localproxy.Route{
			Host:      localproxy.BuildHost(rec.id),
			Target:    target,
			Kind:      "extension",
			Name:      rec.manifest.Name,
			GuestPort: rec.manifest.WebUI.Port,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// Logs returns recent stdout/stderr log lines for extension id.
func (m *Manager) Logs(id string) []string {
	m.mu.RLock()
	rec := m.exts[id]
	var rt extRuntime
	if rec != nil {
		rt = rec.runtime
	}
	m.mu.RUnlock()
	if rt != nil {
		if lines := rt.Logs(); len(lines) > 0 {
			return lines
		}
	}
	b, err := ReadFileNoSymlinkWithin(m.cfg.DataDir, filepath.Join(LogsDir(m.cfg.DataDir, id), "extension.log"))
	if err != nil {
		return nil
	}
	raw := strings.TrimRight(string(b), "\n")
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > maxExtMemLogLines {
		lines = lines[len(lines)-maxExtMemLogLines:]
	}
	return lines
}

// BuildLogs returns captured build/pull output lines for extension id.
func (m *Manager) BuildLogs(id string) []BuildLogEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec := m.exts[id]
	if rec == nil {
		return nil
	}
	return slices.Clone(rec.buildLogs)
}

// SecretStoreStatus reports whether secrets use an OS keychain backend or file fallback.
func (m *Manager) SecretStoreStatus() SecretStoreStatus {
	return SecretStoreStatusFor(m.cfg.Keychain, m.cfg.KeychainNote)
}

// Close stops all running extensions, cleans ephemeral sandboxes, and closes IPC servers.
func (m *Manager) Close(timeout time.Duration) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	cancel := m.cancel
	recs := make([]*extRecord, 0, len(m.exts))
	for _, r := range m.exts {
		recs = append(recs, r)
	}
	m.mu.Unlock()

	if timeout <= 0 {
		timeout = 8 * time.Second
	}

	if delivered := m.notifyHostEventCount("app.stopping", map[string]any{}); delivered > 0 {
		grace := min(75*time.Millisecond, timeout/8)
		if grace > 0 {
			time.Sleep(grace)
		}
	}
	if cancel != nil {
		cancel()
	}

	var stopWG sync.WaitGroup
	for _, rec := range recs {
		stopWG.Add(1)
		go func(r *extRecord) {
			defer stopWG.Done()
			r.opMu.Lock()
			m.stopLocked(r, timeout)
			var changed bool
			m.mu.Lock()
			if r.state.IsActive() || r.state == StateBuilding {
				r.state = StateDiscovered
				r.errMsg = ""
				changed = true
			}
			m.mu.Unlock()
			r.policyMu.Lock()
			m.cleanupOwnedSandboxes(r.id, true)
			r.policyMu.Unlock()
			r.opMu.Unlock()
			if changed {
				m.notifyChanged()
			}
		}(rec)
	}
	stopWG.Wait()
	m.wg.Wait()
	return nil
}

// --- Internal lifecycle helpers ---

func (m *Manager) isClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.closed
}

func (m *Manager) managerContext() context.Context {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.baseCtx != nil {
		return m.baseCtx
	}
	return context.Background()
}

func (m *Manager) acquireActiveRecord(id string) (*extRecord, error) {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil, errors.New("extension manager is closed")
	}
	rec := m.exts[id]
	m.mu.RUnlock()
	if rec == nil {
		return nil, fmt.Errorf("unknown extension %q", id)
	}

	rec.opMu.Lock()
	m.mu.RLock()
	closed := m.closed
	cur := m.exts[id]
	m.mu.RUnlock()
	if closed {
		rec.opMu.Unlock()
		return nil, errors.New("extension manager is closed")
	}
	if cur != rec {
		rec.opMu.Unlock()
		return nil, fmt.Errorf("unknown extension %q", id)
	}
	return rec, nil
}

func (m *Manager) lockActiveRecord(rec *extRecord) bool {
	if rec == nil {
		return false
	}
	rec.opMu.Lock()
	m.mu.RLock()
	ok := !m.closed && m.exts[rec.id] == rec
	m.mu.RUnlock()
	if !ok {
		rec.opMu.Unlock()
		return false
	}
	return true
}

func (m *Manager) getRecordManifest(id string) (*extRecord, *Manifest, error) {
	m.mu.RLock()
	rec := m.exts[id]
	var man *Manifest
	if rec != nil {
		man = rec.manifest
	}
	m.mu.RUnlock()
	if rec == nil {
		return nil, nil, fmt.Errorf("unknown extension %q", id)
	}
	return rec, man, nil
}

func (m *Manager) startLocked(ctx context.Context, rec *extRecord) error {
	m.mu.Lock()
	closed := m.closed
	current := m.exts[rec.id] == rec
	alreadyRunning := rec.runtime != nil
	if closed || !current {
		changed := false
		if rec.state == StateBuilding || rec.state.IsActive() {
			rec.state = StateDiscovered
			rec.errMsg = ""
			changed = true
		}
		m.mu.Unlock()
		if changed {
			m.notifyChanged()
		}
		if closed {
			return errors.New("extension manager is closed")
		}
		return fmt.Errorf("unknown extension %q", rec.id)
	}
	m.mu.Unlock()
	if rec.manifestErr != "" || rec.manifest == nil {
		return fmt.Errorf("extension %q has invalid manifest", rec.id)
	}
	if alreadyRunning {
		return nil
	}
	for _, p := range []string{
		ExtensionDir(m.cfg.DataDir, rec.id),
		ExtensionDataDir(m.cfg.DataDir, rec.id),
		FilesDir(m.cfg.DataDir, rec.id),
		LogsDir(m.cfg.DataDir, rec.id),
	} {
		if err := CheckNoSymlinkWithin(m.cfg.DataDir, p); err != nil {
			return m.failStartLocked(rec, err)
		}
	}

	m.mu.Lock()
	prevCancelWatch := rec.stopWatch
	rec.stopWatch = nil
	rec.state = StateStarting
	rec.errMsg = ""
	rec.idleStopped = false
	rec.lastActivity = time.Now()
	m.mu.Unlock()
	if prevCancelWatch != nil {
		prevCancelWatch()
	}
	m.notifyChanged()

	hostToken, err := randomHexToken(32)
	if err != nil {
		return m.failStartLocked(rec, err)
	}
	extToken, err := randomHexToken(32)
	if err != nil {
		return m.failStartLocked(rec, err)
	}

	var (
		dockerProxy    *scopedDockerProxy
		dockerEndpoint string
		dockerSockPath string
		dockerPort     int
	)
	if m.grants.HasPermission(rec.id, PermDocker, rec.manifest.Permissions) {
		dp := newScopedDockerProxyWithPolicyMu(
			rec.id,
			m.cfg.DataDir,
			m.grants,
			rec.manifest.Permissions,
			func() bool {
				return m.grants.HasPermission(rec.id, PermNetwork, rec.manifest.Permissions)
			},
			m.cfg.DockerBackendDial,
			m.cfg.DockerEngineHandler,
			&rec.policyMu,
		)
		dep, derr := dp.Listen()
		if derr != nil {
			_ = dp.Close()
			return m.failStartLocked(rec, fmt.Errorf("start scoped docker proxy: %w", derr))
		}
		dockerProxy = dp
		dockerEndpoint = dep
		dockerSockPath = dp.SocketPath()
		if ta, ok := dp.Addr().(*net.TCPAddr); ok {
			dockerPort = ta.Port
		}
	}

	events := newHostEventHub()
	hapi := newHostAPIServer(hostAPIConfig{
		extID:               rec.id,
		dataDir:             m.cfg.DataDir,
		token:               hostToken,
		manifest:            rec.manifest,
		grants:              m.grants,
		storage:             rec.storage,
		vault:               rec.vault,
		settings:            rec.settings,
		gestures:            m.gestures,
		hostInfoFn:          m.cfg.HostInfo,
		uiHost:              m.cfg.UIHost,
		supervisor:          m.cfg.Supervisor,
		sandboxesMu:         &m.sandboxesMu,
		policyMu:            &rec.policyMu,
		dockerSockFn:        func() string { return dockerEndpoint },
		dockerBackendDial:   m.cfg.DockerBackendDial,
		dockerEngineHandler: m.cfg.DockerEngineHandler,
		events:              events,
		onBadge: func(extID string, badge BadgeInfo) {
			m.mu.Lock()
			if r := m.exts[extID]; r != nil {
				r.badge = badge
			}
			m.mu.Unlock()
		},
		onFolderPicked: func(extID, _ string) {
			m.mu.Lock()
			if r := m.exts[extID]; r != nil && r.manifest != nil && r.manifest.Runtime.Type == RuntimeSandbox {
				r.pendingRestart = true
			}
			m.mu.Unlock()
		},
		onSandboxChange: func(_ string, info extsdk.SandboxInfo) {
			events.publish("sandbox.state", info)
		},
	})
	hostEndpoint, hostSockPath, hostPort, err := hapi.Listen()
	if err != nil {
		_ = hapi.Close()
		if dockerProxy != nil {
			_ = dockerProxy.Close()
		}
		return m.failStartLocked(rec, fmt.Errorf("start host API server: %w", err))
	}

	var rt extRuntime
	switch rec.manifest.Runtime.Type {
	case RuntimeSandbox:
		rt = newSandboxRuntime(sandboxRuntimeConfig{
			extID:          rec.id,
			dataDir:        m.cfg.DataDir,
			manifest:       rec.manifest,
			source:         rec.source,
			grants:         m.grants,
			supervisor:     m.cfg.Supervisor,
			hostEndpoint:   hostEndpoint,
			hostSockPath:   hostSockPath,
			hostPort:       hostPort,
			hostToken:      hostToken,
			extToken:       extToken,
			dockerEndpoint: dockerEndpoint,
			dockerSockPath: dockerSockPath,
			dockerPort:     dockerPort,
		})
	case RuntimeBinary:
		rt = newBinaryRuntime(binaryRuntimeConfig{
			extID:          rec.id,
			dataDir:        m.cfg.DataDir,
			manifest:       rec.manifest,
			source:         rec.source,
			grants:         m.grants,
			developerMode:  m.cfg.DeveloperMode,
			hostEndpoint:   hostEndpoint,
			hostToken:      hostToken,
			extToken:       extToken,
			dockerEndpoint: dockerEndpoint,
		})
	default:
		_ = hapi.Close()
		if dockerProxy != nil {
			_ = dockerProxy.Close()
		}
		return m.failStartLocked(rec, fmt.Errorf("unsupported runtime type %q", rec.manifest.Runtime.Type))
	}

	if err := rt.Start(ctx); err != nil {
		_ = rt.Stop(2 * time.Second)
		_ = hapi.Close()
		if dockerProxy != nil {
			_ = dockerProxy.Close()
		}
		return m.failStartLocked(rec, err)
	}

	watchCtx, cancelWatch := context.WithCancel(m.managerContext())
	m.mu.Lock()
	if m.closed || m.exts[rec.id] != rec {
		closed := m.closed
		changed := false
		if rec.state == StateBuilding || rec.state.IsActive() {
			rec.state = StateDiscovered
			rec.errMsg = ""
			changed = true
		}
		m.mu.Unlock()
		cancelWatch()
		_ = rt.Stop(2 * time.Second)
		_ = hapi.Close()
		if dockerProxy != nil {
			_ = dockerProxy.Close()
		}
		if changed {
			m.notifyChanged()
		}
		if closed {
			return errors.New("extension manager is closed")
		}
		return fmt.Errorf("unknown extension %q", rec.id)
	}
	rec.hostAPI = hapi
	rec.dockerProxy = dockerProxy
	rec.runtime = rt
	rec.stopWatch = cancelWatch
	rec.state = StateRunning
	rec.errMsg = ""
	rec.lastActivity = time.Now()
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		m.watchRuntime(watchCtx, cancelWatch, rec, rt)
	}()

	m.notifyChanged()
	return nil
}

func (m *Manager) failStartLocked(rec *extRecord, err error) error {
	m.mu.Lock()
	if m.closed {
		if rec.state.IsActive() || rec.state == StateBuilding {
			rec.state = StateDiscovered
			rec.errMsg = ""
		}
	} else {
		rec.errMsg = err.Error()
		if rec.manifest != nil && rec.manifest.Runtime.Type == RuntimeBinary && (m.cfg.DeveloperMode == nil || !m.cfg.DeveloperMode()) {
			rec.state = StateDisabled
		} else {
			rec.state = StateFailed
		}
	}
	m.mu.Unlock()
	m.notifyChanged()
	return err
}

func (m *Manager) markStoppedOnErrorLocked(rec *extRecord, err error) bool {
	if m.closed {
		if rec.state.IsActive() || rec.state == StateBuilding || rec.state == StateCrashed {
			rec.state = StateDiscovered
			rec.errMsg = ""
			return true
		}
		return false
	}
	if rec.state.IsActive() || rec.state == StateBuilding || rec.state == StateCrashed {
		rec.state = StateFailed
		if err != nil {
			rec.errMsg = err.Error()
		}
		return true
	}
	return false
}

func (m *Manager) failAfterStop(rec *extRecord, err error) error {
	m.mu.Lock()
	changed := m.markStoppedOnErrorLocked(rec, err)
	m.mu.Unlock()
	if changed {
		m.notifyChanged()
	}
	return err
}

func (m *Manager) stopLocked(rec *extRecord, timeout time.Duration) {
	m.teardownRuntimeLocked(rec, timeout, true)
}

func (m *Manager) teardownRuntimeLocked(rec *extRecord, timeout time.Duration, cancelWatcher bool) {
	m.mu.Lock()
	rt := rec.runtime
	hapi := rec.hostAPI
	dp := rec.dockerProxy
	var cancelWatch context.CancelFunc
	if cancelWatcher {
		cancelWatch = rec.stopWatch
		rec.stopWatch = nil
	}
	rec.runtime = nil
	rec.hostAPI = nil
	rec.dockerProxy = nil
	rec.pendingRestart = false
	m.mu.Unlock()

	if cancelWatch != nil {
		cancelWatch()
	}
	if rt != nil {
		_ = rt.Stop(timeout)
	}
	if hapi != nil {
		_ = hapi.Close()
	}
	if dp != nil {
		_ = dp.Close()
	}
}

func (m *Manager) watchRuntime(watchCtx context.Context, cancelWatch context.CancelFunc, rec *extRecord, rt extRuntime) {
	defer cancelWatch()

	var exitErr error
	select {
	case <-watchCtx.Done():
		return
	case exitErr = <-rt.Done():
	}

	rec.opMu.Lock()
	m.mu.Lock()
	stillCurrent := !m.closed && m.exts[rec.id] == rec && rec.runtime == rt
	m.mu.Unlock()
	if !stillCurrent || watchCtx.Err() != nil {
		rec.opMu.Unlock()
		return
	}

	// Tear down the exited runtime and IPC servers without cancelling watchCtx so
	// crash-backoff can sleep and restart unless externally stopped.
	m.teardownRuntimeLocked(rec, 2*time.Second, false)

	entry := m.grants.Get(rec.id)
	if entry.Disabled {
		m.mu.Lock()
		rec.state = StateDisabled
		rec.stopWatch = nil
		m.mu.Unlock()
		rec.opMu.Unlock()
		m.notifyChanged()
		return
	}

	m.mu.Lock()
	rec.crashes++
	crashes := rec.crashes
	errMsg := "extension process exited unexpectedly"
	if exitErr != nil {
		errMsg = exitErr.Error()
	}
	rec.errMsg = errMsg
	if crashes >= maxExtensionCrashes {
		rec.state = StateFailed
		rec.stopWatch = nil
		m.mu.Unlock()
		rec.opMu.Unlock()
		m.notifyChanged()
		return
	}
	rec.state = StateCrashed
	m.mu.Unlock()
	rec.opMu.Unlock()
	m.notifyChanged()

	baseCtx := m.managerContext()
	backoff := time.Duration(crashes) * 400 * time.Millisecond
	timer := time.NewTimer(backoff)
	select {
	case <-watchCtx.Done():
		timer.Stop()
		return
	case <-baseCtx.Done():
		timer.Stop()
		return
	case <-timer.C:
	}

	if !m.lockActiveRecord(rec) {
		return
	}
	defer rec.opMu.Unlock()
	m.mu.RLock()
	state := rec.state
	hasRuntime := rec.runtime != nil
	m.mu.RUnlock()
	if watchCtx.Err() != nil || m.grants.Get(rec.id).Disabled || hasRuntime || state != StateCrashed {
		return
	}
	_ = m.startLocked(m.managerContext(), rec)
}

func (m *Manager) ensureRunningClient(ctx context.Context, rec *extRecord, reserveView bool) (*extClient, func(), error) {
	if ctx == nil {
		ctx = m.managerContext()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			m.mu.Lock()
			if reserveView {
				if rec.activeViews > 0 {
					rec.activeViews--
				}
			} else {
				if rec.inFlight > 0 {
					rec.inFlight--
				}
			}
			rec.lastActivity = time.Now()
			m.mu.Unlock()
		})
	}

	m.mu.Lock()
	if m.closed || m.exts[rec.id] != rec {
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return nil, nil, errors.New("extension manager is closed")
		}
		return nil, nil, fmt.Errorf("unknown extension %q", rec.id)
	}
	if reserveView {
		rec.activeViews++
	} else {
		rec.inFlight++
	}
	rec.lastActivity = time.Now()
	rt := rec.runtime
	state := rec.state
	if rt != nil && (state == StateRunning || state == StateDegraded) {
		if c := rt.Client(); c != nil {
			m.mu.Unlock()
			return c, release, nil
		}
	}
	m.wg.Add(1)
	m.mu.Unlock()

	// Decouple cold-start lifecycle from the caller's ctx so cancelling one
	// short-timeout SubscribeView/InvokeTool call never aborts startLocked or
	// poisons the extension globally to StateFailed.
	done := make(chan error, 1)
	go func() {
		defer m.wg.Done()
		if !m.lockActiveRecord(rec) {
			if m.isClosed() {
				done <- errors.New("extension manager is closed")
			} else {
				done <- fmt.Errorf("unknown extension %q", rec.id)
			}
			return
		}
		defer rec.opMu.Unlock()

		m.mu.RLock()
		curRT := rec.runtime
		curState := rec.state
		idleStopped := rec.idleStopped
		m.mu.RUnlock()

		if curRT != nil && (curState == StateRunning || curState == StateDegraded) {
			if c := curRT.Client(); c != nil {
				done <- nil
				return
			}
		}
		if !curState.CanRun() && !idleStopped {
			done <- fmt.Errorf("extension %q is not runnable (state=%s)", rec.id, curState)
			return
		}
		done <- m.startLocked(m.managerContext(), rec)
	}()

	select {
	case <-ctx.Done():
		release()
		return nil, nil, ctx.Err()
	case err := <-done:
		if err != nil {
			release()
			return nil, nil, err
		}
	}

	m.mu.RLock()
	closed := m.closed
	current := m.exts[rec.id] == rec
	rt = rec.runtime
	var client *extClient
	if !closed && current && rt != nil {
		client = rt.Client()
	}
	m.mu.RUnlock()
	if closed {
		release()
		return nil, nil, errors.New("extension manager is closed")
	}
	if !current {
		release()
		return nil, nil, fmt.Errorf("unknown extension %q", rec.id)
	}
	if client == nil {
		release()
		return nil, nil, fmt.Errorf("extension %q client is unavailable", rec.id)
	}
	return client, release, nil
}

func (m *Manager) handlePendingSandboxRestart(rec *extRecord) {
	m.mu.Lock()
	need := rec.pendingRestart
	rec.pendingRestart = false
	closed := m.closed
	current := m.exts[rec.id] == rec
	m.mu.Unlock()
	if !need || closed || !current {
		return
	}
	if !m.lockActiveRecord(rec) {
		return
	}
	defer rec.opMu.Unlock()
	m.mu.RLock()
	man := rec.manifest
	active := rec.state.IsActive()
	m.mu.RUnlock()
	if man != nil && man.Runtime.Type == RuntimeSandbox && active {
		m.stopLocked(rec, 5*time.Second)
		_ = m.startLocked(m.managerContext(), rec)
	}
}

func (m *Manager) idleReaperLoop(ctx context.Context) {
	ticker := time.NewTicker(idleSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapIdleExtensions()
		}
	}
}

func (m *Manager) reapIdleExtensions() {
	now := time.Now()
	m.mu.RLock()
	var candidates []*extRecord
	for _, rec := range m.exts {
		if rec.state != StateRunning || rec.manifest == nil {
			continue
		}
		timeoutSec := rec.manifest.Runtime.IdleTimeoutSec
		if timeoutSec <= 0 {
			continue
		}
		if rec.activeViews > 0 || rec.inFlight > 0 {
			continue
		}
		if now.Sub(rec.lastActivity) >= time.Duration(timeoutSec)*time.Second {
			candidates = append(candidates, rec)
		}
	}
	m.mu.RUnlock()

	for _, rec := range candidates {
		if !m.lockActiveRecord(rec) {
			continue
		}
		m.mu.Lock()
		shouldStop := !m.closed &&
			m.exts[rec.id] == rec &&
			rec.state == StateRunning &&
			rec.manifest != nil &&
			rec.manifest.Runtime.IdleTimeoutSec > 0 &&
			rec.activeViews == 0 &&
			rec.inFlight == 0 &&
			time.Since(rec.lastActivity) >= time.Duration(rec.manifest.Runtime.IdleTimeoutSec)*time.Second
		var (
			rt          extRuntime
			hapi        *hostAPIServer
			dp          *scopedDockerProxy
			cancelWatch context.CancelFunc
		)
		if shouldStop {
			rt = rec.runtime
			hapi = rec.hostAPI
			dp = rec.dockerProxy
			cancelWatch = rec.stopWatch
			rec.stopWatch = nil
			rec.runtime = nil
			rec.hostAPI = nil
			rec.dockerProxy = nil
			rec.pendingRestart = false
			rec.idleStopped = true
			rec.state = StateDiscovered
			rec.errMsg = ""
		}
		m.mu.Unlock()
		if shouldStop {
			if cancelWatch != nil {
				cancelWatch()
			}
			if rt != nil {
				_ = rt.Stop(5 * time.Second)
			}
			if hapi != nil {
				_ = hapi.Close()
			}
			if dp != nil {
				_ = dp.Close()
			}
			m.notifyChanged()
		}
		rec.opMu.Unlock()
	}
}

func (m *Manager) cleanupOwnedSandboxes(extID string, ephemeralOnly bool) {
	st := store.Store{Dir: m.cfg.DataDir}
	m.sandboxesMu.Lock()
	specs, err := st.Sandboxes()
	if err != nil {
		m.sandboxesMu.Unlock()
		return
	}
	var toStop []string
	kept := make([]sandbox.Spec, 0, len(specs))
	for _, sp := range specs {
		owned := sp.Labels["io.gvx.extension"] == extID
		if !owned {
			kept = append(kept, sp)
			continue
		}
		if ephemeralOnly && sp.Labels["io.gvx.ephemeral"] != "true" {
			kept = append(kept, sp)
			continue
		}
		toStop = append(toStop, sp.ID)
	}
	if len(toStop) > 0 {
		_ = st.SaveSandboxes(kept)
	}
	m.sandboxesMu.Unlock()

	if m.cfg.Supervisor != nil {
		for _, sbxID := range toStop {
			_ = m.cfg.Supervisor.Stop(sbxID, 3*time.Second)
			m.cfg.Supervisor.Forget(sbxID)
		}
	}
}

func (m *Manager) enforceOwnedChildSandboxMounts(extID string, onlyDisallowedMounts bool) {
	if m.cfg.Supervisor == nil {
		return
	}
	candidates := make(map[string]sandbox.Spec)
	st := store.Store{Dir: m.cfg.DataDir}
	m.sandboxesMu.Lock()
	if specs, err := st.Sandboxes(); err == nil {
		for _, sp := range specs {
			if sp.Labels["io.gvx.extension"] == extID {
				candidates[sp.ID] = sp
			}
		}
	}
	m.sandboxesMu.Unlock()

	for _, sbxID := range m.cfg.Supervisor.Running() {
		if sbxID == "ext-"+extID {
			continue
		}
		if sp, ok := m.cfg.Supervisor.ActiveSpec(sbxID); ok && sp.Labels["io.gvx.extension"] == extID {
			candidates[sbxID] = sp
		}
	}

	for sbxID, sp := range candidates {
		if sbxID == "ext-"+extID {
			continue
		}
		if !m.cfg.Supervisor.State(sbxID).Status.Active() {
			continue
		}
		if onlyDisallowedMounts && validateSandboxSpecAllowedMounts(sp, extID, m.cfg.DataDir, m.grants) == nil {
			continue
		}
		_ = m.cfg.Supervisor.Stop(sbxID, 3*time.Second)
	}
}

func (m *Manager) dockerBackendRequest(ctx context.Context, method, pathWithQuery string) (int, []byte, error) {
	if m.cfg.DockerEngineHandler != nil {
		if h := m.cfg.DockerEngineHandler(); h != nil {
			req, err := http.NewRequestWithContext(ctx, method, "http://docker"+pathWithQuery, http.NoBody)
			if err != nil {
				return 0, nil, err
			}
			rec := newDockerMemResponseWriter()
			h.ServeHTTP(rec, req)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			return status, rec.body.Bytes(), nil
		}
	}
	if m.cfg.DockerBackendDial == nil {
		return 0, nil, errors.New("docker backend unavailable")
	}
	tr := &http.Transport{
		DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
			return m.cfg.DockerBackendDial(c)
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	cl := &http.Client{
		Transport: tr,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+pathWithQuery, http.NoBody)
	if err != nil {
		return 0, nil, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
	return resp.StatusCode, body, nil
}

func (m *Manager) enforceOwnedDockerContainerMounts(extID string, onlyDisallowedMounts bool) {
	if m.cfg.DockerEngineHandler == nil && m.cfg.DockerBackendDial == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, body, err := m.dockerBackendRequest(ctx, http.MethodGet, "/containers/json")
	if err != nil || status != http.StatusOK {
		return
	}
	var items []struct {
		ID     string            `json:"Id"`
		Labels map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		return
	}
	for _, item := range items {
		cid := strings.TrimSpace(item.ID)
		if cid == "" || item.Labels["io.gvx.extension"] != extID {
			continue
		}
		if onlyDisallowedMounts {
			ist, ibody, ierr := m.dockerBackendRequest(ctx, http.MethodGet, "/containers/"+url.PathEscape(cid)+"/json")
			if ierr == nil && ist == http.StatusOK {
				var inspect containerInspectMeta
				if json.Unmarshal(ibody, &inspect) == nil &&
					validateContainerInspectAllowedMounts(inspect, extID, m.cfg.DataDir, m.grants) == nil {
					continue
				}
			}
		}
		_, _, _ = m.dockerBackendRequest(ctx, http.MethodPost, "/containers/"+url.PathEscape(cid)+"/stop?t=2")
	}
}

func (m *Manager) buildInfoLocked(rec *extRecord) ExtensionInfo {
	entry := m.grants.Get(rec.id)
	used, _ := rec.storage.Used()
	info := ExtensionInfo{
		ID:             rec.id,
		Name:           rec.id,
		State:          rec.state,
		Error:          rec.errMsg,
		Badge:          rec.badge,
		Source:         rec.source,
		Granted:        slices.Clone(entry.Granted),
		Denied:         slices.Clone(entry.Denied),
		AllowedFolders: slices.Clone(entry.AllowedFolders),
		StorageUsed:    used,
		StorageQuota:   rec.storage.Quota(),
	}
	if rec.manifest != nil {
		man := rec.manifest
		info.Name = man.Name
		info.Description = man.Description
		info.Icon = man.Icon
		info.Version = man.Version
		info.Publisher = man.Publisher
		info.Bundled = man.Bundled
		info.RuntimeType = man.Runtime.Type
		info.Unsandboxed = man.Runtime.Type == RuntimeBinary
		info.Permissions = slices.Clone(man.Permissions)
		_, _, asked := NeedsApproval(man, entry, rec.source.Commit)
		info.Asked = asked
		info.Views = man.ToViewInfos()
		info.Commands = man.ToCommandInfos()
		info.Tools = man.ToToolInfos()
		info.Settings = rec.settings.Summaries(man)
		info.I18n = cloneManifestI18n(man.I18n)
		if man.WebUI != nil {
			info.WebUI = &WebUIInfo{
				Port:  man.WebUI.Port,
				Path:  man.WebUI.Path,
				Title: man.WebUI.Title,
				URL:   "http://" + localproxy.BuildHost(rec.id) + man.WebUI.Path,
			}
		}
	}
	return info
}

func (m *Manager) notifyChanged() {
	m.signalChange()
	if m.cfg.UIHost != nil {
		m.cfg.UIHost.Changed()
	}
}

func hasView(m *Manifest, viewID string) bool {
	for _, v := range m.Views {
		if v.ID == viewID {
			return true
		}
	}
	return false
}

func hasCommand(m *Manifest, cmdID string) bool {
	for _, c := range m.Commands {
		if c.ID == cmdID {
			return true
		}
	}
	return false
}

func hasTool(m *Manifest, toolID string) bool {
	for _, t := range m.Tools {
		if t.ID == toolID {
			return true
		}
	}
	return false
}
