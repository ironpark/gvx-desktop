// Package ui is the desktop app's interface, built with ggui.
package ui

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/updater"

	"github.com/ironpark/gvx-desktop/internal/appupdate"
	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/dockerproxy"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/ext"
	"github.com/ironpark/gvx-desktop/internal/imagefs"
	"github.com/ironpark/gvx-desktop/internal/kubeapi"
	"github.com/ironpark/gvx-desktop/internal/localproxy"
	"github.com/ironpark/gvx-desktop/internal/localreg"
	"github.com/ironpark/gvx-desktop/internal/mcpserver"
	"github.com/ironpark/gvx-desktop/internal/nodenet"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/registry"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// Pages of the sidebar.
const (
	PageDashboard  = "dashboard"
	PageSandboxes  = "sandboxes"
	PageContainers = "containers"
	PageNetworks   = "networks"
	PagePods       = "pods"
	PageWorkloads  = "workloads"
	PageServices   = "services"
	PageImages     = "images"
	PageVolumes    = "volumes"
	PageSnapshots  = "snapshots"
	PageDocker     = "docker"
	PageKubernetes = "kubernetes"
	PageSettings   = "settings"
	// PageLabs holds what is still experimental, above Extensions and Settings in
	// the sidebar.
	PageLabs = "labs"
	// PageExtensions is the dedicated extension install/management page, shown
	// between Labs and Settings in the sidebar when Extensions are enabled.
	PageExtensions = "extensions"
	// PageExtensionPrefix is the route prefix for extension views ("ext:<ext-id>/<view-id>").
	PageExtensionPrefix = "ext:"
)

// ExtensionPage returns the navigation page key for an extension's view.
func ExtensionPage(extID, viewID string) string {
	return PageExtensionPrefix + extID + "/" + viewID
}

// ParseExtensionPage extracts extension ID and view ID from an extension page key.
func ParseExtensionPage(page string) (extID, viewID string, ok bool) {
	rest, has := strings.CutPrefix(page, PageExtensionPrefix)
	if !has {
		return "", "", false
	}
	extID, viewID, ok = strings.Cut(rest, "/")
	if !ok || extID == "" || viewID == "" {
		return "", "", false
	}
	return extID, viewID, true
}

// Engine is what the app knows of the rungv binary.
type Engine struct {
	Path    string
	Version rungv.Version
	Info    rungv.Info
	Err     string
	Loading bool
	Checked bool
}

// Ready reports a binary that answered and has a backend to run on.
func (e Engine) Ready() bool {
	return e.Path != "" && e.Err == "" && e.Checked && len(e.Info.AvailableBackends) > 0
}

// Equal makes Engine comparable for signals.
func (e Engine) Equal(o Engine) bool {
	return e.Path == o.Path && e.Version == o.Version && e.Err == o.Err && e.Loading == o.Loading &&
		e.Checked == o.Checked && e.Info.DefaultBackend == o.Info.DefaultBackend &&
		slices.Equal(e.Info.AvailableBackends, o.Info.AvailableBackends) &&
		slices.Equal(e.Info.BackendWarnings, o.Info.BackendWarnings) &&
		e.Info.HostLockdown == o.Info.HostLockdown && e.Info.Images == o.Info.Images &&
		e.Info.Volumes == o.Info.Volumes && e.Info.LayerCacheDir == o.Info.LayerCacheDir &&
		e.Info.VolumeDir == o.Info.VolumeDir
}

// Row is a sandbox with its runtime state, as the tables show it.
type Row struct {
	Spec  sandbox.Spec
	State sandbox.State
}

// Equal makes Row comparable for keyed rows.
func (r Row) Equal(o Row) bool { return r.Spec.Equal(o.Spec) && r.State.Equal(o.State) }

// Listing is a list loaded from rungv with its loading state.
type Listing[T any] struct {
	Items   []T
	Err     string
	Loading bool
	Loaded  bool
}

