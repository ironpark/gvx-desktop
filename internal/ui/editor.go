package ui

import (
	"image/color"
	"slices"
	"strings"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/runtime"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// editorAction describes the editor sheet's caption and primary action for a
// draft against the sandbox's runtime state.
type editorAction struct {
	running      bool
	control      bool
	dynamicPorts bool
	bindAttach   bool
	volumeAttach bool
	netQoS       bool
	natPolicy    bool
	live         bool
	hasDiff      bool
}

func editorActionOf(st sandbox.State, active sandbox.Spec, hasActive bool, edited sandbox.Spec) editorAction {
	if !st.Status.Active() {
		return editorAction{}
	}
	act := editorAction{
		running:      true,
		control:      st.Control,
		dynamicPorts: st.DynamicPorts,
		bindAttach:   st.BindAttach || st.LiveMounts,
		volumeAttach: st.VolumeAttach,
		netQoS:       st.NetQoS,
		natPolicy:    st.NATPolicy,
	}
	if !st.Control || !hasActive {
		return act
	}
	diff, live := sandbox.ComputeLiveDiff(active, edited, st)
	act.live = live
	act.hasDiff = !diff.IsZero()
	return act
}

func editorCaption(a editorAction) string {
	if a.running && a.control {
		if a.volumeAttach {
			return tr("Resources, bandwidth, NAT policy, loopback ports, and volume/bind/tmpfs mounts apply to a running sandbox without a restart. Other settings take a restart.")
		}
		if a.dynamicPorts || a.bindAttach || a.netQoS || a.natPolicy {
			return tr("Resources, bandwidth, NAT policy, loopback ports, and bind/tmpfs mounts apply to a running sandbox without a restart. Other settings take a restart.")
		}
		return tr("Memory, CPUs and LAN isolation apply to a running sandbox at once. Ports, mounts and the other settings take a restart.")
	}
	return tr("Ports and mounts are opened before the guest starts and stay fixed while it runs.")
}

func editorPrimaryLabel(a editorAction) string {
	switch {
	case !a.running:
		return tr("Save and start")
	case a.control && a.live && a.hasDiff:
		return tr("Save and apply")
	case a.control && a.live:
		return tr("Save")
	default:
		return tr("Save and restart")
	}
}

func rgbA(v uint32, a uint8) color.NRGBA {
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), a}
}

