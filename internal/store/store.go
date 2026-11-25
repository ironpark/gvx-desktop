// Package store keeps the desktop app's sandboxes and settings as JSON in
// the user's configuration directory.
package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"

	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// Local registry modes for Settings.LocalRegistry.
const (
	LocalRegistryAuto = "auto" // default: runs automatically while Docker or Kubernetes is active
	LocalRegistryOn   = "on"   // always running while GVX Desktop is open
	LocalRegistryOff  = "off"  // disabled
)

// Default loopback ports for the Labs local proxy and MCP server.
const (
	DefaultLocalProxyHTTPPort  = 18080
	DefaultLocalProxyHTTPSPort = 18443
	DefaultMCPPort             = 18787
)

// Default per-container micro-VM resources (matching apple/container).
const (
	DefaultMicroVMCPUs      = 4
	DefaultMicroVMMemoryMiB = 1024
)

// Settings are the app's preferences.
type Settings struct {
	RungvPath      string `json:"rungvPath,omitempty"` // empty: locate automatically
	CacheDir       string `json:"cacheDir,omitempty"`  // empty: rungv's default
	VolumeDir      string `json:"volumeDir,omitempty"` // empty: rungv's default, or CacheDir/volumes
	Theme          string `json:"theme,omitempty"`     // system (default), light, dark
	Language       string `json:"language,omitempty"`  // system (default), en, ko
	StopOnQuit     bool   `json:"stopOnQuit"`          // stop running sandboxes on quit without asking
	HideOnClose    bool   `json:"hideOnClose"`         // closing the window keeps the app in the tray
	StopTimeout    int    `json:"stopTimeout,omitempty"`
	SidebarNarrow  bool   `json:"sidebarNarrow,omitempty"`
	NoTray         bool   `json:"noTray,omitempty"`         // keep out of the menu bar or notification area
	SearchRegistry string `json:"searchRegistry,omitempty"` // the registry the pull dialog searches: hub (default), quay
	DockerPath     string `json:"dockerPath,omitempty"`     // the docker command; empty: find it
	DockerEndpoint string `json:"dockerEndpoint,omitempty"` // how the app serves the Docker API: socket, tcp; empty: the system's default
	DockerPort     int    `json:"dockerPort,omitempty"`     // the host port of a tcp endpoint; 0: 2375
	K3sServiceLB   bool   `json:"k3sServiceLB,omitempty"`   // the Kubernetes recipe runs k3s's load balancer, ServiceLB
	UpdateChannel  string `json:"updateChannel,omitempty"`  // where rungv is updated from: release (default), source
	SourceRepo     string `json:"sourceRepo,omitempty"`     // git repository for building rungv from source; empty: gfire-sigs/gvx
	SourceBranch   string `json:"sourceBranch,omitempty"`   // branch or tag for building rungv from source; empty: main
	NoUpdateCheck  bool   `json:"noUpdateCheck,omitempty"`  // do not ask GitHub for a newer rungv at launch
	LoginCommand   string `json:"loginCommand,omitempty"`   // what the start at login was registered to run, to register it again when the app moves
	// NoAppUpdateCheck keeps the app from asking GitHub for a newer GVX
	// Desktop by itself, at launch and once a day; SkipAppVersion is a
	// release of it the user chose to skip, which is not offered again.
	NoAppUpdateCheck bool   `json:"noAppUpdateCheck,omitempty"`
	SkipAppVersion   string `json:"skipAppVersion,omitempty"`
	// DockerSharedFolders are the host folders the Docker Engine recipe
	// shares at their own paths: nil for the default, the home folder, and
	// empty for none.
	DockerSharedFolders []string `json:"dockerSharedFolders"`
	// Resume names, by ID, the recipe sandboxes the user left running: the
	// app starts them again when it next starts. Quitting, which stops every
	// sandbox, leaves it as it is; the user stopping one takes it out.
	Resume []string `json:"resume,omitempty"`
	// DockerContextBefore is the docker context that was in use when the app
	// switched docker to gvx, to switch back to.
	DockerContextBefore string `json:"dockerContextBefore,omitempty"`
	// DockerBuilderBefore is the buildx builder selected for the gvx context
	// before the experimental sandbox engine switched it to gvx-desktop.
	DockerBuilderBefore string `json:"dockerBuilderBefore,omitempty"`
	// DockerBuilderAliasInstalled records that the experimental sandbox engine
	// ran `docker buildx install` (setting aliases.builder="buildx") so turning
	// the mode off can undo it with `docker buildx uninstall`.
	DockerBuilderAliasInstalled bool `json:"dockerBuilderAliasInstalled,omitempty"`
	// KubeContextBefore is kubectl's current context when the app switched
	// kubectl to gvx-k3s, to switch back to.
	KubeContextBefore string `json:"kubeContextBefore,omitempty"`
	// SandboxEngine runs containers directly as individual gvx sandboxes
	// served by the in-app Docker API (default). When false, the app uses
	// compatibility mode with the docker:dind sandbox recipe.
	SandboxEngine bool `json:"sandboxEngine"`
	// Amd64Builds adds a linux/amd64 node to the sandbox engine's
	// gvx-desktop builder, served by a small Virtualization.framework VM
	// with Rosetta (experimental, macOS on Apple silicon; meaningful only
	// with SandboxEngine on).
	Amd64Builds bool `json:"amd64Builds,omitempty"`
	// MicroVMContainers enables the per-container Linux micro-VM runtime
	// (Labs, Apple silicon macOS): runs linux/amd64 containers automatically
	// with Rosetta, and runs linux/arm64 containers on a real Linux 6.18
	// kernel when --runtime=microvm is requested.
	MicroVMContainers bool `json:"microVmContainers,omitempty"`
	// MicroVMCPUs and MicroVMMemoryMiB configure default per-VM resources for
	// micro-VM containers; 0 selects DefaultMicroVMCPUs / DefaultMicroVMMemoryMiB.
	MicroVMCPUs      int `json:"microVmCpus,omitempty"`
	MicroVMMemoryMiB int `json:"microVmMemoryMiB,omitempty"`
	// MicroVMImages lists image references or wildcard patterns (e.g. "postgres:16",
	// "postgres", "ghcr.io/org/*") that automatically run in the per-container
	// Linux micro-VM runtime when MicroVMContainers is enabled and no explicit
	// --runtime flag overrides it.
	MicroVMImages []string `json:"microVmImages,omitempty"`
	// DockerExposeLAN publishes container ports on 0.0.0.0 (all interfaces)
	// instead of loopback 127.0.0.1 only (opt-in; defaults to false).
	DockerExposeLAN bool `json:"docker_expose_lan,omitempty"`
	// DockerSSHAgent forwards the host SSH_AUTH_SOCK into Docker containers
	// at /run/host-services/ssh-auth.sock via rungv v0.21+ --unix-forward
	// (opt-in; defaults to false).
	DockerSSHAgent bool `json:"docker_ssh_agent,omitempty"`
	// LocalRegistry controls the built-in OCI image registry on 127.0.0.1:5001:
	// "auto" (default: starts automatically when Docker or Kubernetes is used),
	// "on" (always running), or "off" (disabled).
	LocalRegistry string `json:"localRegistry,omitempty"`
	// LocalProxy runs the built-in *.localhost HTTP/HTTPS reverse proxy (Labs, opt-in).
	LocalProxy bool `json:"localProxy,omitempty"`
	// LocalProxyHTTPPort and LocalProxyHTTPSPort configure the local proxy's
	// loopback ports; 0 selects DefaultLocalProxyHTTPPort / DefaultLocalProxyHTTPSPort.
	LocalProxyHTTPPort  int `json:"localProxyHttpPort,omitempty"`
	LocalProxyHTTPSPort int `json:"localProxyHttpsPort,omitempty"`
	// MCPServer runs the built-in Streamable HTTP MCP server for AI agents (Labs, opt-in).
	MCPServer bool `json:"mcpServer,omitempty"`
	// MCPPort configures the MCP server's loopback port; 0 selects DefaultMCPPort.
	MCPPort int `json:"mcpPort,omitempty"`
	// Extensions enables the GVX Desktop Extensions host runtime and dedicated Extensions sidebar page (Labs, opt-in).
	Extensions bool `json:"extensions,omitempty"`
	// DeveloperMode enables unsandboxed local binary extensions (Extensions page).
	DeveloperMode bool `json:"developerMode,omitempty"`
}

