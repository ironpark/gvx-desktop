// Package ext implements the core GVX Desktop extension host:
// manifest parsing, permission grants, KV storage, secret vault,
// per-extension settings, Git/local/OCI source acquisition, and BuildKit image builds.
package ext

import (
	"context"
	"maps"
	"strings"
	"time"
)

const (
	// APIVersion is the Host/Extension protocol and manifest schema version.
	APIVersion = 1

	// ManifestName is the filename of an extension manifest.
	ManifestName = "extension.toml"

	// DefaultRuntimePort is the default container/extension HTTP port.
	DefaultRuntimePort = 18900

	// DefaultRuntimeMemory is the default sandbox memory limit.
	DefaultRuntimeMemory = "512m"
)

// ExtensionState represents the lifecycle state of an installed extension.
type ExtensionState string

const (
	StateDiscovered ExtensionState = "discovered"
	StateInvalid    ExtensionState = "invalid"
	StateWaiting    ExtensionState = "waiting"
	StateBuilding   ExtensionState = "building"
	StateDisabled   ExtensionState = "disabled"
	StateStarting   ExtensionState = "starting"
	StateRunning    ExtensionState = "running"
	StateDegraded   ExtensionState = "degraded"
	StateCrashed    ExtensionState = "crashed"
	StateFailed     ExtensionState = "failed"
)

// IsActive reports whether the extension is starting, running, or degraded.
func (s ExtensionState) IsActive() bool {
	return s == StateStarting || s == StateRunning || s == StateDegraded
}

// CanRun reports whether the extension is eligible to be started.
func (s ExtensionState) CanRun() bool {
	return s != StateInvalid && s != StateWaiting && s != StateBuilding && s != StateDisabled && s != StateFailed
}

// SourceKind identifies how an extension was installed.
const (
	SourceGit   = "git"
	SourceLocal = "local"
	SourceImage = "image"
)

// SourceSpec persists how an extension was acquired and its built/pulled image metadata.
type SourceSpec struct {
	Kind      string `json:"kind"`
	Repo      string `json:"repo,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Commit    string `json:"commit,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
	ImageRef  string `json:"image_ref,omitempty"`
	ImageID   string `json:"image_id,omitempty"`
	BuiltAt   string `json:"built_at,omitempty"`
}

// ViewInfo describes a contributed extension view for UI navigation.
type ViewInfo struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Icon   string `json:"icon,omitempty"`
	Chrome string `json:"chrome"`
}

// CommandInfo describes a contributed command palette entry.
type CommandInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ToolParamInfo describes a parameter of a contributed MCP tool.
type ToolParamInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ToolInfo describes a contributed MCP tool.
type ToolInfo struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Params      []ToolParamInfo `json:"params,omitempty"`
}

// SettingSummary describes a setting for UI rendering; secret values are never populated.
type SettingSummary struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	Description string   `json:"description,omitempty"`
	Options     []string `json:"options,omitempty"`
	Default     any      `json:"default,omitempty"`
	Set         bool     `json:"set"`
	Value       string   `json:"value,omitempty"`
}