// Model holds the app's state and actions. Signals belong to the UI
// thread; background work posts its results back through post.
type Model struct {
	store   store.Store
	sup     *sandbox.Supervisor
	nodes   *nodenet.Network // the cluster's nodes' network
	host    ggui.Host
	appMenu func() []ggui.MenuItem

	clientMu sync.Mutex
	client   *rungv.Client

	Toasts *ui.ToasterWidget

	Page            *ggui.StateValue[string]
	Settings        *ggui.StateValue[store.Settings]
	Engine          *ggui.StateValue[Engine]
	RungvFeatures   *ggui.StateValue[rungv.Features]
	Update          *ggui.StateValue[Update]
	Releases        *ggui.StateValue[[]rungv.Release] // recent published gvx releases with a build for this host
	SelectedRelease *ggui.StateValue[string]          // chosen release tag in Settings; "" follows the newest
	Autostart       *ggui.StateValue[bool]            // registered to start at login
	BuildLog        *ggui.StateValue[string]          // the end of what a build from source printed
	release         rungv.Release                     // what Update tells of
	// AppUpdate is what the app knows of its own newest release; see
	// model_appupdate.go.
	AppUpdate  *ggui.StateValue[AppUpdate]
	UpdateCtrl *updater.Controller
	appRelease appupdate.Release // what AppUpdate tells of
	appStaged  *appupdate.Staged // downloaded, for a restart to put in place
	appRestart *appupdate.Staged // to finish once the app has ended
	appChecked time.Time         // when the app's releases were last asked for, by the wall clock
	dataDir    string
	Now        *ggui.StateValue[time.Time]
	// Lang is the language the app is worded in, from the settings or the
	// system: one of LanguageTags.
	Lang *ggui.DerivedValue[string]

	Specs                   *ggui.StateValue[[]sandbox.Spec]
	States                  *ggui.StateValue[map[string]sandbox.State]
	Rows                    *ggui.DerivedValue[[]Row]
	Active                  *ggui.DerivedValue[int]
	Running                 *ggui.DerivedValue[int]
	EngineRunning           *ggui.StateValue[int]
	TotalRunning            *ggui.DerivedValue[int]
	EngineVolumeUsers       *ggui.StateValue[map[string][]string]
	EngineContainerRuntimes *ggui.StateValue[map[string]engine.ContainerRuntimeInfo]
	BuilderState            *ggui.StateValue[engine.BuilderState]
	BuildxStatus            *ggui.StateValue[BuildxStatus]
	Amd64State              *ggui.StateValue[engine.Amd64State]
	MicroVMState            *ggui.StateValue[engine.MicroVMState]
	Amd64Check              *ggui.StateValue[Amd64CheckState]
	Amd64Resetting          *ggui.StateValue[bool]
	Selected                *ggui.StateValue[string]
	Logs                    *ggui.StateValue[[]sandbox.LogLine]
	DmesgMode               *ggui.StateValue[bool]
	DmesgLines              *ggui.StateValue[[]sandbox.LogLine]
	DmesgLoading            *ggui.StateValue[bool]
	Detail                  *ggui.StateValue[int]
	Query                   *ggui.StateValue[string]

	Images          *ggui.StateValue[Listing[rungv.Image]]
	Volumes         *ggui.StateValue[Listing[rungv.Volume]]
	Disk            *ggui.StateValue[[]rungv.DiskUsage]
	Snapshots       *ggui.StateValue[[]sandbox.Snapshot]
	SnapshotsLoaded *ggui.StateValue[bool]
	AddingNode      *ggui.StateValue[bool]
	BusyOps         *ggui.StateValue[map[string]bool]

	// ImageView chooses between gvx's own image cache (ImagesGVX), the
	// images inside the Docker Engine (ImagesDocker), and the built-in
	// local OCI registry (ImagesRegistry).
	ImageView                  *ggui.StateValue[int]
	DockerImages               *ggui.StateValue[Listing[dockerapi.Image]]
	DockerImageQuery           *ggui.StateValue[string]
	SelectedDockerImage        *ggui.StateValue[string]
	LocalRegistry              *ggui.StateValue[LocalRegistryState]
	LocalRegistryImages        *ggui.StateValue[Listing[localreg.ImageEntry]]
	LocalRegistryQuery         *ggui.StateValue[string]
	SelectedLocalRegistryImage *ggui.StateValue[string]
	LocalProxy                 *ggui.StateValue[LocalProxyState]
	MCPServer                  *ggui.StateValue[MCPServerState]

	DockerContainers *ggui.StateValue[Listing[dockerapi.Container]]
	DockerVersion    *ggui.StateValue[dockerapi.Version]
	// ContainersAccess says whether the Containers page can be used, which
	// is while the Docker Engine runs, and ContainersOpen is its yes.
	ContainersAccess *ggui.DerivedValue[ContainersAccess]
	ContainersOpen   *ggui.DerivedValue[bool]
	// RunningContainers counts the engine's running containers.
	RunningContainers *ggui.DerivedValue[int]
	// The Containers page: what filters its table, the container chosen in
	// it, which is the first when none is, the tab of its details, and the
	// last lines of its logs.
	ContainerQuery    *ggui.StateValue[string]
	ContainerStatus   *ggui.StateValue[string]
	SelectedContainer *ggui.StateValue[string]
	Container         *ggui.DerivedValue[dockerapi.Container]
	ContainerDetail   *ggui.StateValue[int]
	ContainerLogs     *ggui.StateValue[ContainerLogs]
	// ComposeFolded are the Compose projects whose containers the table
	// folds under their row.
	ComposeFolded *ggui.StateValue[map[string]bool]
	// The Networks page: the engine's networks, what filters them, and the
	// one chosen (by ID or name), which is the first when none is.
	DockerNetworks  *ggui.StateValue[Listing[dockerapi.Network]]
	NetworkQuery    *ggui.StateValue[string]
	SelectedNetwork *ggui.StateValue[string]
	Network         *ggui.DerivedValue[dockerapi.Network]
	KubeconfigReady *ggui.StateValue[bool]
	// Pods are the k3s cluster's, and PodsWaiting says its sandbox runs
	// and its API server has yet to answer: k3s is still coming up.
	Pods        *ggui.StateValue[Listing[kubeapi.Pod]]
	PodsWaiting *ggui.StateValue[bool]
	// KubeNodes are the cluster's nodes as its API server lists them,
	// read while the Kubernetes page shows them.
	KubeNodes *ggui.StateValue[[]kubeapi.Node]
	// PodsAccess says whether the Pods page can be used, which is while
	// the cluster's sandbox runs, and PodsOpen is its yes.
	PodsAccess *ggui.DerivedValue[PodsAccess]
	PodsOpen   *ggui.DerivedValue[bool]
	// RunningPods counts the cluster's running pods.
	RunningPods *ggui.DerivedValue[int]
	// The Pods page: what filters its table, whether it shows the system's
	// pods, the pod chosen in it (namespace/name), which is the first shown
	// when none is, the tab of its details, the container whose logs show,
	// as chosen and as it is, and the last lines of those.
	PodQuery        *ggui.StateValue[string]
	SystemPods      *ggui.StateValue[bool]
	SelectedPod     *ggui.StateValue[string]
	Pod             *ggui.DerivedValue[kubeapi.Pod]
	PodDetail       *ggui.StateValue[int]
	PodContainer    *ggui.StateValue[string]
	PodLogContainer *ggui.DerivedValue[string]
	PodLogs         *ggui.StateValue[ContainerLogs]
	// PodOwner is the workload whose pods alone the Pods page shows, if
	// one is set.
	PodOwner *ggui.StateValue[WorkloadRef]
	// Workloads and Services are the cluster's, read with its pods while a
	// page shows them; the queries filter their tables.
	Workloads     *ggui.StateValue[Listing[kubeapi.Workload]]
	Services      *ggui.StateValue[Listing[kubeapi.Service]]
	WorkloadQuery *ggui.StateValue[string]
	ServiceQuery  *ggui.StateValue[string]
	// What the running sandboxes (by ID), containers (by ID) and pods (by
	// namespace/name) use, read while a page shows them; see model_usage.go.
	SandboxUsage   *ggui.StateValue[map[string]Usage]
	ContainerUsage *ggui.StateValue[map[string]Usage]
	PodUsage       *ggui.StateValue[map[string]PodUse]
	// Forwards are the port forwards that run, oldest first.
	Forwards *ggui.StateValue[[]PortForward]
	// The dialog that starts a port forward: what to, the ports typed, and
	// why it could not start.
	ForwardOpen   *ggui.StateValue[bool]
	ForwardDraft  *ggui.StateValue[ForwardDraft]
	ForwardRemote *ggui.StateValue[string]
	ForwardLocal  *ggui.StateValue[string]
	ForwardErr    *ggui.StateValue[string]
	// The dialog that scales a workload.
	ScaleOpen     *ggui.StateValue[bool]
	ScaleTarget   *ggui.StateValue[kubeapi.Workload]
	ScaleReplicas *ggui.StateValue[float64]
	// DockerEndpoint is the DOCKER_HOST value that reaches the Docker
	// Engine, as DockerHost says it.
	DockerEndpoint *ggui.DerivedValue[string]
	// DockerProxyErr is the last listener error from the app's Docker API
	// proxy, or empty when listening normally.
	DockerProxyErr *ggui.StateValue[string]
	// SandboxEngineStatus tracks the in-app sandbox engine startup lifecycle.
	SandboxEngineStatus *ggui.StateValue[SandboxEngineStatus]
	// DockerExposeLAN mirrors Settings.DockerExposeLAN for reactive UI bindings.
	DockerExposeLAN *ggui.StateValue[bool]
	// DockerSSHAgent mirrors Settings.DockerSSHAgent for reactive UI bindings.
	DockerSSHAgent *ggui.StateValue[bool]
	// DockerBindSymlinks mirrors Settings.DockerBindSymlinks for reactive UI bindings.
	DockerBindSymlinks *ggui.StateValue[bool]
	// DockerLink and KubeLink are the contexts docker and kubectl on this
	// machine reach the recipes' engines through.
	DockerLink, KubeLink *Link
	// BundledDocker describes the Docker CLI + plugins bundled inside this app
	// package and whether ~/.gvx/bin is wired into the user's terminal PATH.
	BundledDocker *ggui.StateValue[BundledDockerState]
	// RungvPath describes whether the active rungv binary is exposed via
	// ~/.gvx/bin and wired into the user's terminal PATH.
	RungvPath *ggui.StateValue[RungvPathState]
	// LinkCommandsOpen unfolds the commands that set the contexts up by hand.
	LinkCommandsOpen *ggui.StateValue[bool]
	// Menus is what the Docker and Kubernetes menus tell; see RecipeMenu.
	Menus *ggui.DerivedValue[MenuState]

	// Editor
	EditorOpen    *ggui.StateValue[bool]
	EditorTouched *ggui.StateValue[bool]
	editorInitial Draft
	Draft         *ggui.StateValue[Draft]
	DraftError    *ggui.DerivedValue[string]
	DraftLine     *ggui.DerivedValue[string]
	DraftAction   *ggui.DerivedValue[editorAction]

	// Dialogs
	ConfirmOpen   *ggui.StateValue[bool]
	ConfirmTitle  *ggui.StateValue[string]
	ConfirmText   *ggui.StateValue[string]
	ConfirmLabel  *ggui.StateValue[string]
	ConfirmSafe   *ggui.StateValue[bool] // the action destroys nothing: its button is not red
	confirmAction func()
	PullOpen      *ggui.StateValue[bool]
	PullTarget    *ggui.StateValue[int]
	PullRef       *ggui.StateValue[string]
	PullLog       *ggui.StateValue[string]
	Pulling       *ggui.StateValue[bool]

	// What the pull dialog's search of a registry found; see SearchTyped.
	SearchResults  *ggui.StateValue[[]registry.Result]
	SearchFor      *ggui.StateValue[string] // the words SearchResults answer
	SearchBusy     *ggui.StateValue[bool]
	SearchErr      *ggui.StateValue[string]
	SearchTags     *ggui.StateValue[[]string]
	SearchTagsOf   *ggui.StateValue[string] // the repository SearchTags are of
	SearchTagsBusy *ggui.StateValue[bool]
	search         search
	// Offline keeps the model from asking the network anything, as the
	// screens rendered with made-up state must not.
	Offline bool
	// Image explorer dialog state (Files, Layers, Inspect).
	ExploreOpen      *ggui.StateValue[bool]
	ExploreTitle     *ggui.StateValue[string]
	ExploreSource    *ggui.StateValue[string]
	ExploreTab       *ggui.StateValue[int]
	ExploreLoading   *ggui.StateValue[bool]
	ExploreProgress  *ggui.StateValue[string]
	ExploreErr       *ggui.StateValue[string]
	ExploreDirErr    *ggui.StateValue[string]
	ExploreFilesNote *ggui.StateValue[string]
	ExploreLayers    *ggui.StateValue[[]imagefs.Layer]
	ExploreLayer     *ggui.StateValue[int]
	ExplorePath      *ggui.StateValue[string]
	ExploreQuery     *ggui.StateValue[string]
	ExploreEntries   *ggui.StateValue[[]imagefs.Entry]
	ExploreSelected  *ggui.StateValue[string]
	ExplorePreview   *ggui.StateValue[FilePreview]
	ExploreInspect   *ggui.StateValue[string]
	explorer         explorerSession
	VolumeOpen       *ggui.StateValue[bool]
	VolumeName       *ggui.StateValue[string]
	VolumeDriver     *ggui.StateValue[string]
	// The daemon.json editor of the Docker Engine (model_dockerd_config.go).
	DockerConfigOpen *ggui.StateValue[bool]
	DockerConfigText *ggui.StateValue[string]
	DockerConfigErr  *ggui.StateValue[string]
	PaletteOpen      *ggui.StateValue[bool]
	PaletteQuery     *ggui.StateValue[string]
	QuitOpen         *ggui.StateValue[bool]
	SidebarNarrow    *ggui.StateValue[bool]
	DockerNavOpen    *ggui.StateValue[bool]
	KubeNavOpen      *ggui.StateValue[bool]
	SandboxStatus    *ggui.StateValue[string]
	ImageQuery       *ggui.StateValue[string]
	VolumeQuery      *ggui.StateValue[string]
	SnapshotQuery    *ggui.StateValue[string]
	ExploreSplit     *ggui.StateValue[float64]
	SelectedImage    *ggui.StateValue[string]
	SelectedVolume   *ggui.StateValue[string]

	dirty     atomic.Bool
	scheduled atomic.Bool
	logSeq    int64
	quitting  bool

	runCLI                                     cliRunner                                                                     // runs docker and kubectl; tests replace it
	runVMHelper                                vmHelperRunner                                                                // runs __vm-helper check/install-rosetta; tests replace it
	syncAmd64NodeHook                          func(on bool)                                                                 // overrides syncAmd64Node in tests
	controlOf                                  func(id string) (sandbox.Control, bool)                                       // overrides m.sup.Control in tests
	pauseSandboxFn                             func(ctx context.Context, id string) error                                    // overrides m.sup.Pause in tests
	unpauseSandboxFn                           func(ctx context.Context, id string) error                                    // overrides m.sup.Unpause in tests
	updateSandboxFn                            func(ctx context.Context, id string, u rungv.Update) (rungv.Resources, error) // overrides m.sup.Update in tests
	applyLiveFn                                func(ctx context.Context, id string, diff sandbox.LiveDiff) error             // overrides m.sup.ApplyLive in tests
	dockerLoading, dockerAgain                 bool                                                                          // LoadDocker runs; it is wanted again after
	dockerNetworksLoading, dockerNetworksAgain bool                                                                          // LoadDockerNetworks runs; it is wanted again after
	dockerImagesLoading, dockerImagesAgain     bool                                                                          // LoadDockerImages runs; it is wanted again after
	logsLoading, logsAgain                     bool                                                                          // LoadContainerLogs runs; it is wanted again after
	podsLoading, podsAgain                     bool                                                                          // LoadPods runs; it is wanted again after
	podLogsLoading, podLogsAgain               bool                                                                          // LoadPodLogs runs; it is wanted again after
	podsAnswered                               bool                                                                          // the cluster's API server answered since it started
	sandboxUsageLoading                        bool                                                                          // LoadSandboxUsage runs
	containerUsageLoading                      bool                                                                          // LoadContainerUsage runs
	sandboxSamples                             map[string]sandboxSample                                                      // the last of each sandbox's, by ID
	containerStats                             map[string]dockerapi.Stats                                                    // the last of each container's, by ID
	terms                                      liveTerminals
	forwards                                   liveForwards
	forwarder                                  forwarder // carries port forwards; tests replace it
	dockerProxy                                *dockerproxy.Proxy
	dockerProxyStarted                         bool
	dockerBackend                              string // overrides recipes.DockerBackendHost() in tests
	engineSpec                                 atomic.Pointer[sandbox.Spec]
	engineLiveMounts                           atomic.Bool
	dockerExposeLAN                            atomic.Bool
	dockerSSHAgent                             atomic.Bool
	dockerBindSymlinks                         atomic.Bool
	sandboxEngine                              sandboxEngine // in-app Docker Engine API (*engine.Engine); tests may replace
	sandboxEngineGen                           int
	amd64Gen                                   int
	microVMGen                                 int
	rosettaState                               atomic.Int32 // 0=unknown, 1=missing, 2=installed
	buildxSyncGen                              int
	keepImages                                 atomic.Pointer[[]string]
	buildCancelMu                              sync.Mutex
	buildCancel                                context.CancelFunc
	localRegMu                                 sync.Mutex
	localRegOpMu                               sync.Mutex
	localReg                                   *localreg.Server
	localRegPort                               int // 0 -> recipes.RegistryPort (5001); <0 -> ephemeral 127.0.0.1:0 (tests)
	localRegDesired                            bool
	localRegWant                               bool
	localRegInFlight                           string
	localRegSyncGen                            int
	localRegLoading                            bool
	localRegAgain                              bool
	localRegChangePending                      atomic.Bool

	localProxyMu         sync.Mutex
	localProxyOpMu       sync.Mutex
	localProxy           *localproxy.Server
	localProxyTestPorts  bool // when true, bind ephemeral 127.0.0.1:0 ports (tests)
	localProxyDesired    bool
	localProxyWant       bool
	localProxyWantHTTP   int
	localProxyWantHTTPS  int
	localProxyBoundHTTP  int // configured HTTP port the current localProxy was created for
	localProxyBoundHTTPS int // configured HTTPS port the current localProxy was created for
	localProxyInFlight   string
	localProxySyncGen    int
	localProxyRoutes     []localproxy.Route

	mcpMu        sync.Mutex
	mcpOpMu      sync.Mutex
	mcpSrv       *mcpserver.Server
	mcpTestPort  int // when <0, bind ephemeral 127.0.0.1:0 port (tests)
	mcpDesired   bool
	mcpWant      bool
	mcpWantPort  int
	mcpBoundPort int // configured port the current mcpSrv was created for
	mcpInFlight  string
	mcpSyncGen   int
	mcpSnapMu    sync.RWMutex
	mcpSnap      mcpSnapshot

	Extensions       *ggui.StateValue[[]ext.ExtensionInfo]
	ExtensionSecrets *ggui.StateValue[ext.SecretStoreStatus]
	ExtNavOpen       *ggui.StateValue[bool]
	ExtBuildLogsTick *ggui.StateValue[int]

	extMgrPtr          atomic.Pointer[ext.Manager]
	extOwnedMgr        bool
	extLifeMu          sync.Mutex
	extMu              sync.Mutex
	extWatchCancel     context.CancelFunc
	extWatchDone       chan struct{}
	extStreamMu        sync.Mutex
	extStreamKey       string
	extStreamCancel    context.CancelFunc
	extViews           map[string]*extViewSession
	developerMode      atomic.Bool
	sandboxEngineMode  atomic.Bool
	hostInfoSnap       atomic.Pointer[ext.HostInfo]
	extThemeSent       string
	extLocaleSent      string
	extDockerStateSent string
}

