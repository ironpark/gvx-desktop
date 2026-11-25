package ext

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const validFullManifest = `
schema_version = 1
id = "github-actions"
version = "0.1.0-beta.1"
publisher = "ironpark"
name = "GitHub Actions"
description = "Run workflows locally."
icon = "workflow"
permissions = ["docker", "secrets", "storage", "localproxy", "network"]

[runtime]
type = "sandbox"
dockerfile = "Dockerfile"
context = "."
target = "runtime"
port = 18901
memory = "1g"
cpus = "1.5"
idle_timeout_sec = 30

[[contributes.view]]
id = "main"
title = "GitHub Actions"
icon = "workflow"
chrome = "toolbar"

[[contributes.command]]
id = "run-local"
title = "Run Local Workflow"

[[contributes.tool]]
id = "list_runners"
description = "List configured runners."

[contributes.tool.params]
repo_path = { type = "string", description = "Repo path", required = true }
limit = { type = "number", description = "Max items" }

[contributes.settings]
zebra = { type = "string", label = "Zebra", default = "z" }
alpha = { type = "boolean", default = true }
mode = { type = "select", options = ["fast", "safe"], default = "safe" }
count = { type = "number", default = 4 }
token = { type = "secret", label = "PAT" }

[contributes.web_ui]
port = 8080
path = "/ui"
title = "Actions Web"
`

