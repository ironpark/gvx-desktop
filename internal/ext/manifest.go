package ext

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/ironpark/gvx-desktop/internal/uiblock"
)

// Manifest limits and defaults.
const (
	MaxManifestBytes = 64 << 10
	MaxPermissions   = 16
	MaxViews         = 8
	MaxCommands      = 32
	MaxTools         = 16
	MaxToolParams    = 16
	MaxSettings      = 32
	MaxOptions       = 32
	MaxLocales       = 16
	MaxTitle         = 80
	MaxDescription   = 300
	MinRuntimePort   = 1024
	MaxRuntimePort   = 65535
)

// Runtime types.
const (
	RuntimeSandbox = "sandbox"
	RuntimeBinary  = "binary"
)

// View chrome modes.
const (
	ViewChromeAuto    = "auto"
	ViewChromeToolbar = "toolbar"
)

// Setting types.
const (
	SettingString  = "string"
	SettingSecret  = "secret"
	SettingBoolean = "boolean"
	SettingNumber  = "number"
	SettingSelect  = "select"
)

var (
	partIDRe     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	toolIDRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	settingKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	semverRe     = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})(-[0-9A-Za-z.-]{1,32})?$`)
	memoryRe     = regexp.MustCompile(`^(?i)\d+(\.\d+)?[bkmgt]?$`)
	cpusRe       = regexp.MustCompile(`^\d+(?:\.\d+)?$`)
	iconNameRe   = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	localeTagRe  = regexp.MustCompile(`^[A-Za-z]{2,8}(?:-[A-Za-z0-9]{2,8})*$`)
)

var (
	viewChromes    = []string{ViewChromeAuto, ViewChromeToolbar}
	toolParamTypes = []string{"string", "number", "boolean"}
	settingTypes   = []string{SettingString, SettingSecret, SettingBoolean, SettingNumber, SettingSelect}
)

var lucideIconsOnce = sync.OnceValue(func() []string {
	names := uiblock.LucideIconNames()
	slices.Sort(names)
	return names
})

// ValidIcon reports whether name is a bundled Lucide icon identifier.
func ValidIcon(name string) bool {
	if len(name) == 0 || len(name) > 48 || !iconNameRe.MatchString(name) {
		return false
	}
	_, ok := slices.BinarySearch(lucideIconsOnce(), name)
	return ok
}

// Manifest is a validated extension.toml document.
type Manifest struct {
	SchemaVersion int
	ID            string
	Version       string
	Publisher     string
	Name          string
	Description   string
	Icon          string
	Permissions   []string
	Runtime       RuntimeDecl
	Views         []ViewDecl
	Commands      []CommandDecl
	Tools         []ToolDecl
	Settings      []SettingDecl
	WebUI         *WebUIDecl
	I18n          map[string]ManifestLocale
	Bundled       bool
	Hash          string
}

// RuntimeDecl configures how the extension runs.
type RuntimeDecl struct {
	Type           string
	Dockerfile     string
	Context        string
	Target         string
	Image          string
	Port           int
	Memory         string
	CPUs           string
	IdleTimeoutSec int
	DevEntrypoint  string
}

// ViewDecl declares a UI view contributed by the extension.
type ViewDecl struct {
	ID     string
	Title  string
	Icon   string
	Chrome string
}

// CommandDecl declares a command palette entry contributed by the extension.
type CommandDecl struct {
	ID    string
	Title string
}

// ToolDecl declares an MCP tool contributed by the extension.
type ToolDecl struct {
	ID          string
	Description string
	Params      []ToolParamDecl
}

// ToolParamDecl describes one parameter of a ToolDecl.
type ToolParamDecl struct {
	Name        string
	Type        string
	Description string
	Required    bool
}

// SettingDecl declares one user-configurable setting in extension.toml order.
type SettingDecl struct {
	Key         string
	Type        string
	Label       string
	Description string
	Options     []string
	Default     any
}

// WebUIDecl declares an optional localproxy HTTP web UI exposed by the extension.
type WebUIDecl struct {
	Port  int
	Path  string
	Title string
}

type rawManifest struct {
	SchemaVersion int        `toml:"schema_version"`
	ID            string     `toml:"id"`
	Version       string     `toml:"version"`
	Publisher     string     `toml:"publisher"`
	Name          string     `toml:"name"`
	Description   string     `toml:"description"`
	Icon          string     `toml:"icon"`
	Permissions   []string   `toml:"permissions"`
	Runtime       rawRuntime `toml:"runtime"`
	Contributes   struct {
		View []struct {
			ID     string `toml:"id"`
			Title  string `toml:"title"`
			Icon   string `toml:"icon"`
			Chrome string `toml:"chrome"`
		} `toml:"view"`
		Command []struct {
			ID    string `toml:"id"`
			Title string `toml:"title"`
		} `toml:"command"`
		Tool []struct {
			ID          string `toml:"id"`
			Description string `toml:"description"`
			Params      map[string]struct {
				Type        string `toml:"type"`
				Description string `toml:"description"`
				Required    bool   `toml:"required"`
			} `toml:"params"`
		} `toml:"tool"`
		Settings map[string]struct {
			Type        string   `toml:"type"`
			Label       string   `toml:"label"`
			Description string   `toml:"description"`
			Options     []string `toml:"options"`
			Default     any      `toml:"default"`
		} `toml:"settings"`
		WebUI *struct {
			Port  *int   `toml:"port"`
			Path  string `toml:"path"`
			Title string `toml:"title"`
		} `toml:"web_ui"`
	} `toml:"contributes"`
	I18n map[string]struct {
		Name        string            `toml:"name"`
		Description string            `toml:"description"`
		Views       map[string]string `toml:"views"`
		Commands    map[string]string `toml:"commands"`
		Settings    map[string]struct {
			Label       string `toml:"label"`
			Description string `toml:"description"`
		} `toml:"settings"`
	} `toml:"i18n"`
}

type rawRuntime struct {
	Type           string  `toml:"type"`
	Dockerfile     *string `toml:"dockerfile"`
	Context        *string `toml:"context"`
	Target         string  `toml:"target"`
	Image          string  `toml:"image"`
	Port           *int    `toml:"port"`
	Memory         string  `toml:"memory"`
	CPUs           string  `toml:"cpus"`
	IdleTimeoutSec int     `toml:"idle_timeout_sec"`
	DevEntrypoint  string  `toml:"dev_entrypoint"`
}

// ParseManifest parses and strictly validates a third-party extension.toml.
func ParseManifest(data []byte) (*Manifest, error) {
	return parseManifest(data, false)
}

// ParseBundledManifest parses and validates a first-party bundled extension.toml,
// allowing the reserved gvx- ID prefix and publisher = "gvx".
func ParseBundledManifest(data []byte) (*Manifest, error) {
	return parseManifest(data, true)
}

// ParseManifestDir reads, parses, and validates extension.toml inside dir,
// verifying expectedID (if non-empty) matches manifest.id and that relative
// runtime paths do not escape dir via symlinks.
func ParseManifestDir(dir, expectedID string) (*Manifest, error) {
	return parseManifestDir(dir, expectedID, false)
}

// ParseBundledManifestDir is like ParseManifestDir for bundled first-party extensions.
func ParseBundledManifestDir(dir, expectedID string) (*Manifest, error) {
	return parseManifestDir(dir, expectedID, true)
}

func parseManifestDir(dir, expectedID string, bundled bool) (*Manifest, error) {
	manifestPath, err := ResolveWithinRoot(dir, ManifestName, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestName, err)
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ManifestName, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ManifestName, err)
	}
	m, err := parseManifest(data, bundled)
	if err != nil {
		return nil, err
	}
	if expectedID != "" && m.ID != expectedID {
		return nil, fmt.Errorf("%s: id %q does not match directory name %q", ManifestName, m.ID, expectedID)
	}
	if m.Runtime.Context != "" {
		if _, err := ResolveWithinRoot(dir, m.Runtime.Context, true); err != nil {
			return nil, fmt.Errorf("runtime.context: %w", err)
		}
	}
	if m.Runtime.Dockerfile != "" {
		if _, err := ResolveWithinRoot(dir, m.Runtime.Dockerfile, false); err != nil {
			return nil, fmt.Errorf("runtime.dockerfile: %w", err)
		}
	}
	if m.Runtime.DevEntrypoint != "" {
		if _, err := ResolveWithinRoot(dir, m.Runtime.DevEntrypoint, false); err != nil {
			return nil, fmt.Errorf("runtime.dev_entrypoint: %w", err)
		}
	}
	return m, nil
}

func parseManifest(data []byte, bundled bool) (*Manifest, error) {
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("%s exceeds %d KiB limit", ManifestName, MaxManifestBytes>>10)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s must be valid UTF-8", ManifestName)
	}
	var raw rawManifest
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(&raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestName, err)
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		keys := make([]string, len(extra))
		for i, k := range extra {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown keys %s", ManifestName, strings.Join(keys, ", "))
	}

	sum := sha256.Sum256(data)
	m := &Manifest{
		SchemaVersion: raw.SchemaVersion,
		ID:            raw.ID,
		Version:       raw.Version,
		Publisher:     raw.Publisher,
		Name:          raw.Name,
		Description:   raw.Description,
		Icon:          raw.Icon,
		Bundled:       bundled,
		Hash:          hex.EncodeToString(sum[:]),
	}

	switch {
	case m.SchemaVersion != APIVersion:
		return nil, fmt.Errorf("schema_version %d is not supported (want %d)", m.SchemaVersion, APIVersion)
	case !ValidID(m.ID):
		return nil, fmt.Errorf("id %q must match ^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$", m.ID)
	case !bundled && (m.ID == "gvx" || strings.HasPrefix(m.ID, "gvx-")):
		return nil, fmt.Errorf("id %q is reserved for first-party gvx extensions", m.ID)
	case !semverRe.MatchString(m.Version):
		return nil, fmt.Errorf("version %q is not valid semver (major.minor.patch[-pre])", m.Version)
	case !ValidID(m.Publisher):
		return nil, fmt.Errorf("publisher %q must match ^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$", m.Publisher)
	case !bundled && m.Publisher == "gvx":
		return nil, fmt.Errorf("publisher %q is reserved for first-party gvx extensions", m.Publisher)
	}
	if err := checkText("name", m.Name, MaxTitle, true); err != nil {
		return nil, err
	}
	if err := checkText("description", m.Description, MaxDescription, false); err != nil {
		return nil, err
	}
	if m.Icon != "" && !ValidIcon(m.Icon) {
		return nil, fmt.Errorf("icon %q is not a bundled Lucide icon", m.Icon)
	}

	perms, err := CheckPermissions(raw.Permissions)
	if err != nil {
		return nil, err
	}
	m.Permissions = perms

	rt, err := checkRuntime(raw.Runtime)
	if err != nil {
		return nil, err
	}
	m.Runtime = rt

	if len(raw.Contributes.View) > MaxViews {
		return nil, fmt.Errorf("more than %d views", MaxViews)
	}
	for _, v := range raw.Contributes.View {
		if !partIDRe.MatchString(v.ID) {
			return nil, fmt.Errorf("view id %q must be lowercase letters, digits and hyphens", v.ID)
		}
		if slices.ContainsFunc(m.Views, func(o ViewDecl) bool { return o.ID == v.ID }) {
			return nil, fmt.Errorf("view %q declared twice", v.ID)
		}
		if err := checkText("view "+v.ID+" title", v.Title, MaxTitle, true); err != nil {
			return nil, err
		}
		if v.Icon != "" && !ValidIcon(v.Icon) {
			return nil, fmt.Errorf("view %s icon %q is not a bundled Lucide icon", v.ID, v.Icon)
		}
		chrome := v.Chrome
		if chrome == "" {
			chrome = ViewChromeAuto
		}
		if !slices.Contains(viewChromes, chrome) {
			return nil, fmt.Errorf("view %s: chrome %q must be %s or %s", v.ID, chrome, ViewChromeAuto, ViewChromeToolbar)
		}
		m.Views = append(m.Views, ViewDecl{
			ID:     v.ID,
			Title:  v.Title,
			Icon:   v.Icon,
			Chrome: chrome,
		})
	}

	if len(raw.Contributes.Command) > MaxCommands {
		return nil, fmt.Errorf("more than %d commands", MaxCommands)
	}
	for _, c := range raw.Contributes.Command {
		if !partIDRe.MatchString(c.ID) {
			return nil, fmt.Errorf("command id %q must be lowercase letters, digits and hyphens", c.ID)
		}
		if slices.ContainsFunc(m.Commands, func(o CommandDecl) bool { return o.ID == c.ID }) {
			return nil, fmt.Errorf("command %q declared twice", c.ID)
		}
		if err := checkText("command "+c.ID+" title", c.Title, MaxTitle, true); err != nil {
			return nil, err
		}
		m.Commands = append(m.Commands, CommandDecl{ID: c.ID, Title: c.Title})
	}

	if len(raw.Contributes.Tool) > MaxTools {
		return nil, fmt.Errorf("more than %d tools", MaxTools)
	}
	for _, tl := range raw.Contributes.Tool {
		if !toolIDRe.MatchString(tl.ID) {
			return nil, fmt.Errorf("tool id %q must be lowercase letters, digits and underscores", tl.ID)
		}
		if slices.ContainsFunc(m.Tools, func(o ToolDecl) bool { return o.ID == tl.ID }) {
			return nil, fmt.Errorf("tool %q declared twice", tl.ID)
		}
		if err := checkText("tool "+tl.ID+" description", tl.Description, MaxDescription, true); err != nil {
			return nil, err
		}
		if len(tl.Params) > MaxToolParams {
			return nil, fmt.Errorf("tool %s: more than %d params", tl.ID, MaxToolParams)
		}
		decl := ToolDecl{ID: tl.ID, Description: tl.Description}
		paramNames := make([]string, 0, len(tl.Params))
		for k := range tl.Params {
			paramNames = append(paramNames, k)
		}
		slices.Sort(paramNames)
		for _, k := range paramNames {
			rp := tl.Params[k]
			if !settingKeyRe.MatchString(k) {
				return nil, fmt.Errorf("tool %s param %q: names must be lowercase letters, digits and underscores", tl.ID, k)
			}
			typ := rp.Type
			if typ == "" {
				typ = "string"
			}
			if !slices.Contains(toolParamTypes, typ) {
				return nil, fmt.Errorf("tool %s param %s: type %q must be string, number, or boolean", tl.ID, k, typ)
			}
			if err := checkText("tool "+tl.ID+" param "+k+" description", rp.Description, MaxDescription, false); err != nil {
				return nil, err
			}
			decl.Params = append(decl.Params, ToolParamDecl{
				Name:        k,
				Type:        typ,
				Description: rp.Description,
				Required:    rp.Required,
			})
		}
		m.Tools = append(m.Tools, decl)
	}

	if len(raw.Contributes.Settings) > MaxSettings {
		return nil, fmt.Errorf("more than %d settings", MaxSettings)
	}
	seenSettings := make(map[string]bool, len(raw.Contributes.Settings))
	for _, k := range md.Keys() {
		if len(k) < 3 || k[0] != "contributes" || k[1] != "settings" {
			continue
		}
		key := k[2]
		if seenSettings[key] {
			continue
		}
		seenSettings[key] = true
		r, ok := raw.Contributes.Settings[key]
		if !ok {
			return nil, fmt.Errorf("setting %q missing from decoded contributes.settings", key)
		}
		s := SettingDecl{
			Key:         key,
			Type:        r.Type,
			Label:       r.Label,
			Description: r.Description,
			Options:     slices.Clone(r.Options),
			Default:     r.Default,
		}
		if err := checkSetting(&s); err != nil {
			return nil, err
		}
		m.Settings = append(m.Settings, s)
	}
	if len(m.Settings) != len(raw.Contributes.Settings) {
		return nil, fmt.Errorf("contributes.settings count mismatch: processed %d, decoded %d", len(m.Settings), len(raw.Contributes.Settings))
	}

	if raw.Contributes.WebUI != nil {
		if !slices.Contains(m.Permissions, PermLocalProxy) {
			return nil, fmt.Errorf("contributes.web_ui requires %q permission", PermLocalProxy)
		}
		w := raw.Contributes.WebUI
		port := m.Runtime.Port
		if w.Port != nil {
			port = *w.Port
			if port < MinRuntimePort || port > MaxRuntimePort {
				return nil, fmt.Errorf("contributes.web_ui.port %d must be between %d and %d", port, MinRuntimePort, MaxRuntimePort)
			}
		}
		webPath := w.Path
		if webPath == "" {
			webPath = "/"
		}
		if err := checkWebUIPath(webPath); err != nil {
			return nil, err
		}
		if err := checkText("contributes.web_ui.title", w.Title, MaxTitle, false); err != nil {
			return nil, err
		}
		m.WebUI = &WebUIDecl{
			Port:  port,
			Path:  webPath,
			Title: w.Title,
		}
	}

	if len(raw.I18n) > MaxLocales {
		return nil, fmt.Errorf("i18n: more than %d locales", MaxLocales)
	}
	if len(raw.I18n) > 0 {
		m.I18n = make(map[string]ManifestLocale, len(raw.I18n))
		for loc, entry := range raw.I18n {
			if !localeTagRe.MatchString(loc) {
				return nil, fmt.Errorf("i18n locale %q must be a valid BCP-47 tag", loc)
			}
			if err := checkText("i18n."+loc+".name", entry.Name, MaxTitle, false); err != nil {
				return nil, err
			}
			if err := checkText("i18n."+loc+".description", entry.Description, MaxDescription, false); err != nil {
				return nil, err
			}
			if len(entry.Views) > MaxViews {
				return nil, fmt.Errorf("i18n.%s.views: more than %d entries", loc, MaxViews)
			}
			var views map[string]string
			if len(entry.Views) > 0 {
				views = make(map[string]string, len(entry.Views))
				for vid, title := range entry.Views {
					if !partIDRe.MatchString(vid) {
						return nil, fmt.Errorf("i18n.%s.views: invalid view id %q", loc, vid)
					}
					if err := checkText("i18n."+loc+".views."+vid, title, MaxTitle, false); err != nil {
						return nil, err
					}
					views[vid] = title
				}
			}
			if len(entry.Commands) > MaxCommands {
				return nil, fmt.Errorf("i18n.%s.commands: more than %d entries", loc, MaxCommands)
			}
			var cmds map[string]string
			if len(entry.Commands) > 0 {
				cmds = make(map[string]string, len(entry.Commands))
				for cid, title := range entry.Commands {
					if !partIDRe.MatchString(cid) {
						return nil, fmt.Errorf("i18n.%s.commands: invalid command id %q", loc, cid)
					}
					if err := checkText("i18n."+loc+".commands."+cid, title, MaxTitle, false); err != nil {
						return nil, err
					}
					cmds[cid] = title
				}
			}
			if len(entry.Settings) > MaxSettings {
				return nil, fmt.Errorf("i18n.%s.settings: more than %d entries", loc, MaxSettings)
			}
			var sets map[string]SettingLocale
			if len(entry.Settings) > 0 {
				sets = make(map[string]SettingLocale, len(entry.Settings))
				for sk, sv := range entry.Settings {
					if !settingKeyRe.MatchString(sk) {
						return nil, fmt.Errorf("i18n.%s.settings: invalid setting key %q", loc, sk)
					}
					if err := checkText("i18n."+loc+".settings."+sk+".label", sv.Label, MaxTitle, false); err != nil {
						return nil, err
					}
					if err := checkText("i18n."+loc+".settings."+sk+".description", sv.Description, MaxDescription, false); err != nil {
						return nil, err
					}
					sets[sk] = SettingLocale{Label: sv.Label, Description: sv.Description}
				}
			}
			m.I18n[loc] = ManifestLocale{
				Name:        entry.Name,
				Description: entry.Description,
				Views:       views,
				Commands:    cmds,
				Settings:    sets,
			}
		}
	}

	return m, nil
}

func checkRuntime(raw rawRuntime) (RuntimeDecl, error) {
	typ := raw.Type
	if typ == "" {
		typ = RuntimeSandbox
	}
	if typ != RuntimeSandbox && typ != RuntimeBinary {
		return RuntimeDecl{}, fmt.Errorf("runtime.type %q must be %q or %q", typ, RuntimeSandbox, RuntimeBinary)
	}

	port := DefaultRuntimePort
	if raw.Port != nil {
		port = *raw.Port
		if port < MinRuntimePort || port > MaxRuntimePort {
			return RuntimeDecl{}, fmt.Errorf("runtime.port %d must be between %d and %d", port, MinRuntimePort, MaxRuntimePort)
		}
	}

	if raw.IdleTimeoutSec < 0 {
		return RuntimeDecl{}, fmt.Errorf("runtime.idle_timeout_sec %d must be >= 0", raw.IdleTimeoutSec)
	}

	image := raw.Image
	if image != "" {
		if err := checkText("runtime.image", image, MaxDescription, true); err != nil {
			return RuntimeDecl{}, err
		}
		if strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t") {
			return RuntimeDecl{}, fmt.Errorf("runtime.image %q is invalid", image)
		}
	}

	target := raw.Target
	if target != "" {
		if err := checkText("runtime.target", target, MaxTitle, true); err != nil {
			return RuntimeDecl{}, err
		}
		if strings.HasPrefix(target, "-") || strings.ContainsAny(target, " \t") {
			return RuntimeDecl{}, fmt.Errorf("runtime.target %q is invalid", target)
		}
	}

	memory := raw.Memory
	if memory == "" && typ == RuntimeSandbox {
		memory = DefaultRuntimeMemory
	}
	if memory != "" {
		if err := checkText("runtime.memory", memory, 32, true); err != nil {
			return RuntimeDecl{}, err
		}
		if !memoryRe.MatchString(memory) {
			return RuntimeDecl{}, fmt.Errorf("runtime.memory %q must look like 512m or 2g", memory)
		}
		numStr := strings.TrimRight(memory, "bBkKmMgGtT")
		memVal, err := strconv.ParseFloat(numStr, 64)
		if err != nil || math.IsNaN(memVal) || math.IsInf(memVal, 0) || memVal <= 0 {
			return RuntimeDecl{}, fmt.Errorf("runtime.memory %q must be greater than zero", memory)
		}
	}

	cpus := raw.CPUs
	if cpus != "" {
		if err := checkText("runtime.cpus", cpus, 32, true); err != nil {
			return RuntimeDecl{}, err
		}
		if strings.Contains(cpus, "_") || !cpusRe.MatchString(cpus) {
			return RuntimeDecl{}, fmt.Errorf("runtime.cpus %q must be a positive number", cpus)
		}
		v, err := strconv.ParseFloat(cpus, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return RuntimeDecl{}, fmt.Errorf("runtime.cpus %q must be a positive number", cpus)
		}
	}

	ctxRel := ""
	if raw.Context != nil {
		var err error
		ctxRel, err = ValidateRelPath(*raw.Context, true)
		if err != nil {
			return RuntimeDecl{}, fmt.Errorf("runtime.context: %w", err)
		}
	} else if typ == RuntimeSandbox {
		ctxRel = "."
	}

	dfRel := ""
	if raw.Dockerfile != nil {
		if *raw.Dockerfile != "" {
			var err error
			dfRel, err = ValidateRelPath(*raw.Dockerfile, false)
			if err != nil {
				return RuntimeDecl{}, fmt.Errorf("runtime.dockerfile: %w", err)
			}
		}
	} else if typ == RuntimeSandbox && image == "" {
		dfRel = "Dockerfile"
	}

	if typ == RuntimeSandbox && dfRel == "" && image == "" {
		return RuntimeDecl{}, fmt.Errorf("runtime.type %q requires runtime.dockerfile or runtime.image", RuntimeSandbox)
	}

	devEntry := raw.DevEntrypoint
	if devEntry != "" {
		var err error
		devEntry, err = ValidateRelPath(devEntry, false)
		if err != nil {
			return RuntimeDecl{}, fmt.Errorf("runtime.dev_entrypoint: %w", err)
		}
	}
	if typ == RuntimeBinary && devEntry == "" {
		return RuntimeDecl{}, fmt.Errorf("runtime.type %q requires runtime.dev_entrypoint", RuntimeBinary)
	}

	return RuntimeDecl{
		Type:           typ,
		Dockerfile:     dfRel,
		Context:        ctxRel,
		Target:         target,
		Image:          image,
		Port:           port,
		Memory:         memory,
		CPUs:           cpus,
		IdleTimeoutSec: raw.IdleTimeoutSec,
		DevEntrypoint:  devEntry,
	}, nil
}

func checkSetting(s *SettingDecl) error {
	if !settingKeyRe.MatchString(s.Key) {
		return fmt.Errorf("setting %q: keys must be lowercase letters, digits and underscores", s.Key)
	}
	if !slices.Contains(settingTypes, s.Type) {
		return fmt.Errorf("setting %s: type %q must be one of %s", s.Key, s.Type, strings.Join(settingTypes, ", "))
	}
	if s.Label == "" {
		s.Label = s.Key
	}
	if err := checkText("setting "+s.Key+" label", s.Label, MaxTitle, true); err != nil {
		return err
	}
	if err := checkText("setting "+s.Key+" description", s.Description, MaxDescription, false); err != nil {
		return err
	}
	if (s.Type == SettingSelect) != (len(s.Options) > 0) {
		return fmt.Errorf("setting %s: options are required for and limited to select settings", s.Key)
	}
	if len(s.Options) > MaxOptions {
		return fmt.Errorf("setting %s: more than %d options", s.Key, MaxOptions)
	}
	for i, o := range s.Options {
		if err := checkText("setting "+s.Key+" option", o, MaxTitle, true); err != nil {
			return err
		}
		if slices.Index(s.Options, o) != i {
			return fmt.Errorf("setting %s: option %q declared twice", s.Key, o)
		}
	}
	if s.Default == nil {
		return nil
	}
	if s.Type == SettingSecret {
		return fmt.Errorf("setting %s: secret settings cannot declare a default", s.Key)
	}
	ok := false
	switch v := s.Default.(type) {
	case string:
		if err := checkText("setting "+s.Key+" default", v, MaxDescription, false); err != nil {
			return err
		}
		ok = s.Type == SettingString || (s.Type == SettingSelect && slices.Contains(s.Options, v))
	case bool:
		ok = s.Type == SettingBoolean
	case int64:
		s.Default, ok = float64(v), s.Type == SettingNumber
	case float64:
		ok = s.Type == SettingNumber && !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	if !ok {
		return fmt.Errorf("setting %s: default %v does not match type %s", s.Key, s.Default, s.Type)
	}
	return nil
}

func checkWebUIPath(p string) error {
	if err := checkText("contributes.web_ui.path", p, MaxDescription, true); err != nil {
		return err
	}
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, " \t\\?#") || strings.Contains(p, "..") {
		return fmt.Errorf("contributes.web_ui.path %q must be a clean URL path starting with '/'", p)
	}
	if path.Clean(p) == ".." {
		return fmt.Errorf("contributes.web_ui.path %q is invalid", p)
	}
	return nil
}

func checkText(what, s string, max int, required bool) error {
	if required && strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s is empty", what)
	}
	if len([]rune(s)) > max {
		return fmt.Errorf("%s exceeds %d characters", what, max)
	}
	if strings.ContainsFunc(s, uiblock.Spoofing) {
		return fmt.Errorf("%s contains control or bidi spoofing characters", what)
	}
	return nil
}

// HasPermission reports whether perm is declared in m.Permissions.
func (m *Manifest) HasPermission(perm string) bool {
	return slices.Contains(m.Permissions, perm)
}

// ToViewInfos converts manifest views to UI ViewInfo entries.
func (m *Manifest) ToViewInfos() []ViewInfo {
	out := make([]ViewInfo, len(m.Views))
	for i, v := range m.Views {
		out[i] = ViewInfo{ID: v.ID, Title: v.Title, Icon: v.Icon, Chrome: v.Chrome}
	}
	return out
}

// ToCommandInfos converts manifest commands to UI CommandInfo entries.
func (m *Manifest) ToCommandInfos() []CommandInfo {
	out := make([]CommandInfo, len(m.Commands))
	for i, c := range m.Commands {
		out[i] = CommandInfo{ID: c.ID, Title: c.Title}
	}
	return out
}

// ToToolInfos converts manifest tools to UI ToolInfo entries.
func (m *Manifest) ToToolInfos() []ToolInfo {
	out := make([]ToolInfo, len(m.Tools))
	for i, t := range m.Tools {
		params := make([]ToolParamInfo, len(t.Params))
		for j, p := range t.Params {
			params[j] = ToolParamInfo{
				Name:        p.Name,
				Type:        p.Type,
				Description: p.Description,
				Required:    p.Required,
			}
		}
		out[i] = ToolInfo{ID: t.ID, Description: t.Description, Params: params}
	}
	return out
}