// NewModel loads the saved state. Call it under a ggui owner (ggui.Root).
func NewModel(st store.Store, dataDir string) *Model {
	settings, _ := st.Settings()
	SetLanguage(ResolveLanguage(settings.Language))
	specs, _ := st.Sandboxes()
	if settings.DockerEndpoint == "" && settings.DockerPort == 0 {
		for _, s := range specs {
			if s.Recipe != recipes.Docker {
				continue
			}
			if api, ok := recipes.LegacyDockerAPI(s); ok && api.TCP {
				settings.DockerEndpoint = DockerTCPEndpoint
				settings.DockerPort = api.Port
				_ = st.SaveSettings(settings)
			}
			break
		}
	}
	if up, changed := upgradeRecipes(specs); changed {
		if err := st.SaveSandboxes(up); err == nil {
			specs = up
		}
	}
	m := &Model{
		store:   st,
		dataDir: dataDir,
		Toasts:  ui.NewToaster().Limit(3),

		Page:            ggui.State(PageDashboard),
		Settings:        ggui.State(settings),
		Engine:          ggui.State(Engine{Loading: true}),
		RungvFeatures:   ggui.State(rungv.Features{}),
		Update:          ggui.State(Update{Source: settings.UpdateChannel == ChannelSource, Repo: rungv.Source{Repo: settings.SourceRepo, Branch: settings.SourceBranch}.DisplayRepo(), Branch: rungv.Source{Repo: settings.SourceRepo, Branch: settings.SourceBranch}.BranchName()}),
		Releases:        ggui.State([]rungv.Release(nil)).WithEqual(func(a, b []rungv.Release) bool { return slices.EqualFunc(a, b, releaseEqual) }),
		SelectedRelease: ggui.State(""),
		Autostart:       ggui.State(false), // read by Start
		BuildLog:        ggui.State(""),
		AppUpdate:       ggui.State(AppUpdate{Install: appupdate.Detect()}),
		Now:             ggui.State(time.Now()),

		Specs:                   ggui.State(specs).WithEqual(func(a, b []sandbox.Spec) bool { return slices.EqualFunc(a, b, sandbox.Spec.Equal) }),
		States:                  ggui.State(map[string]sandbox.State{}).WithEqual(nil),
		EngineRunning:           ggui.State(0),
		EngineVolumeUsers:       ggui.State(map[string][]string(nil)).WithEqual(stringSliceMapEqual),
		EngineContainerRuntimes: ggui.State(map[string]engine.ContainerRuntimeInfo(nil)).WithEqual(maps.Equal[map[string]engine.ContainerRuntimeInfo]),
		BuilderState:            ggui.State(engine.BuilderState{Status: engine.BuilderIdle}),
		BuildxStatus:            ggui.State(BuildxStatus{}).WithEqual(BuildxStatus.Equal),
		Amd64State:              ggui.State(engine.Amd64State{Status: engine.BuilderIdle}).WithEqual(engine.Amd64State.Equal),
		MicroVMState:            ggui.State(engine.MicroVMState{}).WithEqual(engine.MicroVMState.Equal),
		Amd64Check:              ggui.State(Amd64CheckState{}).WithEqual(Amd64CheckState.Equal),
		Amd64Resetting:          ggui.State(false),
		Selected:                ggui.State(""),
		Logs:                    ggui.State([]sandbox.LogLine(nil)).WithEqual(nil),
		DmesgMode:               ggui.State(false),
		DmesgLines:              ggui.State([]sandbox.LogLine(nil)).WithEqual(nil),
		DmesgLoading:            ggui.State(false),
		Detail:                  ggui.State(0),
		Query:                   ggui.State(""),

		Images:                     ggui.State(Listing[rungv.Image]{}).WithEqual(nil),
		Volumes:                    ggui.State(Listing[rungv.Volume]{}).WithEqual(nil),
		Disk:                       ggui.State([]rungv.DiskUsage(nil)).WithEqual(slices.Equal[[]rungv.DiskUsage]),
		Snapshots:                  ggui.State([]sandbox.Snapshot(nil)).WithEqual(slices.Equal[[]sandbox.Snapshot]),
		SnapshotsLoaded:            ggui.State(false),
		AddingNode:                 ggui.State(false),
		BusyOps:                    ggui.State(map[string]bool{}).WithEqual(maps.Equal[map[string]bool]),
		ImageView:                  ggui.State(ImagesGVX),
		DockerImages:               ggui.State(Listing[dockerapi.Image]{}).WithEqual(nil),
		DockerImageQuery:           ggui.State(""),
		SelectedDockerImage:        ggui.State(""),
		LocalRegistry:              ggui.State(LocalRegistryState{Mode: settings.LocalRegistryMode()}).WithEqual(LocalRegistryState.Equal),
		LocalRegistryImages:        ggui.State(Listing[localreg.ImageEntry]{}).WithEqual(localRegistryImagesEqual),
		LocalRegistryQuery:         ggui.State(""),
		SelectedLocalRegistryImage: ggui.State(""),
		LocalProxy: ggui.State(LocalProxyState{
			Enabled:   settings.LocalProxy,
			HTTPPort:  settings.LocalProxyHTTPPortOrDefault(),
			HTTPSPort: settings.LocalProxyHTTPSPortOrDefault(),
		}).WithEqual(LocalProxyState.Equal),
		MCPServer: ggui.State(MCPServerState{
			Enabled: settings.MCPServer,
			Port:    settings.MCPPortOrDefault(),
		}).WithEqual(MCPServerState.Equal),

		DockerContainers:    ggui.State(Listing[dockerapi.Container]{}).WithEqual(nil),
		DockerVersion:       ggui.State(dockerapi.Version{}),
		DockerProxyErr:      ggui.State(""),
		SandboxEngineStatus: ggui.State(SandboxEngineStatus{}),
		DockerExposeLAN:     ggui.State(settings.DockerExposeLAN),
		DockerSSHAgent:      ggui.State(settings.DockerSSHAgent),
		DockerBindSymlinks:  ggui.State(settings.DockerBindSymlinks),
		BundledDocker:       ggui.State(BundledDockerState{}).WithEqual(BundledDockerState.Equal),
		RungvPath:           ggui.State(RungvPathState{}).WithEqual(RungvPathState.Equal),
		ContainerQuery:      ggui.State(""),
		ContainerStatus:     ggui.State("all"),
		SelectedContainer:   ggui.State(""),
		ContainerDetail:     ggui.State(0),
		ContainerLogs:       ggui.State(ContainerLogs{}).WithEqual(ContainerLogs.Equal),
		ComposeFolded:       ggui.State(map[string]bool{}).WithEqual(maps.Equal[map[string]bool]),
		DockerNetworks:      ggui.State(Listing[dockerapi.Network]{}).WithEqual(nil),
		NetworkQuery:        ggui.State(""),
		SelectedNetwork:     ggui.State(""),
		SandboxUsage:        ggui.State(map[string]Usage(nil)).WithEqual(usageEqual),
		ContainerUsage:      ggui.State(map[string]Usage(nil)).WithEqual(usageEqual),
		PodUsage:            ggui.State(map[string]PodUse(nil)).WithEqual(podUsageEqual),
		KubeconfigReady:     ggui.State(false),
		Pods:                ggui.State(Listing[kubeapi.Pod]{}).WithEqual(podsEqual),
		KubeNodes:           ggui.State([]kubeapi.Node(nil)).WithEqual(nodesEqual),
		PodsWaiting:         ggui.State(false),
		PodQuery:            ggui.State(""),
		SystemPods:          ggui.State(false),
		SelectedPod:         ggui.State(""),
		PodDetail:           ggui.State(0),
		PodContainer:        ggui.State(""),
		PodLogs:             ggui.State(ContainerLogs{}).WithEqual(ContainerLogs.Equal),
		PodOwner:            ggui.State(WorkloadRef{}),
		Workloads:           ggui.State(Listing[kubeapi.Workload]{}).WithEqual(workloadsEqual),
		Services:            ggui.State(Listing[kubeapi.Service]{}).WithEqual(servicesEqual),
		WorkloadQuery:       ggui.State(""),
		ServiceQuery:        ggui.State(""),
		Forwards:            ggui.State([]PortForward(nil)).WithEqual(slices.Equal[[]PortForward]),
		ForwardOpen:         ggui.State(false),
		ForwardDraft:        ggui.State(ForwardDraft{}).WithEqual(ForwardDraft.Equal),
		ForwardRemote:       ggui.State(""),
		ForwardLocal:        ggui.State(""),
		ForwardErr:          ggui.State(""),
		ScaleOpen:           ggui.State(false),
		ScaleTarget:         ggui.State(kubeapi.Workload{}).WithEqual(kubeapi.Workload.Equal),
		ScaleReplicas:       ggui.State(1.0),
		LinkCommandsOpen:    ggui.State(false),

		EditorOpen:    ggui.State(false),
		EditorTouched: ggui.State(false),
		Draft:         ggui.State(Draft{}),

		ConfirmOpen:      ggui.State(false),
		ConfirmTitle:     ggui.State(""),
		ConfirmText:      ggui.State(""),
		ConfirmLabel:     ggui.State(tr("Confirm")),
		ConfirmSafe:      ggui.State(false),
		PullOpen:         ggui.State(false),
		PullTarget:       ggui.State(PullTargetGVX),
		PullRef:          ggui.State(""),
		PullLog:          ggui.State(""),
		Pulling:          ggui.State(false),
		SearchResults:    ggui.State([]registry.Result(nil)),
		SearchFor:        ggui.State(""),
		SearchBusy:       ggui.State(false),
		SearchErr:        ggui.State(""),
		SearchTags:       ggui.State([]string(nil)),
		SearchTagsOf:     ggui.State(""),
		SearchTagsBusy:   ggui.State(false),
		ExploreOpen:      ggui.State(false),
		ExploreTitle:     ggui.State(""),
		ExploreSource:    ggui.State(""),
		ExploreTab:       ggui.State(ExploreFiles),
		ExploreLoading:   ggui.State(false),
		ExploreProgress:  ggui.State(""),
		ExploreErr:       ggui.State(""),
		ExploreDirErr:    ggui.State(""),
		ExploreFilesNote: ggui.State(""),
		ExploreLayers:    ggui.State([]imagefs.Layer(nil)).WithEqual(func(a, b []imagefs.Layer) bool { return slices.EqualFunc(a, b, imagefs.Layer.Equal) }),
		ExploreLayer:     ggui.State(0),
		ExplorePath:      ggui.State("/"),
		ExploreQuery:     ggui.State(""),
		ExploreEntries:   ggui.State([]imagefs.Entry(nil)).WithEqual(func(a, b []imagefs.Entry) bool { return slices.EqualFunc(a, b, imagefs.Entry.Equal) }),
		ExploreSelected:  ggui.State(""),
		ExplorePreview:   ggui.State(FilePreview{}).WithEqual(FilePreview.Equal),
		ExploreInspect:   ggui.State(""),
		VolumeOpen:       ggui.State(false),
		VolumeName:       ggui.State(""),
		VolumeDriver:     ggui.State("ext4lite"),
		DockerConfigOpen: ggui.State(false),
		DockerConfigText: ggui.State(""),
		DockerConfigErr:  ggui.State(""),
		PaletteOpen:      ggui.State(false),
		PaletteQuery:     ggui.State(""),
		QuitOpen:         ggui.State(false),
		SidebarNarrow:    ggui.State(settings.SidebarNarrow),
		DockerNavOpen:    ggui.State(true),
		KubeNavOpen:      ggui.State(true),
		SandboxStatus:    ggui.State("all"),
		ImageQuery:       ggui.State(""),
		VolumeQuery:      ggui.State(""),
		SnapshotQuery:    ggui.State(""),
		ExploreSplit:     ggui.State(0.47),
		SelectedImage:    ggui.State(""),
		SelectedVolume:   ggui.State(""),
		Extensions:       ggui.State([]ext.ExtensionInfo(nil)).WithEqual(extensionsEqual),
		ExtensionSecrets: ggui.State(ext.SecretStoreStatus{}),
		ExtNavOpen:       ggui.State(true),
		ExtBuildLogsTick: ggui.State(0),
		extViews:         map[string]*extViewSession{},
		runCLI:           runCLI,
		runVMHelper:      defaultVMHelperRunner,
		forwarder:        forwardThroughCluster,
	}
	m.dockerExposeLAN.Store(settings.DockerExposeLAN)
	m.dockerSSHAgent.Store(settings.DockerSSHAgent)
	m.dockerBindSymlinks.Store(settings.DockerBindSymlinks)
	m.developerMode.Store(settings.DeveloperMode)
	m.sandboxEngineMode.Store(settings.SandboxEngine)
	m.updateHostInfoSnapshot(settings, Engine{}, false)
	m.watchAmd64Check()
	ggui.OnCleanup(m.Toasts.Close)
	ggui.OnCleanup(m.CloseLocalRegistry)
	ggui.OnCleanup(m.CloseLocalProxy)
	ggui.OnCleanup(m.CloseMCPServer)
	ggui.OnCleanup(m.stopExtensionWatchers)
	m.nodes = &nodenet.Network{Dir: filepath.Join(dataDir, "nodes"), Client: m.Client}
	m.sup = &sandbox.Supervisor{
		Client:    m.Client,
		DataDir:   dataDir,
		Notify:    m.onEvent,
		Prepare:   recipes.Prepare,
		StopGrace: recipes.StopGrace,
		Launch:    m.nodes.Launch,
	}
	m.Lang = ggui.Map(m.Settings, func(s store.Settings) string { return ResolveLanguage(s.Language) })
	m.Rows = ggui.Combine(m.Specs, m.States, func(specs []sandbox.Spec, states map[string]sandbox.State) []Row {
		rows := make([]Row, len(specs))
		for i, s := range specs {
			rows[i] = Row{Spec: s, State: states[s.ID]}
		}
		return rows
	}).WithEqual(func(a, b []Row) bool { return slices.EqualFunc(a, b, Row.Equal) })
	m.initDockerProxy(dataDir)
	m.DockerEndpoint = ggui.Map(m.Settings, func(set store.Settings) string {
		return dockerSettingOf(set).Host()
	})
	m.newLinks()
	m.ContainersAccess = ggui.Derived(func() ContainersAccess {
		set := m.Settings.Get()
		if set.SandboxEngine && m.SandboxEngineStatus.Get().Opening {
			return ContainersStarting
		}
		return containersAccess(m.Rows.Get(), set)
	})
	m.ContainersOpen = ggui.Map(m.ContainersAccess, func(a ContainersAccess) bool { return a == ContainersOpen })
	m.RunningContainers = ggui.Map(m.DockerContainers, func(l Listing[dockerapi.Container]) int {
		n := 0
		for _, c := range l.Items {
			if c.State == "running" {
				n++
			}
		}
		return n
	})
	m.Container = ggui.Combine(m.DockerContainers, m.SelectedContainer, func(l Listing[dockerapi.Container], id string) dockerapi.Container {
		return chosenContainer(l.Items, id)
	}).WithEqual(dockerapi.Container.Equal)
	m.Network = ggui.Combine(m.DockerNetworks, m.SelectedNetwork, func(l Listing[dockerapi.Network], id string) dockerapi.Network {
		return chosenNetwork(l.Items, id)
	}).WithEqual(dockerapi.Network.Equal)
	m.PodsAccess = ggui.Map(m.Rows, podsAccess)
	m.PodsOpen = ggui.Map(m.PodsAccess, func(a PodsAccess) bool { return a == PodsOpen })
	m.RunningPods = ggui.Map(m.Pods, func(l Listing[kubeapi.Pod]) int {
		running, _, _ := podCounts(l.Items)
		return running
	})
	m.Pod = ggui.Derived(func() kubeapi.Pod {
		return chosenPod(ownedPods(userPods(m.Pods.Get().Items, m.SystemPods.Get()), m.PodOwner.Get()), m.SelectedPod.Get())
	}).WithEqual(kubeapi.Pod.Equal)
	ggui.Watch(m.SystemPods, func(on bool) {
		if !on {
			if ref := ggui.Peek(m.PodOwner); ref != (WorkloadRef{}) && systemNamespace(ref.Namespace) {
				m.PodOwner.Set(WorkloadRef{})
			}
		}
	})
	m.PodLogContainer = ggui.Combine(m.Pod, m.PodContainer, podLogContainer)
	m.Menus = ggui.Derived(func() MenuState {
		ctrs := m.DockerContainers.Get()
		s := menuState(m.Rows.Get(), ctrs, m.Pods.Get(), m.PodsWaiting.Get(), m.DockerLink.View.Get(), m.KubeLink.View.Get())
		if m.Settings.Get().SandboxEngine {
			s.SandboxEngine = true
			s.SandboxEngineStatus = m.SandboxEngineStatus.Get()
			s.menuContainers(ctrs)
		}
		s.Forwards = menuForwards(m.Forwards.Get())
		return s
	}).WithEqual(MenuState.Equal)
	m.Active = m.States.Map(func(states map[string]sandbox.State) int {
		n := 0
		for _, st := range states {
			if st.Status.Active() {
				n++
			}
		}
		return n
	})
	m.Running = m.States.Map(func(states map[string]sandbox.State) int {
		n := 0
		for _, st := range states {
			if st.Status.Active() && !st.Paused {
				n++
			}
		}
		return n
	})
	m.TotalRunning = ggui.Combine(m.Active, m.EngineRunning, func(a, b int) int { return a + b })
	m.DraftError = ggui.Combine(m.Draft, m.Specs, func(d Draft, specs []sandbox.Spec) string {
		spec, err := d.Spec()
		if err != nil {
			return err.Error()
		}
		if err := spec.Validate(); err != nil {
			return err.Error()
		}
		for _, s := range specs {
			if s.Name == spec.Name && s.ID != spec.ID {
				return tr("another sandbox is called %s", spec.Name)
			}
		}
		return ""
	})
	m.DraftAction = ggui.Combine(m.Draft, m.States, func(d Draft, states map[string]sandbox.State) editorAction {
		if d.ID == "" {
			return editorAction{}
		}
		st := states[d.ID]
		active, ok := m.activeSpec(d.ID)
		spec, err := d.Spec()
		if err != nil {
			run := st.Status.Active()
			return editorAction{
				running:      run,
				control:      run && st.Control,
				dynamicPorts: run && st.DynamicPorts,
				bindAttach:   run && (st.BindAttach || st.LiveMounts),
				netQoS:       run && st.NetQoS,
				natPolicy:    run && st.NATPolicy,
			}
		}
		return editorActionOf(st, active, ok, spec)
	})
	m.DraftLine = m.Draft.Map(func(d Draft) string {
		spec, err := d.Spec()
		if err != nil {
			return "# " + err.Error()
		}
		p := sandbox.Paths{}
		if spec.Checkpoint {
			p = sandbox.Paths{ControlSocket: "<control.sock>", CheckpointDir: "<snapshot-dir>"}
		}
		return wrapCommand(spec.RunArgs(p))
	})
	m.initAppUpdater(dataDir)
	return m
}