// editorSheet is the form that creates and edits a sandbox. Everything in
// it is fixed for a run, so the sheet says so where it matters and shows
// the exact rungv command it will run.
func editorSheet(m *Model) ggui.Widget {
	d := m.Draft
	str := func(sel func(*Draft) *string) ggui.Binding[string] { return d.Field(sel) }
	flag := func(sel func(*Draft) *bool) ggui.Binding[bool] { return d.Field(sel) }

	title := ggui.Map(d, func(x Draft) string {
		if x.ID == "" {
			return tr("New sandbox")
		}
		return tr("Edit %s", x.Name)
	})
	advancedOpen := ggui.State(false)
	ggui.Watch(m.EditorOpen, func(open bool) {
		if open {
			advancedOpen.Set(false)
		}
	})

	cached := ggui.Map(m.Images, func(l Listing[rungv.Image]) []string {
		var refs []string
		for _, img := range l.Items {
			if img.Repository != "<none>" {
				refs = append(refs, img.Ref())
			}
			if len(refs) == 8 {
				break
			}
		}
		return refs
	}).WithEqual(slices.Equal[[]string])
	imageChips := ggui.View(cached, func(refs []string) ggui.Widget {
		if len(refs) == 0 {
			return widget.Box()
		}
		var chips []ggui.Widget
		for _, ref := range refs {
			chips = append(chips, ui.Badge(ref).Outline().OnTap(func() {
				d.Update(func(x Draft) Draft {
					x.Image = ref
					if x.Name == "" {
						x.Name = uniqueName(ggui.Peek(m.Specs), sandbox.SuggestName(ref))
					}
					return x
				})
			}))
		}
		return widget.Column(ui.Caption(tr("Cached images")).Size(11), widget.Wrap(chips...).Gap(6)).Gap(6)
	})

	general := widget.Column(
		ui.Field(tr("Image"), ui.TextField(str(func(x *Draft) *string { return &x.Image })).Placeholder("nginx:alpine").Name(tr("Image"))).
			Help(tr("Any OCI image for linux/%s. A missing image is pulled at start.", goarch())),
		imageChips,
		ui.Field(tr("Name"), ui.TextField(str(func(x *Draft) *string { return &x.Name })).Placeholder("web").Name(tr("Sandbox name"))),
		ui.Field(tr("Command"), ui.TextField(str(func(x *Draft) *string { return &x.Command })).Placeholder(tr("Image default")).Name(tr("Command"))).
			Help(tr("Replaces the image's CMD and keeps its ENTRYPOINT. Quote words as in a shell.")),
	).Gap(14).Stretch()

	network := d.Field(func(x *Draft) *string { return &x.Network })
	isWG := ggui.Map(network, func(n string) bool { return n == "wg" })
	isNAT := ggui.Map(network, func(n string) bool { return n == "nat" || n == "" })
	autoPub := ggui.Bind(func() bool { return d.Get().AutoPublish }, func(v bool) {
		m.EditorTouched.Set(true)
		d.Update(func(x Draft) Draft {
			x.AutoPublish = v
			if !v {
				x.AutoPublishAddr = ""
			}
			return x
		})
	})
	isAutoPub := ggui.Map(d, func(x Draft) bool { return x.AutoPublish || strings.TrimSpace(x.AutoPublishAddr) != "" })

	ports := widget.Column(
		ui.Field(tr("Published ports"), ui.TextField(str(func(x *Draft) *string { return &x.Ports })).Multiline().Lines(3).
			Placeholder("8080:80\n127.0.0.1:9000-9019:9000-9019\nunix:~/.gvx/app.sock:3000").Name(tr("Published ports"))).
			Help(tr("One per line: [IP:][HOST:]GUEST[/udp]. Bound before the guest starts and fixed for the run: publish every port the guest will use, including ports of nested containers.")),
		ui.Field(tr("Unix socket forwards"), ui.TextField(str(func(x *Draft) *string { return &x.UnixForwards })).Multiline().Lines(2).
			Placeholder("/var/run/app.sock=~/.gvx/app.sock").Name(tr("Unix socket forwards"))).
			Help(tr("One per line: GUEST_PATH=HOST_SOCKET[,uid=N,gid=N,mode=0600]. Relays a guest Unix socket to a host socket under lockdown (rungvx v0.21+).")),
		ggui.If(isNAT, func() ggui.Widget {
			return widget.Column(
				ui.Switch(autoPub, tr("Auto-publish ports as the guest listens")).Small(),
				ggui.If(isAutoPub, func() ggui.Widget {
					return ui.Field(tr("Auto-publish bind address"), ui.TextField(str(func(x *Draft) *string { return &x.AutoPublishAddr })).
						Placeholder("127.0.0.1").Name(tr("Auto-publish bind address"))).
						Help(tr("Host IP for --auto-publish, such as 0.0.0.0. Leave empty for 127.0.0.1 only (requires rungvx v0.21+)."))
				}),
				ui.Field(tr("Allowed live port addresses"), ui.TextField(str(func(x *Draft) *string { return &x.DynamicPortAddrs })).Multiline().Lines(2).
					Placeholder("0.0.0.0\n192.168.1.10").Name(tr("Allowed live port addresses"))).
					Help(tr("IP literals, one per line or comma-separated, allowed for live port publishing under lockdown (rungvx v0.21+).")),
			).Gap(14).Stretch()
		}),
	).Gap(14).Stretch()

	workspace := str(func(x *Draft) *string { return &x.Workspace })
	storage := widget.Column(
		ui.Field(tr("Volumes"), ui.TextField(str(func(x *Draft) *string { return &x.Volumes })).Multiline().Lines(2).
			Placeholder("pgdata:/var/lib/postgresql/data\nscratch:/work:size=1g,ephemeral").Name(tr("Volumes"))).
			Help(tr("NAME:/guest/path[:ro][,size=512m][,ephemeral]. Each named volume is one journaled ext4 image; add size= to cap growth or ephemeral for a scratch volume (rungvx v0.22+).")),
		ui.Field(tr("Bind mounts"), ui.TextField(str(func(x *Draft) *string { return &x.Binds })).Multiline().Lines(2).
			Placeholder("~/src:/src\n~/app.conf:/etc/app.conf:ro").Name(tr("Bind mounts"))).
			Help(tr("HOST:/guest/path[:ro][,symlinks=contained]. A host directory or one file. Add symlinks=contained (rungvx v0.26+) to materialize safe relative symlinks on macOS and Linux. The sandbox may only reach what is mounted.")),
		ui.Field(tr("Workspace"), widget.Row(
			widget.Expanded(ui.TextField(workspace).Placeholder(tr("Folder mounted at /workspace")).Name(tr("Workspace folder"))),
			ui.Button(tr("Browse…"), func() { browseFolder(m, workspace, nil) }).Outline(),
		).Gap(8)),
		ui.Switch(flag(func(x *Draft) *bool { return &x.WorkspaceRO }), tr("Mount the workspace read-only")).Small(),
		ui.Switch(flag(func(x *Draft) *bool { return &x.WorkspaceSymlinks }), tr("Create host symbolic links in the workspace (rungvx v0.26+)")).Small(),
		ui.Field(tr("Persistent root file system"), ui.TextField(str(func(x *Draft) *string { return &x.RootfsVolume })).Placeholder(tr("Volume name, or empty for memory")).Name(tr("Root file system volume"))).
			Help(tr("Writes outside mounts go to memory and vanish at exit unless a root volume keeps them.")),
		ui.Switch(flag(func(x *Draft) *bool { return &x.ReadOnlyRoot }), tr("Mount root file system read-only")).Small(),
		ui.Field(tr("Memory mounts (tmpfs)"), ui.TextField(str(func(x *Draft) *string { return &x.Tmpfs })).Multiline().Lines(1).
			Placeholder("/tmp:size=256m").Name(tr("tmpfs mounts"))),
	).Gap(14).Stretch()

	showGPU := ggui.Combine(m.RungvFeatures, d, func(f rungv.Features, x Draft) bool {
		return f.GPU || (x.GPU != "" && x.GPU != "off")
	})
	gpuEnabled := ggui.Map(d, func(x Draft) bool {
		return x.GPU != "" && x.GPU != "off"
	})
	resources := widget.Column(
		widget.Row(
			widget.Expanded(ui.Field(tr("Memory limit"), ui.TextField(str(func(x *Draft) *string { return &x.Memory })).Placeholder(tr("Host RAM")).Name(tr("Memory limit"))).Help(tr("512m, 2g. The guest is killed (exit 137) when it passes the limit."))),
			widget.Expanded(ui.Field(tr("CPU limit"), ui.TextField(str(func(x *Draft) *string { return &x.CPUs })).Placeholder(tr("All CPUs")).Name(tr("CPU limit"))).Help(tr("1.5 CPUs. A hard cap on Windows only."))),
			widget.Expanded(ui.Field(tr("PIDs limit"), ui.TextField(str(func(x *Draft) *string { return &x.PidsLimit })).Placeholder(tr("Unlimited")).Name(tr("PIDs limit"))).Help(tr("Max guest tasks (cgroup2 pids.max)."))),
		).Gap(12).Align(widget.AlignStart),
		ggui.If(showGPU, func() ggui.Widget {
			missingRenderer := ggui.Combine(d, m.Engine, func(x Draft, e Engine) bool {
				return x.GPU != "" && x.GPU != "off" && e.Checked && e.Path != "" && e.GPURenderer == ""
			})
			return widget.Column(
				widget.Row(
					widget.Expanded(ui.Field(tr("GPU acceleration"), ui.Select(d.Field(func(x *Draft) *string { return &x.GPU })).
						Options([]string{"off", "venus", "virgl"}).Format(func(v string) string {
						switch v {
						case "venus":
							return "Venus (Vulkan)"
						case "virgl":
							return "VirGL (OpenGL)"
						default:
							return tr("Off")
						}
					}).Name(tr("GPU acceleration")))),
					ggui.If(gpuEnabled, func() ggui.Widget {
						return widget.Expanded(ui.Field(tr("GPU memory limit"), ui.TextField(str(func(x *Draft) *string { return &x.GPUMemoryLimit })).Placeholder("512m").Name(tr("GPU memory limit"))).
							Help(tr("Shared GPU memory cap, such as 512m or 2g.")))
					}),
				).Gap(12).Align(widget.AlignStart),
				ggui.If(missingRenderer, func() ggui.Widget {
					return ui.Caption(tr("gvx-gpu-renderer was not found. Enable GPU extension in Settings → rungvx updates and reinstall rungvx.")).Color(colorAmber)
				}),
			).Gap(6).Stretch()
		}),
	).Gap(14).Stretch()

	env := widget.Column(
		ui.Field(tr("Environment"), ui.TextField(str(func(x *Draft) *string { return &x.Env })).Multiline().Lines(3).
			Placeholder("KEY=value\nHTTP_PROXY").Name(tr("Environment variables"))).
			Help(tr("KEY=value per line. KEY alone copies the variable from this machine.")),
		ui.Field(tr("Environment files"), ui.TextField(str(func(x *Draft) *string { return &x.EnvFiles })).Multiline().Lines(2).
			Placeholder("~/.config/app.env").Name(tr("Environment files"))).
			Help(tr("One host .env file path per line (--env-file).")),
	).Gap(14).Stretch()

	netSec := widget.Column(
		ui.Field(tr("Network"), ui.ToggleGroup(network).Options([]string{"nat", "none", "wg"}).Format(func(s string) string {
			switch s {
			case "nat":
				return "NAT"
			case "none":
				return tr("None")
			}
			return "WireGuard"
		}).Outline().Name(tr("Network mode"))).Help(tr("NAT relays guest TCP/UDP through host sockets. WireGuard routes everything through a tunnel and needs a config.")),
		ggui.If(isWG, func() ggui.Widget {
			wg := str(func(x *Draft) *string { return &x.WGConf })
			return ui.Field(tr("WireGuard config"), widget.Row(
				widget.Expanded(ui.TextField(wg).Placeholder("~/sandbox-wg.conf").Name(tr("WireGuard config"))),
				ui.Button(tr("Browse…"), func() { browseFile(m, wg, nil) }).Outline(),
			).Gap(8))
		}),
		ggui.If(isNAT, func() ggui.Widget {
			pol := str(func(x *Draft) *string { return &x.NATPolicyFile })
			return widget.Column(
				widget.Row(
					widget.Expanded(ui.Field(tr("Upload limit (TX)"), ui.TextField(str(func(x *Draft) *string { return &x.NetTX })).Placeholder(tr("Unlimited")).Name(tr("Upload limit (TX)"))).
						Help(tr("e.g. 100m, 1g (bits/s)."))),
					widget.Expanded(ui.Field(tr("Download limit (RX)"), ui.TextField(str(func(x *Draft) *string { return &x.NetRX })).Placeholder(tr("Unlimited")).Name(tr("Download limit (RX)"))).
						Help(tr("e.g. 100m, 1g (bits/s)."))),
				).Gap(12).Align(widget.AlignStart),
				ui.Field(tr("NAT firewall policy"), widget.Row(
					widget.Expanded(ui.TextField(pol).Placeholder("~/nat-policy.yaml").Name(tr("NAT firewall policy"))),
					ui.Button(tr("Browse…"), func() { browseFile(m, pol, nil) }).Outline(),
				).Gap(8)).Help(tr("JSON or YAML NAT firewall rules file.")),
			).Gap(14).Stretch()
		}),
		ui.Switch(flag(func(x *Draft) *bool { return &x.IsolateLAN }), tr("Block access to the local network")).Small(),
		ui.Switch(flag(func(x *Draft) *bool { return &x.Privileged }), tr("Privileged inside the sandbox (raw sockets, iptables, setuid) — needed by dockerd")).Small(),
		ui.Switch(flag(func(x *Draft) *bool { return &x.HostCACerts }), tr("Trust this machine's root certificates (TLS-inspecting proxies)")).Small(),
		ui.Field(tr("Execution backend"), ui.Select(d.Field(func(x *Draft) *string { return &x.Backend })).
			Options([]string{"auto", "hvf", "whpx", "systrap"}).Format(func(v string) string {
			if v == "auto" {
				return tr("Auto")
			}
			return v
		}).Name(tr("Execution backend"))).
			Help(tr("auto picks hvf on macOS, whpx on Windows and systrap on Linux. Use whpx for untrusted code on Windows.")),
	).Gap(14).Stretch()

	advanced := widget.Column(
		widget.Row(
			widget.Expanded(ui.Field(tr("Hostname"), ui.TextField(str(func(x *Draft) *string { return &x.Hostname })).Placeholder("gvx").Name(tr("Hostname")))),
			widget.Expanded(ui.Field(tr("User"), ui.TextField(str(func(x *Draft) *string { return &x.User })).Placeholder(tr("Image default")).Name(tr("User")))),
		).Gap(12),
		widget.Row(
			widget.Expanded(ui.Field(tr("Working directory"), ui.TextField(str(func(x *Draft) *string { return &x.Workdir })).Placeholder(tr("Image default")).Name(tr("Working directory")))),
			widget.Expanded(ui.Field(tr("Pull policy"), ui.Select(d.Field(func(x *Draft) *string { return &x.Pull })).Options([]string{"missing", "always", "never"}).Format(func(v string) string {
				switch v {
				case "always":
					return tr("Always")
				case "never":
					return tr("Never")
				}
				return tr("If missing")
			}).Name(tr("Pull policy")))),
		).Gap(12),
		ui.Field(tr("Entrypoint"), ui.TextField(str(func(x *Draft) *string { return &x.Entrypoint })).Placeholder(tr("Image default")).Name(tr("Entrypoint"))),
		ui.Switch(flag(func(x *Draft) *bool { return &x.SetEntrypoint }), tr("Override the entrypoint even when empty (clears it)")).Small(),
		ui.Field(tr("Ready when the log shows"), ui.TextField(str(func(x *Draft) *string { return &x.ReadyPattern })).Placeholder(tr("Ready as soon as it starts")).Name(tr("Ready pattern"))).
			Help(tr("The sandbox shows Starting until a line contains this text.")),
		ui.Field(tr("Extra rungvx flags"), ui.TextField(str(func(x *Draft) *string { return &x.ExtraArgs })).Placeholder("--tune net.nat.mtu=9000").Name(tr("Extra flags"))),
	).Gap(14).Stretch()

	lifecycle := widget.Column(
		ui.Switch(flag(func(x *Draft) *bool { return &x.Checkpoint }), tr("Allow snapshots (checkpoint and restore)")).Small(),
		ui.Switch(flag(func(x *Draft) *bool { return &x.AutoStart }), tr("Start when GVX Desktop starts")).Small(),
	).Gap(10).Stretch()

	section := func(icon *icons.Glyph, title string, body ggui.Widget) ggui.Widget {
		return widget.Column(
			widget.Row(glyph(icon, 16).Color(colorBlue), ui.Heading(title).Size(14)).Gap(8),
			body,
		).Gap(12).Stretch()
	}

	// Only surface the red validation banner after the user edits the form or
	// attempts to save, so opening a blank "New sandbox" form stays calm.
	visibleErr := ggui.Derived(func() string {
		msg := m.DraftError.Get()
		if msg == "" {
			return ""
		}
		if !m.EditorTouched.Get() && m.Draft.Get() == m.editorInitial {
			return ""
		}
		return msg
	})
	errorBanner := ggui.View(visibleErr, func(msg string) ggui.Widget {
		if msg == "" {
			return widget.Box()
		}
		return widget.Padding(ui.Alert(tr("Needs attention"), msg).Destructive().Icon(icons.Alert), 6, 0, 2, 0)
	})

	form := widget.Column(
		section(lucide.Package, tr("Image & command"), general),
		ui.Divider(),
		section(lucide.Plug, tr("Ports"), ports),
		ui.Divider(),
		section(lucide.HardDrive, tr("Storage"), storage),
		ui.Divider(),
		section(lucide.Cpu, tr("Resources"), resources),
		ui.Divider(),
		section(lucide.SlidersHorizontal, tr("Environment"), env),
		ui.Divider(),
		section(lucide.ShieldCheck, tr("Network & security"), netSec),
		ui.Divider(),
		section(lucide.Camera, tr("Lifecycle"), lifecycle),
		ui.Collapsible(advancedOpen, tr("Advanced"), widget.Padding(advanced, 12, 0, 0, 0)),
		ui.Divider(),
		ui.CodeBlockOf(m.DraftLine).Header(tr("This runs")).Language("sh").Small().CopyButton(true),
	).Gap(18).Stretch()

	invalid := ggui.Map(m.DraftError, func(s string) bool { return s != "" })
	saveActions := ggui.View(m.DraftAction, func(a editorAction) ggui.Widget {
		icon := lucide.Play
		switch {
		case a.running && a.control && a.live:
			icon = lucide.Check
		case a.running:
			icon = lucide.RotateCcw
		}
		primary := ui.Button(editorPrimaryLabel(a), func() { m.SaveDraft(true) }).BindDisabled(invalid).Icon(glyph(icon, 14))
		if a.running && a.control && a.live && !a.hasDiff {
			return primary
		}
		return widget.Row(
			ui.Button(tr("Save"), func() { m.SaveDraft(false) }).Outline().BindDisabled(invalid),
			primary,
		).Gap(8).Align(widget.AlignCenter)
	})
	caption := ui.CaptionOf(ggui.Map(m.DraftAction, editorCaption))
	// The sheet stacks its own footer under the content; the form scrolls
	// in the room it has and the validation status + actions stay pinned
	// at the bottom edge.
	actions := widget.Row(
		ui.Button(tr("Cancel"), m.CloseEditor).Outline(),
		widget.Spacer(),
		saveActions,
	).Gap(8).Align(widget.AlignCenter)
	sheetOpen := ggui.Bind(m.EditorOpen.Get, func(open bool) {
		if !open {
			m.CloseEditor()
			return
		}
		m.EditorOpen.Set(true)
	})
	return ui.Sheet(sheetOpen, widget.Column(
		widget.Padding(ui.TitleOf(title).Size(20), 0, 32, 0, 0),
		caption,
		widget.Expanded(widget.Scroll(widget.Padding(form, 16, 12, 24, 0)).FadeEdges(12)),
		errorBanner,
		ui.Divider(),
		widget.Padding(actions, 12, 0, 0, 0),
	).Gap(6).Stretch()).Right().Size(600).Name(tr("Sandbox editor"))
}

// browseFolder asks for a folder and writes it to target.
func browseFolder(m *Model, target ggui.Binding[string], then func(string)) {
	host := ggui.UseHost()
	start := rungv.ExpandHome(ggui.Peek[string](target))
	ggui.Async(func() (string, error) {
		return host.Dialogs().PickFolder(runtime.FileDialog{Title: tr("Choose a folder"), Directory: start})
	}, func(path string, err error) {
		if err != nil || path == "" {
			return
		}
		target.Set(path)
		if then != nil {
			then(path)
		}
	})
}

// browseFile asks for a file and writes it to target.
func browseFile(m *Model, target ggui.Binding[string], then func(string)) {
	host := ggui.UseHost()
	ggui.Async(func() (string, error) {
		return host.Dialogs().OpenFile(runtime.FileDialog{Title: tr("Choose a file")})
	}, func(path string, err error) {
		if err != nil || path == "" {
			return
		}
		target.Set(path)
		if then != nil {
			then(path)
		}
	})
}