// WebUIInfo describes an extension's optional localproxy web UI endpoint.
type WebUIInfo struct {
	Port  int    `json:"port"`
	Path  string `json:"path"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

// BuildLogEntry is one timestamped build output line.
type BuildLogEntry struct {
	Time time.Time `json:"time"`
	Line string    `json:"line"`
}

// SecretStoreStatus describes where secrets are currently persisted.
type SecretStoreStatus struct {
	Store  string `json:"store"`
	Detail string `json:"detail,omitempty"`
}

// BadgeInfo describes a sidebar badge pushed via POST /v1/ui/badge.
type BadgeInfo struct {
	Text    string `json:"text,omitempty"`
	Tone    string `json:"tone,omitempty"`
	Tooltip string `json:"tooltip,omitempty"`
}

// SettingLocale holds localized label and description for one manifest setting.
type SettingLocale struct {
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// ManifestLocale holds localized user-facing metadata declared under [i18n.<locale>]
// in extension.toml.
type ManifestLocale struct {
	Name        string                   `json:"name,omitempty"`
	Description string                   `json:"description,omitempty"`
	Views       map[string]string        `json:"views,omitempty"`
	Commands    map[string]string        `json:"commands,omitempty"`
	Settings    map[string]SettingLocale `json:"settings,omitempty"`
}

// ExtensionInfo is the host's read-only view of an extension for UI and status reporting.
type ExtensionInfo struct {
	ID             string                    `json:"id"`
	Name           string                    `json:"name"`
	Description    string                    `json:"description,omitempty"`
	Icon           string                    `json:"icon,omitempty"`
	Version        string                    `json:"version"`
	Publisher      string                    `json:"publisher"`
	Bundled        bool                      `json:"bundled,omitempty"`
	State          ExtensionState            `json:"state"`
	Error          string                    `json:"error,omitempty"`
	Badge          BadgeInfo                 `json:"badge,omitempty"`
	Source         SourceSpec                `json:"source"`
	RuntimeType    string                    `json:"runtime_type"`
	Unsandboxed    bool                      `json:"unsandboxed,omitempty"`
	Permissions    []string                  `json:"permissions,omitempty"`
	Granted        []string                  `json:"granted,omitempty"`
	Denied         []string                  `json:"denied,omitempty"`
	Asked          []string                  `json:"asked,omitempty"`
	AllowedFolders []string                  `json:"allowed_folders,omitempty"`
	Views          []ViewInfo                `json:"views,omitempty"`
	Commands       []CommandInfo             `json:"commands,omitempty"`
	Tools          []ToolInfo                `json:"tools,omitempty"`
	Settings       []SettingSummary          `json:"settings,omitempty"`
	WebUI          *WebUIInfo                `json:"web_ui,omitempty"`
	I18n           map[string]ManifestLocale `json:"i18n,omitempty"`
	StorageUsed    int                       `json:"storage_used"`
	StorageQuota   int                       `json:"storage_quota"`
}

func (info ExtensionInfo) localeEntry(locale string) (ManifestLocale, bool) {
	if len(info.I18n) == 0 {
		return ManifestLocale{}, false
	}
	norm := strings.ReplaceAll(strings.TrimSpace(locale), "_", "-")
	if norm == "" {
		return ManifestLocale{}, false
	}
	if e, ok := info.I18n[norm]; ok {
		return e, true
	}
	lower := strings.ToLower(norm)
	for k, e := range info.I18n {
		if strings.ToLower(strings.ReplaceAll(k, "_", "-")) == lower {
			return e, true
		}
	}
	if base, _, ok := strings.Cut(lower, "-"); ok && base != "" {
		if e, ok := info.I18n[base]; ok {
			return e, true
		}
		for k, e := range info.I18n {
			if strings.ToLower(k) == base {
				return e, true
			}
		}
	}
	return ManifestLocale{}, false
}

// LocalizedName returns the localized extension name for locale, falling back to Name/ID.
func (info ExtensionInfo) LocalizedName(locale string) string {
	if e, ok := info.localeEntry(locale); ok && e.Name != "" {
		return e.Name
	}
	if info.Name != "" {
		return info.Name
	}
	return info.ID
}

// LocalizedDescription returns the localized extension description for locale.
func (info ExtensionInfo) LocalizedDescription(locale string) string {
	if e, ok := info.localeEntry(locale); ok && e.Description != "" {
		return e.Description
	}
	return info.Description
}

// LocalizedViewTitle returns the localized title for viewID, falling back to the default view title.
func (info ExtensionInfo) LocalizedViewTitle(viewID, locale string) string {
	if e, ok := info.localeEntry(locale); ok {
		if title := e.Views[viewID]; title != "" {
			return title
		}
	}
	for _, v := range info.Views {
		if v.ID == viewID && v.Title != "" {
			return v.Title
		}
	}
	return ""
}

// LocalizedCommandTitle returns the localized title for cmdID, falling back to the default command title.
func (info ExtensionInfo) LocalizedCommandTitle(cmdID, locale string) string {
	if e, ok := info.localeEntry(locale); ok {
		if title := e.Commands[cmdID]; title != "" {
			return title
		}
	}
	for _, c := range info.Commands {
		if c.ID == cmdID && c.Title != "" {
			return c.Title
		}
	}
	return cmdID
}

// LocalizedSettingLabel returns the localized label for setting key.
func (info ExtensionInfo) LocalizedSettingLabel(key, locale string) string {
	if e, ok := info.localeEntry(locale); ok {
		if s, ok := e.Settings[key]; ok && s.Label != "" {
			return s.Label
		}
	}
	for _, s := range info.Settings {
		if s.Key == key && s.Label != "" {
			return s.Label
		}
	}
	return key
}

// LocalizedSettingDescription returns the localized description for setting key.
func (info ExtensionInfo) LocalizedSettingDescription(key, locale string) string {
	if e, ok := info.localeEntry(locale); ok {
		if s, ok := e.Settings[key]; ok && s.Description != "" {
			return s.Description
		}
	}
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Description
		}
	}
	return ""
}

// I18nEqual reports whether two ExtensionInfo values carry identical manifest i18n catalogs.
func (info ExtensionInfo) I18nEqual(other ExtensionInfo) bool {
	if len(info.I18n) != len(other.I18n) {
		return false
	}
	for k, a := range info.I18n {
		b, ok := other.I18n[k]
		if !ok || a.Name != b.Name || a.Description != b.Description ||
			!maps.Equal(a.Views, b.Views) || !maps.Equal(a.Commands, b.Commands) || !maps.Equal(a.Settings, b.Settings) {
			return false
		}
	}
	return true
}

func cloneManifestI18n(src map[string]ManifestLocale) map[string]ManifestLocale {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]ManifestLocale, len(src))
	for k, v := range src {
		out[k] = ManifestLocale{
			Name:        v.Name,
			Description: v.Description,
			Views:       maps.Clone(v.Views),
			Commands:    maps.Clone(v.Commands),
			Settings:    maps.Clone(v.Settings),
		}
	}
	return out
}

// BuildRequest configures a BuildKit OCI image build for an extension.
type BuildRequest struct {
	Dockerfile string
	ContextDir string
	Target     string
	Platform   string
	Tags       []string
	Labels     map[string]string
	BuildArgs  map[string]string
	NoCache    bool
	Pull       bool
}

// BuildResult holds the resulting OCI image ID and tags from a build.
type BuildResult struct {
	ImageID string
	Tags    []string
}

// ImageBuilder builds an OCI image from a local context directory and streams build logs.
type ImageBuilder interface {
	Build(ctx context.Context, req BuildRequest, onLine func(string)) (imageID string, err error)
}

// ImagePuller pulls a prebuilt OCI image reference (implemented by *rungv.Client).
type ImagePuller interface {
	Pull(ctx context.Context, ref string, line func(string)) error
}