// wrapCommand formats a rungv command over several lines, one flag and its
// value per line, as the recipes in the gvx docs are written.
func wrapCommand(args []string) string {
	var b strings.Builder
	b.WriteString("rungvx")
	i := 0
	if len(args) > 0 {
		b.WriteString(" " + rungv.Quote(args[0]))
		i = 1
	}
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") && !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && takesValue(a) {
			b.WriteString(" \\\n  " + rungv.Quote(a) + " " + rungv.Quote(args[i+1]))
			i += 2
			continue
		}
		if strings.HasPrefix(a, "-") {
			b.WriteString(" \\\n  " + rungv.Quote(a))
			i++
			continue
		}
		// The image and its command.
		b.WriteString(" \\\n  " + rungv.FormatArgs(args[i:]))
		break
	}
	return b.String()
}

func takesValue(flag string) bool {
	switch flag {
	case "-ro", "-isolate-lan", "--host-ca-certs", "--privileged=false",
		"-auto-publish", "--auto-publish",
		"-dynamic-ports", "--dynamic-ports",
		"-allow-bind-attach", "--allow-bind-attach",
		"-keep-blobs", "--keep-blobs":
		return false
	}
	return true
}

// Attach gives the model its host once the app exists.
func (m *Model) Attach(host ggui.Host) {
	m.host = host
	if m.UpdateCtrl != nil && host != nil {
		m.UpdateCtrl.SetPost(host.Post)
	}
}