// LocalRegistryMode returns the normalized local registry mode
// (LocalRegistryAuto, LocalRegistryOn, or LocalRegistryOff).
func (s Settings) LocalRegistryMode() string {
	switch s.LocalRegistry {
	case LocalRegistryOn:
		return LocalRegistryOn
	case LocalRegistryOff:
		return LocalRegistryOff
	default:
		return LocalRegistryAuto
	}
}

func portOrDefault(port, def int) int {
	if port >= 1 && port <= 65535 {
		return port
	}
	return def
}

// LocalProxyHTTPPortOrDefault returns LocalProxyHTTPPort when in [1, 65535],
// or DefaultLocalProxyHTTPPort otherwise.
func (s Settings) LocalProxyHTTPPortOrDefault() int {
	return portOrDefault(s.LocalProxyHTTPPort, DefaultLocalProxyHTTPPort)
}

// LocalProxyHTTPSPortOrDefault returns LocalProxyHTTPSPort when in [1, 65535],
// or DefaultLocalProxyHTTPSPort otherwise.
func (s Settings) LocalProxyHTTPSPortOrDefault() int {
	return portOrDefault(s.LocalProxyHTTPSPort, DefaultLocalProxyHTTPSPort)
}

// MCPPortOrDefault returns MCPPort when in [1, 65535], or DefaultMCPPort otherwise.
func (s Settings) MCPPortOrDefault() int {
	return portOrDefault(s.MCPPort, DefaultMCPPort)
}