func TestParseManifestValidAndOrderPreserved(t *testing.T) {
	m, err := ParseManifest([]byte(validFullManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.ID != "github-actions" || m.Version != "0.1.0-beta.1" || m.Publisher != "ironpark" {
		t.Fatalf("unexpected header: %+v", m)
	}
	if m.Runtime.Type != RuntimeSandbox || m.Runtime.Port != 18901 || m.Runtime.Memory != "1g" || m.Runtime.CPUs != "1.5" {
		t.Fatalf("unexpected runtime: %+v", m.Runtime)
	}
	if len(m.Views) != 1 || m.Views[0].Chrome != ViewChromeToolbar {
		t.Fatalf("unexpected views: %+v", m.Views)
	}
	if len(m.Commands) != 1 || m.Commands[0].ID != "run-local" {
		t.Fatalf("unexpected commands: %+v", m.Commands)
	}
	if len(m.Tools) != 1 || len(m.Tools[0].Params) != 2 {
		t.Fatalf("unexpected tools: %+v", m.Tools)
	}
	// Verify settings preserve exact declaration order from TOML via md.Keys().
	wantOrder := []string{"zebra", "alpha", "mode", "count", "token"}
	if len(m.Settings) != len(wantOrder) {
		t.Fatalf("settings len = %d, want %d", len(m.Settings), len(wantOrder))
	}
	for i, wantKey := range wantOrder {
		if m.Settings[i].Key != wantKey {
			t.Errorf("settings[%d].Key = %q, want %q", i, m.Settings[i].Key, wantKey)
		}
	}
	if m.Settings[1].Label != "alpha" {
		t.Errorf("expected omitted label to default to key, got %q", m.Settings[1].Label)
	}
	if m.WebUI == nil || m.WebUI.Port != 8080 || m.WebUI.Path != "/ui" {
		t.Fatalf("unexpected web_ui: %+v", m.WebUI)
	}
	if m.Hash == "" {
		t.Fatal("expected non-empty manifest Hash")
	}
}

func TestParseManifestDefaults(t *testing.T) {
	minimal := `
schema_version = 1
id = "minimal-ext"
version = "1.0.0"
publisher = "acme"
name = "Minimal"
`
	m, err := ParseManifest([]byte(minimal))
	if err != nil {
		t.Fatalf("ParseManifest(minimal): %v", err)
	}
	if m.Runtime.Type != RuntimeSandbox {
		t.Errorf("Runtime.Type = %q, want sandbox", m.Runtime.Type)
	}
	if m.Runtime.Dockerfile != "Dockerfile" {
		t.Errorf("Runtime.Dockerfile = %q, want Dockerfile", m.Runtime.Dockerfile)
	}
	if m.Runtime.Context != "." {
		t.Errorf("Runtime.Context = %q, want .", m.Runtime.Context)
	}
	if m.Runtime.Port != DefaultRuntimePort {
		t.Errorf("Runtime.Port = %d, want %d", m.Runtime.Port, DefaultRuntimePort)
	}
	if m.Runtime.Memory != DefaultRuntimeMemory {
		t.Errorf("Runtime.Memory = %q, want %s", m.Runtime.Memory, DefaultRuntimeMemory)
	}
}

func TestParseManifestBundledReservation(t *testing.T) {
	bundledTOML := `
schema_version = 1
id = "gvx-core"
version = "1.0.0"
publisher = "gvx"
name = "GVX Core"

[runtime]
image = "ghcr.io/ironpark/gvx-core:1.0.0"
`
	if _, err := ParseManifest([]byte(bundledTOML)); err == nil {
		t.Fatal("ParseManifest should reject gvx- prefix and publisher=gvx for third-party")
	}
	m, err := ParseBundledManifest([]byte(bundledTOML))
	if err != nil {
		t.Fatalf("ParseBundledManifest failed: %v", err)
	}
	if !m.Bundled || m.Runtime.Dockerfile != "" {
		t.Fatalf("unexpected bundled manifest: %+v", m)
	}
}

func TestParseManifestRejections(t *testing.T) {
	base := func(extra string) []byte {
		return []byte(`
schema_version = 1
id = "test-ext"
version = "1.2.3"
publisher = "acme"
name = "Test Ext"
` + extra)
	}

	cases := []struct {
		name string
		data []byte
	}{
		{"over_64kib", make([]byte, MaxManifestBytes+1)},
		{"unknown_top_key", base("unknown_field = true\n")},
		{"unknown_runtime_key", base("[runtime]\nunknown_rt = 1\n")},
		{"wrong_schema_version", []byte("schema_version = 2\nid=\"a\"\nversion=\"1.0.0\"\npublisher=\"b\"\nname=\"N\"\n")},
		{"bad_id", []byte("schema_version = 1\nid=\"Bad_ID\"\nversion=\"1.0.0\"\npublisher=\"b\"\nname=\"N\"\n")},
		{"bad_semver", []byte("schema_version = 1\nid=\"good-id\"\nversion=\"1.2\"\npublisher=\"b\"\nname=\"N\"\n")},
		{"bad_icon", base("icon = \"nonexistent-lucide-icon-xyz\"\n")},
		{"spoof_name", []byte("schema_version = 1\nid=\"good-id\"\nversion=\"1.0.0\"\npublisher=\"b\"\nname=\"Bad\u202eName\"\n")},
		{"spoof_control_newline", []byte("schema_version = 1\nid=\"good-id\"\nversion=\"1.0.0\"\npublisher=\"b\"\nname=\"Bad\nName\"\n")},
		{"spoof_view_title", base("[[contributes.view]]\nid=\"v\"\ntitle=\"Hi\u200bThere\"\n")},
		{"bad_view_chrome", base("[[contributes.view]]\nid=\"v\"\ntitle=\"V\"\nchrome=\"floating\"\n")},
		{"implicit_perm_host", base("permissions = [\"host\"]\n")},
		{"implicit_perm_toast", base("permissions = [\"ui:toast\"]\n")},
		{"unknown_perm", base("permissions = [\"net:github.com\"]\n")},
		{"dup_perm", base("permissions = [\"docker\", \"docker\"]\n")},
		{"binary_without_entrypoint", base("[runtime]\ntype = \"binary\"\n")},
		{"sandbox_empty_dockerfile_and_image", base("[runtime]\ntype = \"sandbox\"\ndockerfile = \"\"\n")},
		{"port_too_low", base("[runtime]\nport = 80\n")},
		{"port_too_high", base("[runtime]\nport = 70000\n")},
		{"negative_idle", base("[runtime]\nidle_timeout_sec = -1\n")},
		{"bad_memory", base("[runtime]\nmemory = \"lots\"\n")},
		{"zero_memory", base("[runtime]\nmemory = \"0m\"\n")},
		{"zero_memory_plain", base("[runtime]\nmemory = \"0\"\n")},
		{"zero_memory_float", base("[runtime]\nmemory = \"0.0g\"\n")},
		{"bad_cpus", base("[runtime]\ncpus = \"0\"\n")},
		{"underscore_cpus", base("[runtime]\ncpus = \"1_0\"\n")},
		{"hex_cpus", base("[runtime]\ncpus = \"0x1p0\"\n")},
		{"traversal_dockerfile", base("[runtime]\ndockerfile = \"../Dockerfile\"\n")},
		{"abs_dockerfile", base("[runtime]\ndockerfile = \"/Dockerfile\"\n")},
		{"backslash_context", base("[runtime]\ncontext = \"a\\\\b\"\n")},
		{"secret_with_default", base("[contributes.settings]\ntok = { type = \"secret\", default = \"x\" }\n")},
		{"dotted_secret_with_default", base("[contributes.settings]\nalpha.type = \"secret\"\nalpha.default = \"x\"\n")},
		{"select_without_options", base("[contributes.settings]\nmode = { type = \"select\" }\n")},
		{"string_with_options", base("[contributes.settings]\nmode = { type = \"string\", options = [\"a\"] }\n")},
		{"web_ui_without_localproxy", base("[contributes.web_ui]\nport = 8080\n")},
		{"web_ui_protocol_relative_path", base("permissions = [\"localproxy\"]\n[contributes.web_ui]\npath = \"//evil.com\"\n")},
		{"web_ui_query_path", base("permissions = [\"localproxy\"]\n[contributes.web_ui]\npath = \"/ui?x=1\"\n")},
		{"web_ui_fragment_path", base("permissions = [\"localproxy\"]\n[contributes.web_ui]\npath = \"/ui#frag\"\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseManifest(tc.data); err == nil {
				t.Fatalf("ParseManifest(%s) unexpectedly succeeded", tc.name)
			}
		})
	}
}

func TestParseManifestBounds(t *testing.T) {
	// >8 views
	var vb strings.Builder
	vb.WriteString("schema_version=1\nid=\"ext\"\nversion=\"1.0.0\"\npublisher=\"pub\"\nname=\"Ext\"\n")
	for i := 0; i < MaxViews+1; i++ {
		fmt.Fprintf(&vb, "[[contributes.view]]\nid=\"v%d\"\ntitle=\"View %d\"\n", i, i)
	}
	if _, err := ParseManifest([]byte(vb.String())); err == nil {
		t.Fatal("expected error for >8 views")
	}

	// >32 commands
	var cb strings.Builder
	cb.WriteString("schema_version=1\nid=\"ext\"\nversion=\"1.0.0\"\npublisher=\"pub\"\nname=\"Ext\"\n")
	for i := 0; i < MaxCommands+1; i++ {
		fmt.Fprintf(&cb, "[[contributes.command]]\nid=\"c%d\"\ntitle=\"Cmd %d\"\n", i, i)
	}
	if _, err := ParseManifest([]byte(cb.String())); err == nil {
		t.Fatal("expected error for >32 commands")
	}

	// >16 tools
	var tb strings.Builder
	tb.WriteString("schema_version=1\nid=\"ext\"\nversion=\"1.0.0\"\npublisher=\"pub\"\nname=\"Ext\"\n")
	for i := 0; i < MaxTools+1; i++ {
		fmt.Fprintf(&tb, "[[contributes.tool]]\nid=\"t_%d\"\ndescription=\"Tool %d\"\n", i, i)
	}
	if _, err := ParseManifest([]byte(tb.String())); err == nil {
		t.Fatal("expected error for >16 tools")
	}

	// >32 settings
	var sb strings.Builder
	sb.WriteString("schema_version=1\nid=\"ext\"\nversion=\"1.0.0\"\npublisher=\"pub\"\nname=\"Ext\"\n[contributes.settings]\n")
	for i := 0; i < MaxSettings+1; i++ {
		fmt.Fprintf(&sb, "s_%d = { type = \"string\" }\n", i)
	}
	if _, err := ParseManifest([]byte(sb.String())); err == nil {
		t.Fatal("expected error for >32 settings")
	}
}

func TestParseManifestDirAndSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tomlData := []byte(`
schema_version = 1
id = "my-ext"
version = "1.0.0"
publisher = "pub"
name = "My Ext"
`)
	if err := os.WriteFile(filepath.Join(dir, ManifestName), tomlData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ParseManifestDir(dir, "my-ext"); err != nil {
		t.Fatalf("ParseManifestDir valid failed: %v", err)
	}
	if _, err := ParseManifestDir(dir, "wrong-id"); err == nil {
		t.Fatal("ParseManifestDir should reject mismatched directory id")
	}

	if runtime.GOOS != "windows" {
		_ = os.Remove(filepath.Join(dir, "Dockerfile"))
		if err := os.Symlink(filepath.Join(outside, "Dockerfile"), filepath.Join(dir, "Dockerfile")); err != nil {
			t.Fatal(err)
		}
		if _, err := ParseManifestDir(dir, "my-ext"); err == nil {
			t.Fatal("ParseManifestDir should reject Dockerfile symlink escaping extension root")
		}
	}
}

func TestParseManifestDottedSettingsOrderPreserved(t *testing.T) {
	raw := []byte(`
schema_version = 1
id = "dotted-settings"
version = "1.0.0"
publisher = "acme"
name = "Dotted Settings"

[contributes.settings]
zeta = { type = "string", label = "Zeta" }
alpha.type = "string"
alpha.label = "Alpha"
alpha.default = "hello"

[contributes.settings.middle]
type = "number"
label = "Middle"
default = 42
`)
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	want := []string{"zeta", "alpha", "middle"}
	if len(m.Settings) != len(want) {
		t.Fatalf("settings count = %d, want %d", len(m.Settings), len(want))
	}
	for i, k := range want {
		if m.Settings[i].Key != k {
			t.Errorf("settings[%d].Key = %q, want %q", i, m.Settings[i].Key, k)
		}
	}
	if m.Settings[1].Label != "Alpha" || m.Settings[1].Default != "hello" {
		t.Fatalf("dotted alpha setting = %+v", m.Settings[1])
	}
}

func TestParseManifest_RejectsInvalidUTF8(t *testing.T) {
	raw := []byte("schema_version = 1\nid = \"bad-utf8\"\nversion = \"0.1.0\"\npublisher = \"acme\"\nname = \"Bad\xffName\"\n[runtime]\ntype = \"sandbox\"\nimage = \"alpine:3.20\"\n")
	if _, err := ParseManifest(raw); err == nil {
		t.Fatal("expected ParseManifest to reject invalid UTF-8 bytes")
	} else if !strings.Contains(strings.ToLower(err.Error()), "utf-8") {
		t.Fatalf("expected UTF-8 error message, got: %v", err)
	}
}

func TestParseManifestI18nAndLocalizationHelpers(t *testing.T) {
	raw := []byte(`
schema_version = 1
id = "demo-i18n"
version = "0.2.0"
publisher = "acme"
name = "Demo Actions"
description = "English description."

[runtime]
type = "sandbox"
image = "alpine:3.20"

[[contributes.view]]
id = "main"
title = "Demo View"

[[contributes.command]]
id = "run-now"
title = "Run Now"

[contributes.settings]
api_url = { type = "string", label = "API URL", description = "Endpoint URL." }

[i18n.ko]
name = "데모 액션"
description = "한국어 설명."
[i18n.ko.views]
main = "데모 뷰"
[i18n.ko.commands]
run-now = "지금 실행"
[i18n.ko.settings.api_url]
label = "API 주소"
description = "엔드포인트 주소입니다."

[i18n.zh-Hant]
description = "繁體中文說明。"
`)
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("ParseManifest with i18n: %v", err)
	}
	info := ExtensionInfo{
		ID:          m.ID,
		Name:        m.Name,
		Description: m.Description,
		Views:       m.ToViewInfos(),
		Commands:    m.ToCommandInfos(),
		Settings: []SettingSummary{
			{Key: "api_url", Label: m.Settings[0].Label, Description: m.Settings[0].Description},
		},
		I18n: cloneManifestI18n(m.I18n),
	}
	if got := info.LocalizedName("ko"); got != "데모 액션" {
		t.Fatalf("LocalizedName(ko) = %q", got)
	}
	if got := info.LocalizedName("ko-KR"); got != "데모 액션" {
		t.Fatalf("LocalizedName(ko-KR) = %q", got)
	}
	if got := info.LocalizedDescription("zh-Hant"); got != "繁體中文說明。" {
		t.Fatalf("LocalizedDescription(zh-Hant) = %q", got)
	}
	if got := info.LocalizedViewTitle("main", "ko"); got != "데모 뷰" {
		t.Fatalf("LocalizedViewTitle(ko) = %q", got)
	}
	if got := info.LocalizedCommandTitle("run-now", "ko"); got != "지금 실행" {
		t.Fatalf("LocalizedCommandTitle(ko) = %q", got)
	}
	if got := info.LocalizedSettingLabel("api_url", "ko"); got != "API 주소" {
		t.Fatalf("LocalizedSettingLabel(ko) = %q", got)
	}
	if got := info.LocalizedSettingDescription("api_url", "ko"); got != "엔드포인트 주소입니다." {
		t.Fatalf("LocalizedSettingDescription(ko) = %q", got)
	}
	// Fallback to English defaults for missing locale.
	if got := info.LocalizedCommandTitle("run-now", "fr"); got != "Run Now" {
		t.Fatalf("LocalizedCommandTitle(fr fallback) = %q", got)
	}

	// Reject invalid locale tag.
	badLocale := []byte(`
schema_version = 1
id = "bad-loc"
version = "0.1.0"
publisher = "acme"
name = "Bad"
[runtime]
type = "sandbox"
image = "alpine:3.20"
[i18n."../bad"]
name = "Nope"
`)
	if _, err := ParseManifest(badLocale); err == nil {
		t.Fatal("expected ParseManifest to reject invalid i18n locale tag")
	}
}