// SetMenuSource supplies the application menu items for non-macOS windows
// that render ui.AppMenubar inside the window frame.
func (m *Model) SetMenuSource(fn func() []ggui.MenuItem) { m.appMenu = fn }

func (m *Model) appMenus() []ggui.MenuItem {
	if m.appMenu == nil {
		return nil
	}
	return m.appMenu()
}

// Supervisor is the model's process supervisor.
func (m *Model) Supervisor() *sandbox.Supervisor { return m.sup }

// Client returns the current rungv client; safe from any goroutine.
func (m *Model) Client() *rungv.Client {
	m.clientMu.Lock()
	defer m.clientMu.Unlock()
	if m.client == nil {
		return &rungv.Client{}
	}
	c := *m.client
	return &c
}

func (m *Model) post(fn func()) {
	if m.host != nil {
		m.host.Post(fn)
	}
}

// async runs work on a goroutine and done with its error on the UI thread.
func (m *Model) async(work func(ctx context.Context) error, done func(error)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		err := work(ctx)
		m.post(func() { done(err) })
	}()
}

func (m *Model) toastError(title string, err error) {
	desc := err.Error()
	if advice := rungv.Diagnose(desc, 0); advice != "" {
		desc += "\n" + advice
	}
	m.Toasts.Push(ui.Toast(title, desc).Error().Duration(8 * time.Second))
}

func (m *Model) toastOK(title, desc string) {
	m.Toasts.Push(ui.Toast(title, desc).Success())
}

// Confirm asks before running action.
func (m *Model) Confirm(title, text, label string, action func()) {
	m.ConfirmTitle.Set(title)
	m.ConfirmText.Set(text)
	m.ConfirmLabel.Set(label)
	m.confirmAction = action
	m.ConfirmSafe.Set(false)
	m.ConfirmOpen.Set(true)
}

// Offer asks before running an action that destroys nothing, such as
// starting what another action needs.
func (m *Model) Offer(title, text, label string, action func()) {
	m.Confirm(title, text, label, action)
	m.ConfirmSafe.Set(true)
}

// RunConfirmed runs the pending confirmation.
func (m *Model) RunConfirmed() {
	if fn := m.confirmAction; fn != nil {
		m.confirmAction = nil
		fn()
	}
}

// Go switches the page. The Containers page opens only while the Docker
// Engine runs, and the cluster's Pods, Workloads and Services only while
// the cluster does, as the sidebar and the menus that lead there say.
func (m *Model) Go(page string) {
	if (page == PageContainers || page == PageNetworks) && ggui.Peek(m.ContainersAccess) != ContainersOpen ||
		clusterPage(page) && ggui.Peek(m.PodsAccess) != PodsOpen {
		return
	}
	if prev := ggui.Peek(m.Page); prev != page && strings.HasPrefix(prev, PageExtensionPrefix) {
		m.cancelExtensionStream()
	}
	m.Page.Set(page)
	switch page {
	case PageImages:
		m.LoadImages()
		m.LoadDockerImages()
		m.LoadLocalRegistry()
	case PageVolumes:
		m.LoadVolumes()
	case PageSnapshots:
		m.LoadSnapshots()
	case PageDashboard:
		m.LoadDisk()
	case PageSettings:
		m.LoadDisk()
		m.LoadLocalRegistry()
	case PageLabs:
		if amd64BuilderPlatform() {
			m.CheckAmd64()
			m.refreshAmd64State()
			m.refreshMicroVMState()
		}
		m.refreshExtensions()
	case PageExtensions:
		m.refreshExtensions()
	case PageDocker:
		m.LoadDocker()
		m.LoadDockerImages()
		m.LoadDockerNetworks()
	case PageContainers:
		m.DockerNavOpen.Set(true)
		m.LoadDocker()
	case PageNetworks:
		m.DockerNavOpen.Set(true)
		m.LoadDocker()
		m.LoadDockerNetworks()
	case PageKubernetes:
		m.CheckKubeconfig()
		m.LoadPods()
	case PagePods, PageWorkloads, PageServices:
		m.KubeNavOpen.Set(true)
		m.LoadPods()
	}
}

// connectTab is the index of the Connect tab of the sandbox details.
const connectTab = 1

// ---- Engine -------------------------------------------------------------

// Start locates rungv, loads everything and starts the autostart
// sandboxes. Call it on the UI thread after Attach.
func (m *Model) Start() {
	m.refreshStates()
	if specs := ggui.Peek(m.Specs); len(specs) > 0 {
		m.Selected.Set(specs[0].ID)
	}
	if !m.Offline && ggui.Peek(m.Settings).SandboxEngine {
		m.SandboxEngineStatus.Set(SandboxEngineStatus{Opening: true})
	}
	m.DetectEngine(func(ok bool) {
		if ok {
			m.startWithApp()
		}
	})
	m.syncBundledDocker()
	m.refreshRungvPathState()
	m.startDockerProxy()
	m.followLocalRegistry()
	m.followLocalProxy()
	m.followMCPServer()
	if ggui.Peek(m.Settings).Extensions {
		m.initExtensions()
	}
	m.followExtensions()
	m.followDocker()
	m.followPods()
	m.followForwards()
	m.followSandboxUsage()
	m.followContainerUsage()
	m.followLinks()
	// A registration to start at login names the program; one that has
	// moved since is named again.
	m.refreshAutostart()
	if !ggui.Peek(m.Settings).NoUpdateCheck {
		m.CheckUpdate(true)
	}
	m.startAppUpdates()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			m.post(func() { m.Now.Set(time.Now()) })
		}
	}()
}