// MicroVMCPUsOrDefault returns MicroVMCPUs when >= 1, or DefaultMicroVMCPUs otherwise.
func (s Settings) MicroVMCPUsOrDefault() int {
	if s.MicroVMCPUs >= 1 {
		return s.MicroVMCPUs
	}
	return DefaultMicroVMCPUs
}

// MicroVMMemoryMiBOrDefault returns MicroVMMemoryMiB when >= 128, or
// DefaultMicroVMMemoryMiB otherwise.
func (s Settings) MicroVMMemoryMiBOrDefault() int {
	if s.MicroVMMemoryMiB >= 128 {
		return s.MicroVMMemoryMiB
	}
	return DefaultMicroVMMemoryMiB
}

// Equal makes Settings comparable for signals, which a list in it keeps
// == from doing.
func (s Settings) Equal(o Settings) bool {
	if !slices.Equal(s.Resume, o.Resume) || !slices.Equal(s.MicroVMImages, o.MicroVMImages) {
		return false
	}
	s.Resume, o.Resume = nil, nil
	s.MicroVMImages, o.MicroVMImages = nil, nil
	return reflect.DeepEqual(s, o) // which tells a nil list of shared folders from an empty one
}

// DefaultSettings are the settings of a first launch.
func DefaultSettings() Settings {
	return Settings{Theme: "system", StopOnQuit: true, HideOnClose: true, StopTimeout: 15, SandboxEngine: true}
}

// Store reads and writes the files under Dir.
type Store struct {
	Dir string
}

// DefaultDir is gvx-desktop under the user's configuration directory.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "gvx-desktop"), nil
}

func (s Store) path(name string) string { return filepath.Join(s.Dir, name) }

func (s Store) read(name string, v any) error {
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// write replaces a file atomically.
func (s Store) write(name string, v any) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, name+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.path(name))
}

// Sandboxes reads the saved sandboxes, oldest first.
func (s Store) Sandboxes() ([]sandbox.Spec, error) {
	var specs []sandbox.Spec
	if err := s.read("sandboxes.json", &specs); err != nil {
		return nil, err
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].CreatedAt.Before(specs[j].CreatedAt) })
	return specs, nil
}

// SaveSandboxes writes the sandboxes.
func (s Store) SaveSandboxes(specs []sandbox.Spec) error {
	if specs == nil {
		specs = []sandbox.Spec{}
	}
	return s.write("sandboxes.json", specs)
}

// Settings reads the settings, filling defaults.
func (s Store) Settings() (Settings, error) {
	set := DefaultSettings()
	err := s.read("settings.json", &set)
	if set.StopTimeout <= 0 {
		set.StopTimeout = 15
	}
	return set, err
}

// SaveSettings writes the settings.
func (s Store) SaveSettings(set Settings) error { return s.write("settings.json", set) }