// DetectEngine looks for rungv and asks it about the host.
func (m *Model) DetectEngine(then func(ok bool)) {
	set := ggui.Peek(m.Settings)
	m.Engine.Update(func(e Engine) Engine { e.Loading = true; return e })
	type result struct {
		e        Engine
		c        *rungv.Client
		features rungv.Features
	}
	var r result
	m.async(func(ctx context.Context) error {
		path, err := rungv.Locate(set.RungvPath)
		r.e = Engine{Path: path, Checked: true}
		if err != nil {
			r.e.Err = tr("rungvx was not found. Install it, or set its path in Settings.")
			return nil
		}
		c := &rungv.Client{Path: path, CacheDir: rungv.ExpandHome(set.CacheDir), VolumeDir: rungv.ExpandHome(set.VolumeDir)}
		r.c = c
		if v, err := c.Version(ctx); err == nil {
			r.e.Version = v
		} else {
			r.e.Err = tr("rungvx did not run: ") + err.Error()
			return nil
		}
		info, err := c.Info(ctx)
		if err != nil {
			r.e.Err = tr("rungvx info failed: ") + err.Error()
			return nil
		}
		r.e.Info = info
		r.features = c.Features(ctx)
		return nil
	}, func(error) {
		m.clientMu.Lock()
		if r.e.Err == "" {
			m.client = r.c
		} else {
			m.client = nil
		}
		m.clientMu.Unlock()
		m.RungvFeatures.Set(r.features)
		m.Engine.Set(r.e)
		m.refreshRungvPathState()
		ok := r.e.Path != "" && r.e.Err == ""
		ready := ok && len(r.e.Info.AvailableBackends) > 0
		if ok {
			m.LoadImages()
			m.LoadVolumes()
			m.LoadDisk()
			m.LoadSnapshots()
		}
		if ready && ggui.Peek(m.Settings).SandboxEngine {
			m.openSandboxEngine()
		} else if ggui.Peek(m.Settings).SandboxEngine {
			m.SandboxEngineStatus.Set(SandboxEngineStatus{Opening: false})
		}
		if then != nil {
			then(ready)
		}
	})
}

// SignRungv signs the binary with the hypervisor entitlement (macOS).
func (m *Model) SignRungv() {
	path := ggui.Peek(m.Engine).Path
	if path == "" {
		return
	}
	m.async(func(ctx context.Context) error {
		return rungv.Sign(ctx, path)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Signing failed"), err)
			return
		}
		m.toastOK(tr("rungvx signed"), tr("Hypervisor.framework access is granted."))
		m.DetectEngine(nil)
	})
}

// ---- Sandboxes ----------------------------------------------------------

func (m *Model) onEvent(e sandbox.Event) {
	switch e.Kind {
	case sandbox.EventPortBusy:
		m.post(func() { m.announce(e) })
	case sandbox.EventReady, sandbox.EventExited:
		m.post(func() {
			m.refreshStates()
			m.announce(e)
		})
	default:
		m.dirty.Store(true)
		if m.scheduled.CompareAndSwap(false, true) {
			time.AfterFunc(120*time.Millisecond, func() {
				m.scheduled.Store(false)
				if m.dirty.Swap(false) {
					m.post(m.refreshStates)
				}
			})
		}
	}
}

func (m *Model) announce(e sandbox.Event) {
	spec, ok := m.spec(e.ID)
	if !ok || m.quitting {
		return
	}
	st := m.sup.State(e.ID)
	switch e.Kind {
	case sandbox.EventPortBusy:
		_, port, _ := net.SplitHostPort(e.Port)
		n, err := strconv.Atoi(port)
		if err != nil {
			return
		}
		m.Toasts.Push(ui.Toast(tr("Port %d is already in use on this computer", n),
			tr("A container publishes it, and will not be reachable at 127.0.0.1:%d until what uses it stops.", n)).Warning().Duration(8*time.Second).
			Action(tr("Logs"), func() { m.ShowSandbox(e.ID, 0) }))
	case sandbox.EventReady:
		m.toastOK(tr("%s is ready", spec.Name), m.readyText(spec))
		if spec.Recipe == recipes.Kubernetes {
			m.CheckKubeconfig()
			m.CheckLink(m.KubeLink) // it has written its kubeconfig now
		}
	case sandbox.EventExited:
		// An engine that ended before it was ever ready is not started again
		// with the app, to fail there again; one that ended after it was up
		// is, as the user left it running: it may have been ended with the
		// session at a logout.
		if (e.State.Status == sandbox.Failed || e.State.Status == sandbox.Exited) && e.State.ReadyAt.IsZero() {
			m.setResume(e.ID, false)
		}
		switch st.Status {
		case sandbox.Failed:
			desc := st.Error
			if st.Advice != "" {
				desc = st.Advice + "\n" + st.Error
			}
			m.Toasts.Push(ui.Toast(fmt.Sprintf(tr("%s failed (exit %d)"), spec.Name, st.ExitCode), desc).Error().Duration(10*time.Second).
				Action(tr("Logs"), func() { m.ShowSandbox(e.ID, 0) }))
		case sandbox.Exited:
			m.Toasts.Push(ui.Toast(tr("%s exited", spec.Name), tr("The guest command finished with code 0.")).Info())
		}
		m.LoadVolumes()
		m.LoadSnapshots()
	}
}

func (m *Model) readyText(s sandbox.Spec) string {
	switch s.Recipe {
	case recipes.Docker:
		return tr("The Docker Engine accepts connections on %s.", m.DockerHost())
	case recipes.Kubernetes:
		return tr("kubectl can reach the cluster with KUBECONFIG=%s.", recipes.Kubeconfig())
	case recipes.Registry:
		return tr("Push images to %s from the Docker Engine; pods pull them from there.", recipes.RegistryHost())
	}
	return tr("The sandbox is running.")
}

// refreshStates copies the supervisor's states and the selected logs into
// signals.
func (m *Model) refreshStates() {
	specs := ggui.Peek(m.Specs)
	states := make(map[string]sandbox.State, len(specs))
	for _, s := range specs {
		states[s.ID] = m.sup.State(s.ID)
	}
	m.States.Set(states)
	m.refreshLogs()
}

func (m *Model) refreshLogs() {
	id := ggui.Peek(m.Selected)
	if id == "" {
		if m.logSeq != -1 {
			m.Logs.Set(nil)
			m.logSeq = -1
		}
		return
	}
	logs := m.sup.Logs(id)
	if seq := logs.Seq(); seq != m.logSeq {
		m.logSeq = seq
		m.Logs.Set(logs.Snapshot())
	}
}

// Select shows a sandbox in the detail panel.
func (m *Model) Select(id string) {
	if ggui.Peek(m.Selected) == id {
		return
	}
	m.Selected.Set(id)
	m.DmesgMode.Set(false)
	m.DmesgLines.Set(nil)
	m.logSeq = -2
	m.refreshLogs()
}

// ShowSandbox opens the sandboxes page on a sandbox and detail tab.
func (m *Model) ShowSandbox(id string, tab int) {
	m.Select(id)
	m.Detail.Set(tab)
	m.Go(PageSandboxes)
}

func (m *Model) spec(id string) (sandbox.Spec, bool) {
	for _, s := range ggui.Peek(m.Specs) {
		if s.ID == id {
			return s, true
		}
	}
	return sandbox.Spec{}, false
}

func (m *Model) saveSpecs(specs []sandbox.Spec) {
	m.Specs.Set(specs)
	if err := m.store.SaveSandboxes(specs); err != nil {
		m.toastError(tr("Could not save sandboxes"), err)
	}
	m.refreshStates()
}

// StartSandbox starts a saved sandbox.
func (m *Model) StartSandbox(id string) {
	spec, ok := m.spec(id)
	if !ok || m.sup.State(id).Status.Active() {
		return // a cluster's agent its server started already
	}
	if !ggui.Peek(m.Engine).Ready() {
		m.toastError(tr("The engine is not ready"), errors.New(engineProblem(ggui.Peek(m.Engine))))
		return
	}
	if spec.Recipe == recipes.Docker {
		if refreshed, changed := m.syncDockerRecipeSpec(spec); changed {
			spec = refreshed
			specs := slices.Clone(ggui.Peek(m.Specs))
			if i := slices.IndexFunc(specs, func(s sandbox.Spec) bool { return s.ID == spec.ID }); i >= 0 {
				specs[i] = spec
				m.saveSpecs(specs)
			}
		}
	}
	if err := m.sup.Start(spec); err != nil {
		m.toastError(tr("Could not start %s", spec.Name), err)
		return
	}
	m.setResume(id, true)
	if spec.Recipe == recipes.Kubernetes {
		// The cluster starts whole: its agents with its server.
		m.forAgents(func(a string, st sandbox.State) {
			if !st.Status.Active() {
				m.StartSandbox(a)
			}
		})
	}
	m.refreshStates()
	m.LoadVolumes()
}

// syncDockerRecipeSpec refreshes the Docker Engine sandbox spec from current
// LAN-port and SSH-agent settings before starting (for example updating a
// rotated macOS SSH_AUTH_SOCK path after reboot).
func (m *Model) syncDockerRecipeSpec(spec sandbox.Spec) (sandbox.Spec, bool) {
	set := ggui.Peek(m.Settings)
	updated := spec
	if set.DockerExposeLAN && recipes.DockerAutoPublishAddr(updated) == "" {
		if !sandbox.HasAutoPublish(updated) {
			updated = sandbox.WithAutoPublish(updated, true)
		}
		updated = recipes.WithDockerAutoPublishAddr(updated, "0.0.0.0")
	}
	if set.DockerSSHAgent {
		updated = recipes.WithSSHAgentForward(updated, os.Getenv("SSH_AUTH_SOCK"))
	}
	if updated.Equal(spec) {
		return spec, false
	}
	return updated, true
}

// startWithApp starts what starts with the app: the sandboxes set to, and
// the recipe sandboxes the user left running when the app last ended.
func (m *Model) startWithApp() {
	for _, id := range launchStarts(ggui.Peek(m.Specs), ggui.Peek(m.Settings).Resume) {
		m.StartSandbox(id)
	}
}

// launchStarts picks from specs the sandboxes to start with the app.
func launchStarts(specs []sandbox.Spec, resume []string) []string {
	var ids []string
	for _, s := range specs {
		if s.AutoStart || s.Recipe != "" && slices.Contains(resume, s.ID) {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// setResume notes whether the user wants a recipe sandbox running, so that
// one left running starts again with the app. The recipe sandboxes are the
// engines the app hosts for docker and kubectl; any sandbox can be set to
// start with the app instead. It is the user's will that counts: starting
// and stopping one note it, and quitting, which stops every sandbox, does
// not.
func (m *Model) setResume(id string, on bool) {
	if spec, ok := m.spec(id); on && (!ok || spec.Recipe == "") {
		return
	}
	if slices.Contains(ggui.Peek(m.Settings).Resume, id) == on {
		return
	}
	m.UpdateSettings(func(s *store.Settings) {
		s.Resume = slices.DeleteFunc(slices.Clone(s.Resume), func(r string) bool { return r == id })
		if on {
			s.Resume = append(s.Resume, id)
		}
	})
}

func engineProblem(e Engine) string {
	switch {
	case e.Err != "":
		return e.Err
	case len(e.Info.AvailableBackends) == 0:
		return tr("rungvx found no execution backend on this host.")
	}
	return tr("rungvx is still being checked.")
}

// StopSandbox stops a running sandbox, as the user asked: a recipe
// sandbox is not started again with the app.
func (m *Model) StopSandbox(id string) {
	m.setResume(id, false)
	timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
	go func() { _ = m.sup.Stop(id, timeout) }()
	if spec, ok := m.spec(id); ok && spec.Recipe == recipes.Kubernetes {
		// and the cluster stops whole.
		m.forAgents(func(a string, st sandbox.State) {
			if st.Status.Active() {
				m.StopSandbox(a)
			}
		})
	}
}

// RestartSandbox stops and starts a sandbox.
func (m *Model) RestartSandbox(id string) {
	timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
	m.async(func(context.Context) error {
		return m.sup.Stop(id, timeout)
	}, func(error) { m.StartSandbox(id) })
}

// PauseSandbox freezes a running sandbox over its control socket.
// Any open terminal sessions in the sandbox are closed (and swept) first while
// the guest can still accept exec calls.
func (m *Model) PauseSandbox(id string) {
	spec, ok := m.spec(id)
	if !ok {
		return
	}
	if spec.Recipe == recipes.Kubernetes {
		m.forAgents(func(a string, st sandbox.State) {
			if st.Status == sandbox.Running && !st.Paused && st.Control {
				m.PauseSandbox(a)
			}
		})
	}
	m.async(func(ctx context.Context) error {
		m.closeTerminalsFor(sandboxTermTarget(id))
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if m.pauseSandboxFn != nil {
			return m.pauseSandboxFn(ctx, id)
		}
		return m.sup.Pause(ctx, id)
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not pause %s", spec.Name), err)
		}
		m.refreshStates()
	})
}

// ResumeSandbox thaws a paused sandbox over its control socket and drains any
// terminal sweeps that were deferred while it was paused.
func (m *Model) ResumeSandbox(id string) {
	spec, ok := m.spec(id)
	if !ok {
		return
	}
	if spec.Recipe == recipes.Kubernetes {
		m.forAgents(func(a string, st sandbox.State) {
			if st.Status.Active() && st.Paused {
				m.ResumeSandbox(a)
			}
		})
	}
	m.async(func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var err error
		if m.unpauseSandboxFn != nil {
			err = m.unpauseSandboxFn(ctx, id)
		} else {
			err = m.sup.Unpause(ctx, id)
		}
		if err != nil {
			return err
		}
		m.drainSandboxSweeps(ctx, id)
		return nil
	}, func(err error) {
		if err != nil {
			m.toastError(tr("Could not resume %s", spec.Name), err)
		}
		m.refreshStates()
	})
}

func (m *Model) sandboxControl(id string) (sandbox.Control, bool) {
	if m.controlOf != nil {
		return m.controlOf(id)
	}
	if m.sup == nil {
		return nil, false
	}
	return m.sup.Control(id)
}

func (m *Model) activeSpec(id string) (sandbox.Spec, bool) {
	if m.sup != nil {
		if s, ok := m.sup.ActiveSpec(id); ok {
			return s, true
		}
	}
	if m.Offline {
		if s, ok := m.spec(id); ok && ggui.Peek(m.States)[id].Status.Active() {
			return s, true
		}
	}
	return sandbox.Spec{}, false
}

func (m *Model) updateSandbox(ctx context.Context, id string, u rungv.Update) (rungv.Resources, error) {
	if m.updateSandboxFn != nil {
		return m.updateSandboxFn(ctx, id, u)
	}
	return m.sup.Update(ctx, id, u)
}

func (m *Model) applyLiveSandbox(ctx context.Context, id string, diff sandbox.LiveDiff) error {
	if m.applyLiveFn != nil {
		return m.applyLiveFn(ctx, id, diff)
	}
	if m.updateSandboxFn != nil && !diff.Update.IsZero() &&
		diff.NATPolicyFile == "" &&
		len(diff.AddPorts) == 0 && len(diff.RemovePorts) == 0 &&
		len(diff.AddMounts) == 0 && len(diff.RemoveMounts) == 0 {
		_, err := m.updateSandboxFn(ctx, id, diff.Update)
		return err
	}
	return m.sup.ApplyLive(ctx, id, diff)
}

func liveDiffToastDesc(diff sandbox.LiveDiff) string {
	if diff.NATPolicyFile == "" &&
		len(diff.AddPorts) == 0 && len(diff.RemovePorts) == 0 &&
		len(diff.AddMounts) == 0 && len(diff.RemoveMounts) == 0 &&
		diff.Update.NetTXBPS == nil && diff.Update.NetRXBPS == nil {
		return tr("The running sandbox has the new memory, CPU and LAN settings.")
	}
	return tr("The running sandbox has been updated without restarting.")
}

// AskDelete confirms and removes a sandbox.
func (m *Model) AskDelete(id string) {
	spec, ok := m.spec(id)
	if !ok {
		return
	}
	text := tr("The sandbox definition and its logs are removed. Volumes and images stay.")
	if m.sup.State(id).Status.Active() {
		text = tr("The sandbox is running and will be stopped. ") + text
	}
	// The cluster's server goes with its agents.
	gone := []string{id}
	if spec.Recipe == recipes.Kubernetes {
		if agents := agentsOf(ggui.Peek(m.Specs)); len(agents) > 0 {
			gone = append(gone, agents...)
			text = trn(len(agents), "Its %d other node is deleted with it. ", "Its %d other nodes are deleted with it. ") + text
		}
	}
	m.Confirm(tr("Delete %s?", spec.Name), text, tr("Delete sandbox"), func() {
		timeout := time.Duration(ggui.Peek(m.Settings).StopTimeout) * time.Second
		m.async(func(context.Context) error {
			var errs []error
			for _, g := range gone {
				errs = append(errs, m.sup.Stop(g, timeout))
			}
			return errors.Join(errs...)
		}, func(error) {
			for _, g := range gone {
				m.sup.Forget(g)
				m.setResume(g, false)
			}
			specs := slices.DeleteFunc(slices.Clone(ggui.Peek(m.Specs)), func(s sandbox.Spec) bool { return slices.Contains(gone, s.ID) })
			if slices.Contains(gone, ggui.Peek(m.Selected)) {
				next := ""
				if len(specs) > 0 {
					next = specs[0].ID
				}
				m.Select(next)
			}
			m.saveSpecs(specs)
			m.Toasts.Push(ui.Toast(tr("Deleted %s", spec.Name), tr("Its volumes are still listed under Volumes.")).Info())
		})
	})
}

// Duplicate copies a sandbox into the editor as a new one.
func (m *Model) Duplicate(id string) {
	spec, ok := m.spec(id)
	if !ok {
		return
	}
	c := spec.Clone()
	c.ID = ""
	c.Name = uniqueName(ggui.Peek(m.Specs), spec.Name+"-copy")
	c.AutoStart = false
	m.OpenEditor(c)
}

func uniqueName(specs []sandbox.Spec, name string) string {
	taken := map[string]bool{}
	for _, s := range specs {
		taken[s.Name] = true
	}
	if !taken[name] {
		return name
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", name, i); !taken[n] {
			return n
		}
	}
}

// NewSandbox opens the editor on an empty sandbox, or one for image.
func (m *Model) NewSandbox(image string) {
	spec := sandbox.Spec{Image: image}
	if image != "" {
		spec.Name = uniqueName(ggui.Peek(m.Specs), sandbox.SuggestName(image))
	}
	m.OpenEditor(spec)
}

// EditSandbox opens the editor on a saved sandbox.
func (m *Model) EditSandbox(id string) {
	if spec, ok := m.spec(id); ok {
		m.OpenEditor(spec)
	}
}

// OpenEditor shows the editor sheet with spec.
func (m *Model) OpenEditor(spec sandbox.Spec) {
	d := DraftFrom(spec)
	m.editorInitial = d
	m.EditorTouched.Set(false)
	m.Draft.Set(d)
	m.EditorOpen.Set(true)
}

// EditorDirty reports whether the draft in the open editor differs from when
// the editor opened.
func (m *Model) EditorDirty() bool {
	return ggui.Peek(m.Draft) != m.editorInitial
}

// CloseEditor closes the editor sheet, asking for confirmation when the user
// has unsaved changes.
func (m *Model) CloseEditor() {
	if !ggui.Peek(m.EditorOpen) {
		return
	}
	if m.EditorDirty() {
		m.Confirm(tr("Discard unsaved changes?"),
			tr("Your edits to the sandbox settings have not been saved."),
			tr("Discard changes"),
			func() { m.EditorOpen.Set(false) })
		return
	}
	m.EditorOpen.Set(false)
}

// isBusy reports whether an asynchronous mutation keyed by k is in flight.
func (m *Model) isBusy(k string) bool {
	return ggui.Peek(m.BusyOps)[k]
}

// setBusy marks or clears an in-flight mutation key k for reactive UI buttons.
func (m *Model) setBusy(k string, on bool) {
	m.BusyOps.Update(func(cur map[string]bool) map[string]bool {
		if cur[k] == on {
			return cur
		}
		next := maps.Clone(cur)
		if next == nil {
			next = map[string]bool{}
		}
		if on {
			next[k] = true
		} else {
			delete(next, k)
		}
		return next
	})
}

// SaveDraft stores the editor's sandbox, and starts it with start.
func (m *Model) SaveDraft(start bool) bool {
	m.EditorTouched.Set(true)
	d := ggui.Peek(m.Draft)
	spec, err := d.Spec()
	if err == nil && spec.Recipe == recipes.Docker {
		spec = recipes.EnsureDockerBackendPort(spec)
	}
	if err == nil {
		err = spec.Validate()
	}
	if err != nil {
		m.toastError(tr("Check the sandbox settings"), err)
		return false
	}
	specs := slices.Clone(ggui.Peek(m.Specs))
	for _, s := range specs {
		if s.Name == spec.Name && s.ID != spec.ID {
			m.toastError(tr("Name in use"), errors.New(tr("another sandbox is called %s", spec.Name)))
			return false
		}
	}
	st := sandbox.State{}
	if spec.ID != "" {
		st = m.sup.State(spec.ID)
		if !st.Status.Active() && m.Offline {
			st = ggui.Peek(m.States)[spec.ID]
		}
	}
	active := spec.ID != "" && st.Status.Active()
	var (
		applyLive   bool
		liveChecked bool
		liveDiff    sandbox.LiveDiff
	)
	if active && st.Control {
		if runningSpec, ok := m.activeSpec(spec.ID); ok {
			if diff, live := sandbox.ComputeLiveDiff(runningSpec, spec, st); live {
				liveChecked = true
				if !diff.IsZero() {
					applyLive = true
					liveDiff = diff
				}
			}
		}
	}
	if spec.ID == "" {
		spec.ID = sandbox.NewID()
		spec.CreatedAt = time.Now()
		specs = append(specs, spec)
	} else {
		i := slices.IndexFunc(specs, func(s sandbox.Spec) bool { return s.ID == spec.ID })
		if i < 0 {
			specs = append(specs, spec)
		} else {
			if active && !start && !liveChecked && !specs[i].Equal(spec) {
				m.Toasts.Push(ui.Toast(tr("Saved %s", spec.Name), tr("Restart the sandbox to apply the changes: ports and mounts are fixed while it runs.")).Warning())
			}
			specs[i] = spec
		}
	}
	m.saveSpecs(specs)
	m.EditorOpen.Set(false)
	m.Select(spec.ID)
	if applyLive && start {
		id, name, diff := spec.ID, spec.Name, liveDiff
		m.async(func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			return m.applyLiveSandbox(ctx, id, diff)
		}, func(err error) {
			if err != nil {
				m.toastError(tr("Could not change the running sandbox"), err)
				return
			}
			m.toastOK(tr("Applied to %s", name), liveDiffToastDesc(diff))
			m.refreshStates()
		})
		return true
	}
	if applyLive && !start {
		m.Toasts.Push(ui.Toast(tr("Saved %s", spec.Name), tr("Saved for the next start without changing the running sandbox.")).Info())
		return true
	}
	if liveChecked {
		// Controllable running sandbox with zero live diff (e.g. only AutoStart
		// changed or nothing changed): keep the spec saved without restarting.
		return true
	}
	if start {
		if active {
			m.RestartSandbox(spec.ID)
		} else {
			m.StartSandbox(spec.ID)
		}
		m.Detail.Set(0)
		m.Go(PageSandboxes)
	}
	return true
}

// CreateFromRecipe saves the preset's sandbox once and starts it.
func (m *Model) CreateFromRecipe(key string) {
	for _, s := range ggui.Peek(m.Specs) {
		if s.Recipe == key {
			m.Select(s.ID)
			if !m.sup.State(s.ID).Status.Active() {
				m.StartSandbox(s.ID)
			}
			return
		}
	}
	var spec sandbox.Spec
	switch key {
	case recipes.Docker:
		set := ggui.Peek(m.Settings)
		spec = recipes.WithSharedFolders(recipes.DockerEngine(), sharedSetting(set))
		if set.DockerExposeLAN {
			spec = recipes.WithDockerAutoPublishAddr(spec, "0.0.0.0")
		}
		if set.DockerSSHAgent {
			spec = recipes.WithSSHAgentForward(spec, os.Getenv("SSH_AUTH_SOCK"))
		}
	case recipes.Kubernetes:
		spec = recipes.KubernetesNodeWith(ggui.Peek(m.Settings).K3sServiceLB)
	case recipes.Registry:
		spec = recipes.ImageRegistry()
	default:
		return
	}
	spec.Name = uniqueName(ggui.Peek(m.Specs), spec.Name)
	m.saveSpecs(append(slices.Clone(ggui.Peek(m.Specs)), spec))
	m.Select(spec.ID)
	m.StartSandbox(spec.ID)
}

// upgradeRecipes brings the recipe sandboxes an earlier version of the app
// saved to what the recipes make now, as recipes.Upgrade does: they start
// so the next time, with what the user changed of them.
func upgradeRecipes(specs []sandbox.Spec) ([]sandbox.Spec, bool) {
	out := slices.Clone(specs)
	upgraded := false
	for i, s := range out {
		if up, changed := recipes.Upgrade(s); changed {
			out[i], upgraded = up, true
		}
	}
	return out, upgraded
}

// OpenRecipe shows what a preset's sandbox runs: the Docker and Kubernetes
// pages, and for the image registry, which has no page, its sandbox's
// Connect tab.
func (m *Model) OpenRecipe(key string) {
	switch key {
	case recipes.Docker:
		m.Go(PageDocker)
	case recipes.Kubernetes:
		m.Go(PageKubernetes)
	default:
		if r, ok := m.RecipeSandbox(ggui.Peek(m.Rows), key); ok {
			m.ShowSandbox(r.Spec.ID, connectTab)
		}
	}
}

// RecipeSandbox returns the sandbox made from a preset.
func (m *Model) RecipeSandbox(rows []Row, key string) (Row, bool) {
	for _, r := range rows {
		if r.Spec.Recipe == key {
			return r, true
		}
	}
	return Row{}, false
}

// ClearLogs empties the logs of a sandbox.
func (m *Model) ClearLogs(id string) {
	m.sup.Logs(id).Clear()
	m.logSeq = -2
	m.refreshLogs()
}

// LoadSandboxDmesg fetches buffered guest kernel logs (rungvx dmesg) for id.
func (m *Model) LoadSandboxDmesg(id string) {
	if id == "" {
		return
	}
	m.DmesgLoading.Set(true)
	var out string
	m.async(func(ctx context.Context) error {
		dCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var err error
		out, err = m.sup.Dmesg(dCtx, id, rungv.DmesgOptions{Tail: 500})
		return err
	}, func(err error) {
		m.DmesgLoading.Set(false)
		if ggui.Peek(m.Selected) != id {
			return
		}
		if err != nil {
			m.Toasts.Push(ui.Toast(tr("Could not read kernel logs"), err.Error()).Error())
			return
		}
		now := time.Now()
		var lines []sandbox.LogLine
		for i, line := range strings.Split(strings.TrimRight(out, "\r\n"), "\n") {
			if line == "" {
				continue
			}
			lines = append(lines, sandbox.LogLine{
				Seq:    int64(i + 1),
				Time:   now,
				Stream: sandbox.Stdout,
				Text:   line,
			})
		}
		m.DmesgLines.Set(lines)
	})
}

// SetSandboxLogLevel updates the running sandbox's Sentry log verbosity
// (rungvx debug --log-level warning|info|debug).
func (m *Model) SetSandboxLogLevel(id, level string) {
	spec, _ := m.spec(id)
	m.async(func(ctx context.Context) error {
		dCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return m.sup.SetLogLevel(dCtx, id, level)
	}, func(err error) {
		if err != nil {
			m.Toasts.Push(ui.Toast(tr("Could not change log level"), err.Error()).Error())
			return
		}
		m.Toasts.Push(ui.Toast(tr("Log level updated"), tr("%s now logs at %s level.", spec.Name, level)).Success())
	})
}

// ---- Snapshots ----------------------------------------------------------

// Checkpoint snapshots a running sandbox.
func (m *Model) Checkpoint(id string, leaveRunning bool) {
	spec, _ := m.spec(id)
	toast := m.Toasts.Push(ui.Toast(tr("Writing snapshot"), tr("Saving memory, tasks and volumes of %s…", spec.Name)).Loading())
	var dir string
	m.async(func(ctx context.Context) error {
		var err error
		dir, err = m.sup.Checkpoint(ctx, id, leaveRunning)
		return err
	}, func(err error) {
		if err != nil {
			m.Toasts.Update(toast, ui.Toast(tr("Snapshot failed"), err.Error()).Error())
			return
		}
		if !leaveRunning {
			m.setResume(id, false) // the user stopped it with the snapshot
		}
		what := tr("The sandbox stopped after the snapshot.")
		if leaveRunning {
			what = tr("The sandbox keeps running.")
		}
		m.Toasts.Update(toast, ui.Toast(tr("Snapshot saved"), what+tr(" Restore it from the Snapshots page.")).Success())
		_ = dir
		m.LoadSnapshots()
		m.refreshStates()
	})
}

// LoadSnapshots lists the bundles of every sandbox.
func (m *Model) LoadSnapshots() {
	specs := ggui.Peek(m.Specs)
	var snaps []sandbox.Snapshot
	m.async(func(context.Context) error {
		for _, s := range specs {
			snaps = append(snaps, m.sup.Snapshots(s.ID)...)
		}
		slices.SortFunc(snaps, func(a, b sandbox.Snapshot) int { return b.Time.Compare(a.Time) })
		return nil
	}, func(error) {
		m.Snapshots.Set(snaps)
		m.SnapshotsLoaded.Set(true)
	})
}

// Restore resumes a snapshot.
func (m *Model) Restore(snap sandbox.Snapshot) {
	spec, ok := m.spec(snap.SandboxID)
	if !ok {
		m.toastError(tr("Cannot restore"), errors.New(tr("the sandbox of this snapshot was deleted")))
		return
	}
	if m.sup.State(spec.ID).Status.Active() {
		m.toastError(tr("Cannot restore"), errors.New(tr("stop %s first", spec.Name)))
		return
	}
	if err := m.sup.Restore(spec, snap.Dir); err != nil {
		m.toastError(tr("Could not restore %s", spec.Name), err)
		return
	}
	m.setResume(spec.ID, true)
	m.ShowSandbox(spec.ID, 0)
	m.refreshStates()
}

// DeleteSnapshot removes a bundle after confirmation.
func (m *Model) DeleteSnapshot(snap sandbox.Snapshot) {
	m.Confirm(tr("Delete snapshot?"), tr("The bundle %s (%s) is removed from disk.", snap.Name, formatBytes(snap.Size)), tr("Delete snapshot"), func() {
		m.async(func(context.Context) error { return removeAll(snap.Dir) }, func(err error) {
			if err != nil {
				m.toastError(tr("Could not delete the snapshot"), err)
			}
			m.LoadSnapshots()
		})
	})
}
