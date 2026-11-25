package ui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/engine"
	"github.com/ironpark/gvx-desktop/internal/localreg"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
	"github.com/ironpark/gvx-desktop/internal/store"
)

// ---- Images -------------------------------------------------------------

func imagesPage(m *Model) ggui.Widget {
	type headerKey struct {
		view int
		se   bool
	}
	hk := ggui.Derived(func() headerKey {
		return headerKey{view: m.ImageView.Get(), se: m.Settings.Get().SandboxEngine}
	})
	header := ggui.View(hk, func(k headerKey) ggui.Widget {
		if k.view == ImagesRegistry {
			summary := ui.CaptionOf(ggui.Derived(func() string {
				st := m.LocalRegistry.Get()
				l := m.LocalRegistryImages.Get()
				mode := m.Settings.Get().LocalRegistryMode()
				imgCount := st.ImageCount
				if imgCount == 0 && len(l.Items) > 0 {
					imgCount = len(l.Items)
				}
				totalBytes := st.TotalBytes
				if totalBytes == 0 && len(l.Items) > 0 {
					for _, e := range l.Items {
						totalBytes += e.Size
					}
				}
				switch {
				case st.LegacyActive:
					return tr("Served by legacy registry sandbox on %s.", recipes.RegistryLocal())
				case st.Err != "":
					return tr("Could not start on %s: %s", recipes.RegistryLocal(), st.Err)
				case st.Running:
					if st.ReclaimableBytes > 0 {
						return tr("Running on %s · %d images · %s · %s reclaimable", recipes.RegistryLocal(), imgCount, formatBytes(totalBytes), formatBytes(st.ReclaimableBytes))
					}
					return tr("Running on %s · %d images · %s", recipes.RegistryLocal(), imgCount, formatBytes(totalBytes))
				case mode == store.LocalRegistryOff:
					return tr("Disabled · %d images · %s on disk", imgCount, formatBytes(totalBytes))
				default:
					return tr("Stopped · starts automatically with Docker or Kubernetes · %d images · %s", imgCount, formatBytes(totalBytes))
				}
			}))
			return pageHeader(tr("Images"), summary,
				tip(iconButton(lucide.RefreshCw, tr("Refresh local registry"), m.LoadLocalRegistry), tr("Refresh")),
				ui.Button(tr("Prune"), m.AskPruneLocalRegistry).Outline().Icon(glyph(lucide.Eraser, 16)).BindDisabled(ggui.Derived(func() bool {
					return m.BusyOps.Get()["lreg:prune"] || m.LocalRegistry.Get().ReclaimableBytes == 0
				})),
				ui.Button(tr("Copy address"), func() { m.Copy(recipes.RegistryLocal(), recipes.RegistryLocal()) }).Outline().Icon(glyph(lucide.Copy, 16)),
			)
		}
		if k.se {
			summary := ui.CaptionOf(ggui.Derived(func() string {
				open := m.ContainersOpen.Get()
				dl := m.DockerImages.Get()
				gl := m.Images.Get()
				disk := m.Disk.Get()
				useDocker := useDockerForCachedImages(open, dl, gl)
				count := len(gl.Items)
				if useDocker {
					count = len(dl.Items)
				}
				var s strings.Builder
				s.WriteString(fmt.Sprintf(tr("%d cached images"), count))
				hasDisk := false
				for _, r := range disk {
					if r.Type == "Images" {
						s.WriteString(fmt.Sprintf(tr(" · %s on disk · %s reclaimable"), formatSize(r.Size), formatSize(r.Reclaimable)))
						hasDisk = true
						break
					}
				}
				if !hasDisk && useDocker && len(dl.Items) > 0 {
					s.WriteString(" · " + formatBytes(dockerImagesTotalSize(dl.Items)))
				}
				return s.String() + tr(". Shared by GVX sandboxes and Docker; layers are read-only EROFS files.")
			}))
			return pageHeader(tr("Images"), summary,
				tip(iconButton(lucide.RefreshCw, tr("Refresh images"), func() {
					m.LoadImages()
					m.LoadDockerImages()
					m.LoadDisk()
				}), tr("Refresh")),
				ui.Button(tr("Prune"), m.AskPruneCachedImages).Outline().Icon(glyph(lucide.Eraser, 16)).BindDisabled(ggui.Map(m.BusyOps, func(b map[string]bool) bool {
					return b["img:prune"] || b["dimg:prune"]
				})),
				ui.Button(tr("Pull image"), func() {
					if ggui.Peek(m.ContainersOpen) {
						m.OpenDockerPull("")
					} else {
						m.OpenPull("")
					}
				}).Icon(glyph(lucide.Download, 16)),
			)
		}
		if k.view == ImagesDocker {
			summary := ui.CaptionOf(ggui.Derived(func() string {
				open, l := m.ContainersOpen.Get(), m.DockerImages.Get()
				if !open || !l.Loaded || l.Err != "" {
					return tr("Images built or pulled inside the Docker Engine sandbox.")
				}
				return tr("%d images · %s. Images built or pulled inside the Docker Engine sandbox.", len(l.Items), formatBytes(dockerImagesTotalSize(l.Items)))
			}))
			return pageHeader(tr("Images"), summary,
				tip(iconButton(lucide.RefreshCw, tr("Refresh Docker images"), m.LoadDockerImages).BindDisabled(ggui.Not(m.ContainersOpen)), tr("Refresh")),
				ui.Button(tr("Prune"), m.AskPruneDockerImages).Outline().Icon(glyph(lucide.Eraser, 16)).BindDisabled(ggui.Derived(func() bool {
					return !m.ContainersOpen.Get() || m.BusyOps.Get()["dimg:prune"]
				})),
				ui.Button(tr("Pull image"), func() { m.OpenDockerPull("") }).Icon(glyph(lucide.Download, 16)).BindDisabled(ggui.Not(m.ContainersOpen)),
			)
		}
		summary := ui.CaptionOf(ggui.Combine(m.Images, m.Disk, func(l Listing[rungv.Image], d []rungv.DiskUsage) string {
			var s strings.Builder
			s.WriteString(fmt.Sprintf(tr("%d cached images"), len(l.Items)))
			for _, r := range d {
				if r.Type == "Images" {
					s.WriteString(fmt.Sprintf(tr(" · %s on disk · %s reclaimable"), formatSize(r.Size), formatSize(r.Reclaimable)))
				}
			}
			return s.String() + tr(". Layers are compiled to read-only EROFS files, never extracted onto the host.")
		}))
		return pageHeader(tr("Images"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh images"), m.LoadImages), tr("Refresh")),
			ui.Button(tr("Prune"), m.AskPruneImages).Outline().Icon(glyph(lucide.Eraser, 16)).BindDisabled(ggui.Map(m.BusyOps, func(b map[string]bool) bool { return b["img:prune"] })),
			ui.Button(tr("Pull image"), func() { m.OpenPull("") }).Icon(glyph(lucide.Download, 16)),
		)
	})
	tabs := ggui.View(ggui.Map(m.Settings, func(s storeSettings) bool { return s.SandboxEngine }), func(se bool) ggui.Widget {
		if se {
			unifiedTab := ggui.Bind(func() int {
				if m.ImageView.Get() == ImagesRegistry {
					return 1
				}
				return 0
			}, func(i int) {
				if i == 1 {
					m.ImageView.Set(ImagesRegistry)
				} else {
					m.ImageView.Set(ImagesGVX)
				}
			})
			return ui.Tabs(unifiedTab,
				ui.Tab(tr("Cached Images"), widget.Padding(cachedImagesView(m), 12, 0, 0, 0)).
					Header(tabHeader(lucide.Layers, tr("Cached Images"))),
				ui.Tab(tr("Local Registry"), widget.Padding(localRegistryImagesView(m), 12, 0, 0, 0)).
					Header(tabHeader(lucide.Warehouse, tr("Local Registry"))),
			).Name(tr("Image sources"))
		}
		return ui.Tabs(m.ImageView,
			ui.Tab(tr("GVX Sandboxes"), widget.Padding(gvxImagesView(m), 12, 0, 0, 0)).
				Header(tabHeader(lucide.Box, tr("GVX Sandboxes"))),
			ui.Tab("Docker", widget.Padding(dockerImagesView(m), 12, 0, 0, 0)).
				Header(tabHeader(dockerIcon, "Docker")),
			ui.Tab(tr("Local Registry"), widget.Padding(localRegistryImagesView(m), 12, 0, 0, 0)).
				Header(tabHeader(lucide.Warehouse, tr("Local Registry"))),
		).Name(tr("Image sources"))
	})
	return page(header, tabs)
}

type cachedImageRow struct {
	docker   DockerImageRow
	gvx      rungv.Image
	fromGVX  bool
	sizeText string
}

func (r cachedImageRow) Key() string {
	if r.fromGVX {
		return r.gvx.Key()
	}
	return r.docker.Key()
}

func (r cachedImageRow) Ref() string {
	if r.fromGVX {
		return r.gvx.Ref()
	}
	return r.docker.Ref()
}

func useDockerForCachedImages(open bool, dl Listing[dockerapi.Image], gl Listing[rungv.Image]) bool {
	return open && dl.Loaded && dl.Err == "" && (len(dl.Items) > 0 || !gl.Loaded || len(gl.Items) == 0)
}

func gvxToCachedRow(img rungv.Image) cachedImageRow {
	dangling := img.Repository == "" || img.Repository == "<none>" || img.Tag == "" || img.Tag == "<none>"
	shortID := strings.TrimPrefix(img.ID, "sha256:")
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return cachedImageRow{
		docker: DockerImageRow{
			ID:         img.ID,
			ShortID:    shortID,
			Repository: img.Repository,
			Tag:        img.Tag,
			Digest:     img.Digest,
			Dangling:   dangling,
		},
		gvx:      img,
		fromGVX:  true,
		sizeText: formatSize(img.Size),
	}
}

func filterCachedImages(rows []cachedImageRow, q string) []cachedImageRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return rows
	}
	var out []cachedImageRow
	for _, r := range rows {
		d := r.docker
		if strings.Contains(strings.ToLower(d.Repository+":"+d.Tag+" "+d.Arch+" "+d.ShortID+" "+d.ID), q) {
			out = append(out, r)
		}
	}
	return out
}

// cachedImagesView is the unified shared-cache image view shown in the default
// Sandbox Engine mode, combining GVX sandbox actions and Docker/Kubernetes
// actions in one table without requiring the Docker API to browse local cache.
func cachedImagesView(m *Model) ggui.Widget {
	all := ggui.Derived(func() []cachedImageRow {
		open := m.ContainersOpen.Get()
		dl := m.DockerImages.Get()
		gl := m.Images.Get()
		if useDockerForCachedImages(open, dl, gl) {
			drows := dockerImageRows(dl.Items)
			out := make([]cachedImageRow, len(drows))
			for i, d := range drows {
				out[i] = cachedImageRow{docker: d, sizeText: formatBytes(d.Size)}
			}
			return out
		}
		out := make([]cachedImageRow, len(gl.Items))
		for i, g := range gl.Items {
			out[i] = gvxToCachedRow(g)
		}
		return out
	}).WithEqual(nil)
	visible := ggui.Combine(all, m.ImageQuery, filterCachedImages).WithEqual(nil)

	explore := func(row cachedImageRow, tab int) {
		if row.fromGVX {
			m.ExploreGVXImage(row.gvx, tab)
		} else {
			m.ExploreDockerImage(row.docker, tab)
		}
	}
	removeRow := func(row cachedImageRow) {
		if row.fromGVX {
			users := sandboxImageUsers(ggui.Peek(m.Rows), row.docker)
			m.AskRemoveImageWithUsers(row.gvx, users)
		} else {
			m.AskRemoveCachedImage(row.docker)
		}
	}

	table := ui.Table(visible, cachedImageRow.Key,
		ui.Col(tr("Repository"), func(r ggui.Readable[cachedImageRow]) ggui.Widget {
			type repoCell struct {
				repo      string
				users     string
				arch      string
				foreign   bool
				microVM   bool
				vmMatched bool
			}
			return ggui.View(ggui.Derived(func() repoCell {
				row := r.Get()
				ctrs := m.DockerContainers.Get()
				set := m.Settings.Get()
				users := dockerImageAllUsers(ctrs.Items, m.Rows.Get(), true, row.docker)
				microVM := set.SandboxEngine && set.MicroVMContainers
				return repoCell{
					repo:      row.docker.Repository,
					users:     strings.Join(users, ", "),
					arch:      row.docker.Arch,
					foreign:   row.docker.ForeignArch(),
					microVM:   microVM,
					vmMatched: microVM && !row.docker.ForeignArch() && row.docker.Pullable() && engine.MatchMicroVMImage(set.MicroVMImages, row.Ref()),
				}
			}), func(info repoCell) ggui.Widget {
				parts := []ggui.Widget{
					glyph(lucide.Layers, 16).Color(colorGray),
					widget.Flexible(widget.Text(info.repo).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()),
				}
				if info.foreign {
					badgeTip := tr("Built for linux/%s; cannot be run in a sandbox yet.", info.arch)
					if info.microVM && info.arch == "amd64" {
						badgeTip = tr("Built for linux/amd64; runs in a Rosetta micro-VM.")
					}
					parts = append(parts, tip(ui.Badge(info.arch).Outline(), badgeTip))
				}
				if info.vmMatched {
					parts = append(parts, tip(ui.Badge("micro-VM").Outline(), tr("Runs in a Linux micro-VM automatically.")))
				}
				if info.users != "" {
					parts = append(parts, tip(ui.Badge(tr("In use")).Outline(), tr("Used by %s", info.users)))
				}
				return widget.Row(parts...).Gap(8).Align(widget.AlignCenter)
			})
		}).Grow(3),
		ui.Col(tr("Tag"), func(r ggui.Readable[cachedImageRow]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(row cachedImageRow) string { return row.docker.Tag })).Size(12).NoWrap().Ellipsis()
		}).Grow(1),
		ui.Col(tr("Image ID"), func(r ggui.Readable[cachedImageRow]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(row cachedImageRow) string { return row.docker.ShortID })).Size(12)
		}).W(130),
		ui.Col(tr("Created"), func(r ggui.Readable[cachedImageRow]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(row cachedImageRow, now time.Time) string {
				if row.fromGVX {
					return imageAge(row.gvx, now)
				}
				if row.docker.Created == 0 {
					return "—"
				}
				return since(time.Unix(row.docker.Created, 0), now)
			})).Size(12).NoWrap()
		}).W(130),
		ui.TextCol(tr("Size"), func(row cachedImageRow) string { return row.sizeText }).W(96).Right(),
		ui.Col("", func(r ggui.Readable[cachedImageRow]) ggui.Widget {
			type actState struct {
				row            cachedImageRow
				busy           bool
				containersOpen bool
				microVM        bool
				vmExact        bool
			}
			return ggui.View(ggui.Derived(func() actState {
				row := r.Get()
				b := m.BusyOps.Get()
				set := m.Settings.Get()
				busy := b["dimg:"+row.docker.Key()]
				if row.fromGVX {
					busy = b["img:"+row.gvx.Key()]
				}
				normRef := engine.NormalizeMicroVMImagePattern(row.Ref())
				return actState{
					row:            row,
					busy:           busy,
					containersOpen: m.ContainersOpen.Get(),
					microVM:        set.SandboxEngine && set.MicroVMContainers,
					vmExact:        normRef != "" && slices.Contains(set.MicroVMImages, normRef),
				}
			}), func(st actState) ggui.Widget {
				row := st.row
				img := row.docker
				ref := row.Ref()
				canRun := img.Pullable() && !img.ForeignArch()
				entries := []ggui.Widget{
					ui.MenuItem(tr("Browse files"), func() { explore(row, ExploreFiles) }),
					ui.MenuItem(tr("Run…"), func() { m.NewSandbox(ref) }).Disabled(!canRun),
					ui.MenuItem(tr("Inspect"), func() { explore(row, ExploreInspect) }),
					ui.MenuItem(tr("Pull again"), func() {
						if row.fromGVX || !st.containersOpen {
							m.OpenPull(ref)
						} else {
							m.OpenDockerPull(ref)
						}
						m.Pull()
					}).Disabled(!img.Pullable()),
					ui.MenuDivider(),
					ui.MenuItem(tr("Load into cluster"), func() { m.LoadIntoCluster(img) }).Disabled(!canRun || !st.containersOpen),
					ui.MenuItem(tr("Push to local registry"), func() { m.PushToRegistry(img) }).Disabled(!img.Pullable() || !st.containersOpen),
					ui.MenuDivider(),
					ui.MenuItem(tr("Copy reference"), func() { m.Copy(ref, ref) }),
					ui.MenuItem(tr("Copy ID"), func() { m.Copy(img.ID, img.ShortID) }),
				}
				if !img.ForeignArch() {
					entries = append(entries, ui.MenuItem(tr("Copy docker run command"), func() { m.Copy("docker run --rm -it "+ref, "docker run") }))
					if st.microVM && img.EffectiveArch() == "arm64" {
						entries = append(entries, ui.MenuItem(tr("Copy docker run (--runtime=microvm)"), func() { m.Copy("docker run --rm -it --runtime=microvm "+ref, "docker run") }))
						if img.Pullable() {
							if st.vmExact {
								entries = append(entries, ui.MenuItem(tr("Stop running in micro-VM"), func() { m.RemoveMicroVMImage(ref) }))
							} else {
								entries = append(entries, ui.MenuItem(tr("Always run in micro-VM"), func() { m.AddMicroVMImage(ref) }))
							}
						}
					}
				} else if st.microVM && img.Arch == "amd64" {
					entries = append(entries, ui.MenuItem(tr("Copy docker run command"), func() { m.Copy("docker run --rm -it --platform linux/amd64 "+ref, "docker run") }))
				}
				entries = append(entries,
					ui.MenuDivider(),
					ui.MenuItem(tr("Remove…"), func() { removeRow(row) }).Destructive().Disabled(st.busy),
				)
				return widget.Row(
					tip(iconButton(lucide.FolderSearch, tr("Browse files of %s", ref), func() { explore(row, ExploreFiles) }), tr("Browse files")),
					tip(iconButton(lucide.Play, tr("Run %s", ref), func() { m.NewSandbox(ref) }).Disabled(!canRun), tr("Run in a new sandbox")),
					moreMenu(tr("More actions for %s", ref), entries...),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(112).Identified("actions"),
	).BindSelected(m.SelectedImage).RowName(cachedImageRow.Ref).RowHeight(tableRow).OnSelect(func(row cachedImageRow) { explore(row, ExploreFiles) })

	type cachedViewState struct {
		state int // 0: table, 1: empty, 2: error, 3: loading
		err   string
	}
	state := ggui.Derived(func() cachedViewState {
		open := m.ContainersOpen.Get()
		dl := m.DockerImages.Get()
		gl := m.Images.Get()
		if useDockerForCachedImages(open, dl, gl) {
			if len(dl.Items) == 0 {
				return cachedViewState{state: 1}
			}
			return cachedViewState{state: 0}
		}
		switch {
		case gl.Err != "":
			return cachedViewState{state: 2, err: gl.Err}
		case len(gl.Items) > 0:
			return cachedViewState{state: 0}
		case gl.Loaded:
			if open && dl.Err != "" {
				return cachedViewState{state: 2, err: dl.Err}
			}
			return cachedViewState{state: 1}
		case open && dl.Err != "":
			return cachedViewState{state: 2, err: dl.Err}
		default:
			return cachedViewState{state: 3}
		}
	})
	return ggui.View(state, func(st cachedViewState) ggui.Widget {
		switch st.state {
		case 1:
			return ui.Empty(tr("No images cached"), tr("Pull an image, or start a sandbox: a missing image is pulled on first run.")).
				Media(glyph(lucide.Layers, 32).Color(colorGray)).
				Action(ui.Button(tr("Pull an image"), func() {
					if ggui.Peek(m.ContainersOpen) {
						m.OpenDockerPull("")
					} else {
						m.OpenPull("")
					}
				}).Icon(glyph(lucide.Download, 16))).Outline()
		case 2:
			return ui.Alert(tr("Could not list images"), st.err).Destructive().Icon(icons.Alert)
		case 3:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Reading the image cache…"))).Gap(8)
		}
		count := ggui.Combine(visible, all, func(v, a []cachedImageRow) string { return showing(len(v), len(a)) })
		return tableCard(table.Toolbar(filterBar(m.ImageQuery, tr("Filter images"), count)))
	})
}

func tabHeader(icon *icons.Glyph, label string) ggui.Widget {
	return widget.Row(glyph(icon, 14), widget.Text(label).NoWrap()).Gap(6).Align(widget.AlignCenter)
}

func normalizeImageRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if !strings.Contains(ref, "@") {
		lastSlash := strings.LastIndex(ref, "/")
		if !strings.Contains(ref[lastSlash+1:], ":") {
			ref += ":latest"
		}
	}
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "index.docker.io/")
	ref = strings.TrimPrefix(ref, "library/")
	return ref
}

func gvxImagesView(m *Model) ggui.Widget {
	visible := ggui.Combine(m.Images, m.ImageQuery, func(l Listing[rungv.Image], q string) []rungv.Image {
		q = strings.ToLower(strings.TrimSpace(q))
		var out []rungv.Image
		for _, img := range l.Items {
			if q == "" || strings.Contains(strings.ToLower(img.Repository+":"+img.Tag+" "+img.ID), q) {
				out = append(out, img)
			}
		}
		return out
	}).WithEqual(nil)
	used := func(ref string) []string {
		norm := normalizeImageRef(ref)
		var names []string
		for _, r := range ggui.Peek(m.Rows) {
			if normalizeImageRef(r.Spec.Image) == norm {
				names = append(names, r.Spec.Name)
			}
		}
		return names
	}
	table := ui.Table(visible, rungv.Image.Key,
		ui.Col(tr("Repository"), func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return widget.Row(
				glyph(lucide.Layers, 16).Color(colorGray),
				widget.Flexible(widget.TextOf(ggui.Map(r, func(i rungv.Image) string { return i.Repository })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()),
			).Gap(10).Align(widget.AlignCenter)
		}).Grow(3),
		ui.Col(tr("Tag"), func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i rungv.Image) string { return i.Tag })).Size(12).NoWrap().Ellipsis()
		}).Grow(1),
		ui.Col(tr("Image ID"), func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i rungv.Image) string { return i.ID })).Size(12)
		}).W(130),
		ui.Col(tr("Created"), func(r ggui.Readable[rungv.Image]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, imageAge)).Size(12).NoWrap()
		}).W(130),
		ui.TextCol(tr("Size"), func(i rungv.Image) string { return formatSize(i.Size) }).W(96).Right(),
		ui.Col("", func(r ggui.Readable[rungv.Image]) ggui.Widget {
			type actState struct {
				img  rungv.Image
				busy bool
			}
			return ggui.View(ggui.Combine(r, m.BusyOps, func(img rungv.Image, b map[string]bool) actState {
				return actState{img: img, busy: b["img:"+img.Key()]}
			}), func(st actState) ggui.Widget {
				img := st.img
				ref := img.Ref()
				return widget.Row(
					tip(iconButton(lucide.FolderSearch, tr("Browse files of %s", ref), func() { m.ExploreGVXImage(img, ExploreFiles) }), tr("Browse files")),
					tip(iconButton(lucide.Play, tr("Run %s", ref), func() { m.NewSandbox(ref) }), tr("Run in a new sandbox")),
					moreMenu(tr("More actions for %s", ref),
						ui.MenuItem(tr("Browse files"), func() { m.ExploreGVXImage(img, ExploreFiles) }),
						ui.MenuItem(tr("Run…"), func() { m.NewSandbox(ref) }),
						ui.MenuItem(tr("Inspect"), func() { m.ExploreGVXImage(img, ExploreInspect) }),
						ui.MenuItem(tr("Pull again"), func() { m.OpenPull(ref); m.Pull() }),
						ui.MenuItem(tr("Copy reference"), func() { m.Copy(ref, ref) }),
						ui.MenuDivider(),
						ui.MenuItem(tr("Remove…"), func() {
							m.AskRemoveImageWithUsers(img, used(ref))
						}).Destructive().Disabled(st.busy),
					),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(112).Identified("actions"),
	).BindSelected(m.SelectedImage).RowName(rungv.Image.Ref).RowHeight(tableRow).OnSelect(func(img rungv.Image) { m.ExploreGVXImage(img, ExploreFiles) })

	type imgViewState struct {
		state int
		err   string
	}
	return ggui.View(ggui.Map(m.Images, func(l Listing[rungv.Image]) imgViewState {
		switch {
		case l.Err != "":
			return imgViewState{state: 2, err: l.Err}
		case l.Loaded && len(l.Items) == 0:
			return imgViewState{state: 1}
		case !l.Loaded:
			return imgViewState{state: 3}
		}
		return imgViewState{}
	}), func(st imgViewState) ggui.Widget {
		switch st.state {
		case 1:
			return ui.Empty(tr("No images cached"), tr("Pull an image, or start a sandbox: a missing image is pulled on first run.")).
				Media(glyph(lucide.Layers, 32).Color(colorGray)).
				Action(ui.Button(tr("Pull an image"), func() { m.OpenPull("") }).Icon(glyph(lucide.Download, 16))).Outline()
		case 2:
			return ui.Alert(tr("Could not list images"), st.err).Destructive().Icon(icons.Alert)
		case 3:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Reading the image cache…"))).Gap(8)
		}
		count := ggui.Combine(visible, m.Images, func(v []rungv.Image, l Listing[rungv.Image]) string { return showing(len(v), len(l.Items)) })
		return tableCard(table.Toolbar(filterBar(m.ImageQuery, tr("Filter images"), count)))
	})
}

// dockerImagesGate words the Docker Engine tab of Images while the engine does
// not run.
func dockerImagesGate() gateWords {
	return gateWords{
		recipe: recipes.Docker, page: PageDocker, icon: lucide.Layers,
		openPage: tr("Open Docker page"),
		notSetUp: [2]string{tr("The Docker Engine is not set up"),
			tr("Set it up to build and pull images with the docker CLI. They show here while it runs.")},
		starting: [2]string{tr("The Docker Engine is starting"), tr("Its images show here once dockerd is ready.")},
		stopped:  [2]string{tr("The Docker Engine is not running"), tr("Start it to see and manage its images here.")},
		paused:   [2]string{tr("The Docker Engine is paused"), tr("Resume it to see and manage its images here.")},
		start:    tr("Start Docker Engine"),
		resume:   tr("Resume Docker Engine"),
	}
}

func dockerImagesView(m *Model) ggui.Widget {
	listing := m.DockerImages
	all := ggui.Map(listing, func(l Listing[dockerapi.Image]) []DockerImageRow {
		return dockerImageRows(l.Items)
	}).WithEqual(nil)
	visible := ggui.Combine(all, m.DockerImageQuery, filterDockerImages).WithEqual(nil)

	table := ui.Table(visible, DockerImageRow.Key,
		ui.Col(tr("Repository"), func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			type repoCell struct {
				repo      string
				users     string
				arch      string
				foreign   bool
				microVM   bool
				vmMatched bool
			}
			return ggui.View(ggui.Derived(func() repoCell {
				img := r.Get()
				ctrs := m.DockerContainers.Get()
				set := m.Settings.Get()
				users := dockerImageAllUsers(ctrs.Items, m.Rows.Get(), set.SandboxEngine, img)
				microVM := set.SandboxEngine && set.MicroVMContainers
				return repoCell{
					repo:      img.Repository,
					users:     strings.Join(users, ", "),
					arch:      img.Arch,
					foreign:   img.ForeignArch(),
					microVM:   microVM,
					vmMatched: microVM && !img.ForeignArch() && img.Pullable() && engine.MatchMicroVMImage(set.MicroVMImages, img.Ref()),
				}
			}), func(info repoCell) ggui.Widget {
				parts := []ggui.Widget{
					glyph(lucide.Layers, 16).Color(colorGray),
					widget.Flexible(widget.Text(info.repo).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()),
				}
				if info.foreign {
					badgeTip := tr("Built for linux/%s; cannot be run in a sandbox yet.", info.arch)
					if info.microVM && info.arch == "amd64" {
						badgeTip = tr("Built for linux/amd64; runs in a Rosetta micro-VM.")
					}
					parts = append(parts, tip(ui.Badge(info.arch).Outline(), badgeTip))
				}
				if info.vmMatched {
					parts = append(parts, tip(ui.Badge("micro-VM").Outline(), tr("Runs in a Linux micro-VM automatically.")))
				}
				if info.users != "" {
					parts = append(parts, tip(ui.Badge(tr("In use")).Outline(), tr("Used by %s", info.users)))
				}
				return widget.Row(parts...).Gap(8).Align(widget.AlignCenter)
			})
		}).Grow(3),
		ui.Col(tr("Tag"), func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i DockerImageRow) string { return i.Tag })).Size(12).NoWrap().Ellipsis()
		}).Grow(1),
		ui.Col(tr("Image ID"), func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(i DockerImageRow) string { return i.ShortID })).Size(12)
		}).W(130),
		ui.Col(tr("Created"), func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(i DockerImageRow, now time.Time) string {
				if i.Created == 0 {
					return "—"
				}
				return since(time.Unix(i.Created, 0), now)
			})).Size(12).NoWrap()
		}).W(130),
		ui.TextCol(tr("Size"), func(i DockerImageRow) string { return formatBytes(i.Size) }).W(96).Right(),
		ui.Col("", func(r ggui.Readable[DockerImageRow]) ggui.Widget {
			type dimgActState struct {
				img     DockerImageRow
				busy    bool
				microVM bool
				vmExact bool
			}
			return ggui.View(ggui.Derived(func() dimgActState {
				img := r.Get()
				b := m.BusyOps.Get()
				set := m.Settings.Get()
				normRef := engine.NormalizeMicroVMImagePattern(img.Ref())
				return dimgActState{
					img:     img,
					busy:    b["dimg:"+img.Key()],
					microVM: set.SandboxEngine && set.MicroVMContainers,
					vmExact: normRef != "" && slices.Contains(set.MicroVMImages, normRef),
				}
			}), func(st dimgActState) ggui.Widget {
				img := st.img
				ref := img.Ref()
				entries := []ggui.Widget{
					ui.MenuItem(tr("Browse files"), func() { m.ExploreDockerImage(img, ExploreFiles) }),
					ui.MenuItem(tr("Inspect"), func() { m.ExploreDockerImage(img, ExploreInspect) }),
					ui.MenuItem(tr("Pull again"), func() { m.OpenDockerPull(ref); m.Pull() }).Disabled(!img.Pullable()),
					ui.MenuItem(tr("Load into cluster"), func() { m.LoadIntoCluster(img) }).Disabled(!img.Pullable() || img.ForeignArch()),
					ui.MenuItem(tr("Push to local registry"), func() { m.PushToRegistry(img) }).Disabled(!img.Pullable()),
					ui.MenuItem(tr("Copy reference"), func() { m.Copy(ref, ref) }),
					ui.MenuItem(tr("Copy ID"), func() { m.Copy(img.ID, img.ShortID) }),
				}
				if !img.ForeignArch() {
					entries = append(entries, ui.MenuItem(tr("Copy docker run command"), func() { m.Copy("docker run --rm -it "+ref, "docker run") }))
					if st.microVM && img.EffectiveArch() == "arm64" {
						entries = append(entries, ui.MenuItem(tr("Copy docker run (--runtime=microvm)"), func() { m.Copy("docker run --rm -it --runtime=microvm "+ref, "docker run") }))
						if img.Pullable() {
							if st.vmExact {
								entries = append(entries, ui.MenuItem(tr("Stop running in micro-VM"), func() { m.RemoveMicroVMImage(ref) }))
							} else {
								entries = append(entries, ui.MenuItem(tr("Always run in micro-VM"), func() { m.AddMicroVMImage(ref) }))
							}
						}
					}
				} else if st.microVM && img.Arch == "amd64" {
					entries = append(entries, ui.MenuItem(tr("Copy docker run command"), func() { m.Copy("docker run --rm -it --platform linux/amd64 "+ref, "docker run") }))
				}
				entries = append(entries,
					ui.MenuDivider(),
					ui.MenuItem(tr("Remove…"), func() { m.AskRemoveDockerImage(img) }).Destructive().Disabled(st.busy),
				)
				return widget.Row(
					tip(iconButton(lucide.FolderSearch, tr("Browse files of %s", ref), func() { m.ExploreDockerImage(img, ExploreFiles) }), tr("Browse files")),
					tip(iconButton(lucide.Trash, tr("Remove %s", ref), func() { m.AskRemoveDockerImage(img) }).Disabled(st.busy), tr("Remove")),
					moreMenu(tr("More actions for %s", ref), entries...),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(112).Identified("actions"),
	).BindSelected(m.SelectedDockerImage).RowName(DockerImageRow.Ref).RowHeight(tableRow).OnSelect(func(img DockerImageRow) { m.ExploreDockerImage(img, ExploreFiles) })

	type dimgViewState struct {
		state int
		err   string
		se    bool
	}
	state := ggui.Derived(func() dimgViewState {
		a, l, se := m.ContainersAccess.Get(), listing.Get(), m.Settings.Get().SandboxEngine
		switch {
		case a != ContainersOpen:
			return dimgViewState{state: 0, se: se}
		case l.Err != "":
			return dimgViewState{state: 2, err: l.Err, se: se}
		case !l.Loaded:
			return dimgViewState{state: 3, se: se}
		case len(l.Items) == 0:
			return dimgViewState{state: 1, se: se}
		}
		return dimgViewState{state: 4, se: se}
	})
	return ggui.View(state, func(s dimgViewState) ggui.Widget {
		switch s.state {
		case 0:
			stage := ggui.Map(m.ContainersAccess, func(a ContainersAccess) int { return int(a) })
			return ggui.View(gateOf(m, recipes.Docker, stage), func(g engineGate) ggui.Widget { return engineNotRunning(m, g, dockerImagesGate()) })
		case 1:
			return ui.Empty(tr("No Docker images"), tr("Pull an image here, or build one with the docker CLI: docker build -t myapp .")).
				Media(glyph(lucide.Layers, 32).Color(colorGray)).
				Action(ui.Button(tr("Pull an image"), func() { m.OpenDockerPull("") }).Icon(glyph(lucide.Download, 16))).Outline()
		case 2:
			return ui.Alert(tr("Could not list Docker images"), s.err).Destructive().Icon(icons.Alert)
		case 3:
			msg := tr("Asking dockerd…")
			if s.se {
				msg = tr("Loading images…")
			}
			return widget.Row(ui.Spinner(), ui.Caption(msg)).Gap(8)
		}
		count := ggui.Combine(visible, all, func(v, a []DockerImageRow) string { return showing(len(v), len(a)) })
		return tableCard(table.Toolbar(filterBar(m.DockerImageQuery, tr("Filter Docker images"), count)))
	})
}

func localRegistryImagesView(m *Model) ggui.Widget {
	type cardState struct {
		mode         string
		running      bool
		legacyActive bool
		legacyPaused bool
		err          string
	}
	cs := ggui.Derived(func() cardState {
		st := m.LocalRegistry.Get()
		return cardState{
			mode:         m.Settings.Get().LocalRegistryMode(),
			running:      st.Running,
			legacyActive: st.LegacyActive,
			legacyPaused: st.LegacyPaused,
			err:          st.Err,
		}
	})
	modeBind := ggui.Bind(func() string {
		return m.Settings.Get().LocalRegistryMode()
	}, m.SetLocalRegistryMode)

	endpointPill := func(label, addr string) ggui.Widget {
		return tip(
			ui.ButtonOf(widget.Row(
				ui.Caption(label).Size(11).NoWrap(),
				ui.Mono(addr).Size(12).NoWrap(),
				glyph(lucide.Copy, 12).Color(colorGray),
			).Gap(6).Align(widget.AlignCenter), func() {
				m.Copy(addr, addr)
			}).Outline().Size(ui.ButtonExtraSmall).Name(tr("Copy %s", addr)),
			tr("Copy %s", addr),
		)
	}

	statusCard := ggui.View(cs, func(st cardState) ggui.Widget {
		var dotColor color.Color = colorGray
		statusTitle := tr("Stopped (starts automatically)")
		switch {
		case st.legacyActive:
			dotColor = colorAmber
			if st.legacyPaused {
				statusTitle = tr("Legacy registry sandbox (paused)")
			} else {
				statusTitle = tr("Served by legacy registry sandbox")
			}
		case st.err != "":
			dotColor = colorRed
			statusTitle = tr("Could not start local registry")
		case st.running:
			dotColor = colorGreen
			statusTitle = tr("Running on %s", recipes.RegistryLocal())
		case st.mode == store.LocalRegistryOff:
			dotColor = colorGray
			statusTitle = tr("Local registry is off")
		}

		var controls []ggui.Widget
		if st.err != "" {
			controls = append(controls, ui.Button(tr("Retry"), m.RetryLocalRegistry).Outline().Size(ui.ButtonSmall).Icon(glyph(lucide.RefreshCw, 14)))
		}
		if st.legacyActive {
			controls = append(controls, ui.Button(tr("Open sandbox"), func() {
				if r, ok := m.RecipeSandbox(ggui.Peek(m.Rows), recipes.Registry); ok {
					m.ShowSandbox(r.Spec.ID, connectTab)
				}
			}).Outline().Size(ui.ButtonSmall))
		}
		controls = append(controls, ui.ToggleGroup(modeBind).
			Options([]string{store.LocalRegistryAuto, store.LocalRegistryOn, store.LocalRegistryOff}).
			Format(localRegistryShortModeName).
			Outline().Size(ui.ButtonSmall).Spacing(0).
			Name(tr("Local registry mode")))

		rows := []ggui.Widget{
			widget.Row(
				dot(dotColor, 8),
				widget.Expanded(widget.Text(statusTitle).Size(14).Weight(ggui.WeightMedium).NoWrap().Ellipsis()),
				widget.Row(controls...).Gap(8).Align(widget.AlignCenter),
			).Gap(10).Align(widget.AlignCenter),
		}
		if st.err != "" {
			rows = append(rows, widget.Row(
				glyph(lucide.TriangleExclamationPoint, 13).Color(colorRed),
				widget.Expanded(ui.Caption(st.err).Size(12).Color(colorRed)),
			).Gap(8).Align(widget.AlignStart))
		} else if st.legacyActive {
			rows = append(rows, ui.Caption(tr("The built-in registry yields %s while the legacy registry sandbox is active.", recipes.RegistryLocal())).Size(12))
		}
		rows = append(rows, widget.Wrap(
			endpointPill(tr("Host CLI"), recipes.RegistryLocal()),
			endpointPill(tr("Cluster / Docker"), recipes.RegistryHost()),
		).Gap(8))

		return ui.Card(widget.Column(rows...).Gap(10).Stretch())
	})

	visible := ggui.Combine(m.LocalRegistryImages, m.LocalRegistryQuery, func(l Listing[localreg.ImageEntry], q string) []localreg.ImageEntry {
		return filterLocalRegistryImages(l.Items, q)
	}).WithEqual(nil)

	table := ui.Table(visible, localreg.ImageEntry.Key,
		ui.Col(tr("Repository"), func(r ggui.Readable[localreg.ImageEntry]) ggui.Widget {
			return widget.Row(
				glyph(lucide.Layers, 16).Color(colorGray),
				widget.Flexible(widget.TextOf(ggui.Map(r, func(e localreg.ImageEntry) string { return e.Repository })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()),
			).Gap(10).Align(widget.AlignCenter)
		}).Grow(3),
		ui.Col(tr("Tag"), func(r ggui.Readable[localreg.ImageEntry]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(e localreg.ImageEntry) string { return e.Tag })).Size(12).NoWrap().Ellipsis()
		}).Grow(1),
		ui.Col(tr("Digest"), func(r ggui.Readable[localreg.ImageEntry]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(e localreg.ImageEntry) string { return e.ShortDigest() })).Size(12)
		}).W(130),
		ui.Col(tr("Platform"), func(r ggui.Readable[localreg.ImageEntry]) ggui.Widget {
			return ui.CaptionOf(ggui.Map(r, func(e localreg.ImageEntry) string {
				if len(e.Platforms) == 0 {
					return "—"
				}
				return strings.Join(e.Platforms, ", ")
			})).Size(12).NoWrap().Ellipsis()
		}).W(140),
		ui.Col(tr("Updated"), func(r ggui.Readable[localreg.ImageEntry]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(e localreg.ImageEntry, now time.Time) string {
				if e.UpdatedAt.IsZero() {
					return "—"
				}
				return since(e.UpdatedAt, now)
			})).Size(12).NoWrap()
		}).W(120),
		ui.TextCol(tr("Size"), func(e localreg.ImageEntry) string { return formatBytes(e.Size) }).W(96).Right(),
		ui.Col("", func(r ggui.Readable[localreg.ImageEntry]) ggui.Widget {
			type lregActState struct {
				img  localreg.ImageEntry
				busy bool
			}
			return ggui.View(ggui.Combine(r, m.BusyOps, func(img localreg.ImageEntry, b map[string]bool) lregActState {
				return lregActState{img: img, busy: b["lreg:"+img.Key()]}
			}), func(st lregActState) ggui.Widget {
				img := st.img
				key := img.Key()
				clusterRef := img.Ref(recipes.RegistryHost())
				localRef := img.Ref(recipes.RegistryLocal())
				return widget.Row(
					tip(iconButton(lucide.Copy, tr("Copy %s", clusterRef), func() { m.Copy(clusterRef, clusterRef) }), tr("Copy cluster reference")),
					tip(iconButton(lucide.Trash, tr("Delete %s", key), func() { m.AskDeleteLocalRegistryImage(img) }).Disabled(st.busy), tr("Delete")),
					moreMenu(tr("More actions for %s", key),
						ui.MenuItem(tr("Copy cluster reference"), func() { m.Copy(clusterRef, clusterRef) }),
						ui.MenuItem(tr("Copy local reference"), func() { m.Copy(localRef, localRef) }),
						ui.MenuItem(tr("Copy docker pull command"), func() { m.Copy("docker pull "+localRef, "docker pull") }),
						ui.MenuItem(tr("Copy digest"), func() { m.Copy(img.Digest, img.ShortDigest()) }),
						ui.MenuDivider(),
						ui.MenuItem(tr("Delete…"), func() { m.AskDeleteLocalRegistryImage(img) }).Destructive().Disabled(st.busy),
					),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(112).Identified("actions"),
	).BindSelected(m.SelectedLocalRegistryImage).RowName(localreg.ImageEntry.Key).RowHeight(tableRow)

	type lregBodyState struct {
		state int // 0: table, 1: empty, 2: error, 3: loading, 4: off-empty
		err   string
	}
	bodyState := ggui.Derived(func() lregBodyState {
		l := m.LocalRegistryImages.Get()
		reg := m.LocalRegistry.Get()
		mode := m.Settings.Get().LocalRegistryMode()
		switch {
		case l.Err != "":
			return lregBodyState{state: 2, err: l.Err}
		case len(l.Items) > 0:
			return lregBodyState{state: 0}
		case !l.Loaded && l.Loading:
			return lregBodyState{state: 3}
		case mode == store.LocalRegistryOff && !reg.LegacyActive:
			return lregBodyState{state: 4}
		default:
			return lregBodyState{state: 1}
		}
	})
	body := ggui.View(bodyState, func(st lregBodyState) ggui.Widget {
		switch st.state {
		case 1:
			pushCmd := "docker push " + recipes.RegistryLocal() + "/myapp:dev"
			return ui.Empty(
				tr("No images in the local registry"),
				tr("Push from Images → Push to local registry, or run docker push %s/myapp:dev. For one-off loads into k3s without tagging, use Load into cluster.", recipes.RegistryLocal()),
			).Media(glyph(lucide.Warehouse, 32).Color(colorGray)).
				Action(ui.Button(tr("Copy push command"), func() { m.Copy(pushCmd, pushCmd) }).Outline().Icon(glyph(lucide.Copy, 16))).Outline()
		case 2:
			return ui.Alert(tr("Could not list local registry images"), st.err).Destructive().Icon(icons.Alert)
		case 3:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Reading local registry…"))).Gap(8)
		case 4:
			return ui.Empty(
				tr("Local registry is off"),
				tr("Enable it to push images with docker push %s/… and pull them from Kubernetes pods.", recipes.RegistryLocal()),
			).Media(glyph(lucide.Warehouse, 32).Color(colorGray)).
				Action(ui.Button(tr("Enable (Auto)"), func() { m.SetLocalRegistryMode(store.LocalRegistryAuto) })).Outline()
		}
		count := ggui.Combine(visible, m.LocalRegistryImages, func(v []localreg.ImageEntry, l Listing[localreg.ImageEntry]) string {
			return showing(len(v), len(l.Items))
		})
		return tableCard(table.Toolbar(filterBar(m.LocalRegistryQuery, tr("Filter local registry images"), count)))
	})
	return widget.Column(statusCard, body).Gap(12).Stretch()
}

// ---- Volumes ------------------------------------------------------------

func volumesPage(m *Model) ggui.Widget {
	summary := ui.CaptionOf(ggui.Combine(m.Volumes, m.Disk, func(l Listing[rungv.Volume], d []rungv.DiskUsage) string {
		var s strings.Builder
		s.WriteString(fmt.Sprintf(tr("%d volumes"), len(l.Items)))
		for _, r := range d {
			if r.Type == "Local Volumes" {
				s.WriteString(tr(" · %s on disk", formatSize(r.Size)))
			}
		}
		return s.String() + tr(". Each ext4lite volume is one journaled ext4 image file that grows on demand.")
	}))
	all := ggui.Map(m.Volumes, func(l Listing[rungv.Volume]) []rungv.Volume { return l.Items }).WithEqual(nil)
	visible := ggui.Derived(func() []rungv.Volume {
		items := all.Get()
		q := strings.ToLower(strings.TrimSpace(m.VolumeQuery.Get()))
		if q == "" {
			return items
		}
		rows := m.Rows.Get()
		engUsers := m.EngineVolumeUsers.Get()
		var out []rungv.Volume
		for _, v := range items {
			users := strings.Join(volumeUsersWithEngine(rows, engUsers, v.Name), " ")
			if strings.Contains(strings.ToLower(v.Name+" "+v.Driver+" "+users), q) {
				out = append(out, v)
			}
		}
		return out
	}).WithEqual(nil)
	count := ggui.Combine(visible, all, func(v, a []rungv.Volume) string { return showing(len(v), len(a)) })
	type volStatus struct {
		inUse bool
		users string
	}
	table := ui.Table(visible,
		func(v rungv.Volume) string { return v.Name },
		ui.Col(tr("Name"), func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return widget.Row(
				glyph(lucide.HardDrive, 16).Color(colorGray),
				widget.Flexible(widget.TextOf(ggui.Map(r, func(v rungv.Volume) string { return v.Name })).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()),
			).Gap(10).Align(widget.AlignCenter)
		}).Grow(2),
		ui.TextCol(tr("Driver"), func(v rungv.Volume) string { return v.Driver }).W(100),
		ui.Col(tr("Status"), func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			return ggui.View(ggui.Derived(func() volStatus {
				v := r.Get()
				users := volumeUsersWithEngine(m.Rows.Get(), m.EngineVolumeUsers.Get(), v.Name)
				return volStatus{inUse: v.InUse, users: strings.Join(users, ", ")}
			}), func(st volStatus) ggui.Widget {
				switch {
				case st.inUse && st.users != "":
					row := widget.Row(
						dot(colorGreen, 7),
						widget.Text(tr("In use")).Size(12).NoWrap(),
						widget.Flexible(ui.Caption("· "+st.users).Size(12).NoWrap().Ellipsis()),
					).Gap(6).Align(widget.AlignCenter)
					return tip(row, tr("Used by %s", st.users))
				case st.inUse:
					return widget.Row(dot(colorGreen, 7), widget.Text(tr("In use")).Size(12)).Gap(6).Align(widget.AlignCenter)
				case st.users != "":
					return tip(ui.Caption(tr("used by %s", st.users)).Size(12).NoWrap().Ellipsis(), tr("Used by %s", st.users))
				}
				return ui.Caption(tr("unused")).Size(12)
			})
		}).Grow(2),
		ui.TextCol(tr("Size"), func(v rungv.Volume) string { return formatSize(v.Size) }).W(96).Right(),
		ui.TextCol(tr("Created"), func(v rungv.Volume) string { return shortDate(v.CreatedAt) }).W(140),
		ui.Col("", func(r ggui.Readable[rungv.Volume]) ggui.Widget {
			type volActState struct {
				v    rungv.Volume
				busy bool
			}
			return ggui.View(ggui.Combine(r, m.BusyOps, func(v rungv.Volume, b map[string]bool) volActState {
				return volActState{v: v, busy: b["vol:"+v.Name]}
			}), func(st volActState) ggui.Widget {
				v := st.v
				menuItems := []ggui.Widget{
					ui.MenuItem(tr("Reveal in file manager"), func() { m.Open(v.Mountpoint) }).Disabled(v.Mountpoint == ""),
					ui.MenuItem(tr("Copy name"), func() { m.Copy(v.Name, v.Name) }),
				}
				if v.Mountpoint != "" {
					menuItems = append(menuItems, ui.MenuItem(tr("Copy path"), func() { m.Copy(v.Mountpoint, v.Mountpoint) }))
				}
				menuItems = append(menuItems,
					ui.MenuDivider(),
					ui.MenuItem(tr("Delete…"), func() { m.AskRemoveVolume(v) }).Destructive().Disabled(v.InUse || st.busy),
				)
				delTip := tr("Delete")
				if v.InUse {
					delTip = tr("In use — stop the sandbox or container first.")
				}
				return widget.Row(
					tip(iconButton(lucide.FolderOpen, tr("Reveal %s", v.Name), func() { m.Open(v.Mountpoint) }).Disabled(v.Mountpoint == ""), tr("Reveal in file manager")),
					tip(iconButton(lucide.Trash, tr("Delete %s", v.Name), func() { m.AskRemoveVolume(v) }).Disabled(v.InUse || st.busy), delTip),
					moreMenu(tr("More actions for %s", v.Name), menuItems...),
				).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(112).Identified("actions"),
	).BindSelected(m.SelectedVolume).RowName(func(v rungv.Volume) string { return v.Name }).RowHeight(tableRow).
		Toolbar(filterBar(m.VolumeQuery, tr("Filter by name, driver or sandbox"), count))

	type volViewState struct {
		state int
		err   string
	}
	body := ggui.View(ggui.Map(m.Volumes, func(l Listing[rungv.Volume]) volViewState {
		switch {
		case l.Err != "":
			return volViewState{state: 2, err: l.Err}
		case l.Loaded && len(l.Items) == 0:
			return volViewState{state: 1}
		case !l.Loaded:
			return volViewState{state: 3}
		}
		return volViewState{}
	}), func(st volViewState) ggui.Widget {
		switch st.state {
		case 1:
			return ui.Empty(tr("No volumes"), tr("A sandbox creates a named volume the first time it mounts one, or create one here.")).
				Media(glyph(lucide.HardDrive, 32).Color(colorGray)).
				Action(ui.Button(tr("Create volume"), m.OpenVolumeDialog).Icon(glyph(lucide.Plus, 16))).Outline()
		case 2:
			return ui.Alert(tr("Could not list volumes"), st.err).Destructive().Icon(icons.Alert)
		case 3:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Reading volumes…"))).Gap(8)
		}
		return widget.Column(
			tableCard(table),
			ui.Caption(tr("A volume can be mounted by one sandbox at a time: its image file is locked while in use. Linux can mount it directly with mount -o loop.")),
		).Gap(12).Stretch()
	})
	return page(
		pageHeader(tr("Volumes"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh volumes"), m.LoadVolumes), tr("Refresh")),
			ui.Button(tr("Prune"), m.AskPruneVolumes).Outline().Icon(glyph(lucide.Eraser, 16)).BindDisabled(ggui.Map(m.BusyOps, func(b map[string]bool) bool { return b["vol:prune"] })),
			ui.Button(tr("Create volume"), m.OpenVolumeDialog).Icon(glyph(lucide.Plus, 16)),
		),
		body,
	)
}

func shortDate(s string) string {
	// rungv writes RFC 3339 or "2006-01-02 15:04:05 -0700 MST".
	if len(s) >= 16 {
		return strings.Replace(s[:16], "T", " ", 1)
	}
	return s
}

// ---- Snapshots ----------------------------------------------------------

func snapshotsPage(m *Model) ggui.Widget {
	summary := ui.CaptionOf(ggui.Map(m.Snapshots, func(s []sandbox.Snapshot) string {
		var total int64
		for _, x := range s {
			total += x.Size
		}
		return fmt.Sprintf(tr("%d snapshots · %s. Checkpoint a running sandbox and resume it later, in milliseconds."), len(s), formatBytes(total))
	}))
	enabled := ggui.Map(m.Specs, func(specs []sandbox.Spec) []sandbox.Spec {
		var out []sandbox.Spec
		for _, s := range specs {
			if s.Checkpoint {
				out = append(out, s)
			}
		}
		return out
	}).WithEqual(nil)
	hint := ggui.View(enabled, func(specs []sandbox.Spec) ggui.Widget {
		if len(specs) > 0 {
			names := make([]string, len(specs))
			for i, s := range specs {
				names[i] = s.Name
			}
			return ui.Caption(tr("Snapshots are on for: %s. Take one from the sandbox's Snapshots tab.", strings.Join(names, ", ")))
		}
		return ui.Alert(tr("No sandbox has snapshots turned on"),
			tr("Turn on \"Allow snapshots\" in a sandbox's settings: gvx opens the bundle directory and a control socket before the guest starts.")).Icon(icons.Info)
	})
	visible := ggui.Derived(func() []sandbox.Snapshot {
		all := m.Snapshots.Get()
		specs := m.Specs.Get()
		q := strings.ToLower(strings.TrimSpace(m.SnapshotQuery.Get()))
		if q == "" {
			return all
		}
		names := make(map[string]string, len(specs))
		for _, sp := range specs {
			names[sp.ID] = sp.Name
		}
		var out []sandbox.Snapshot
		for _, s := range all {
			sbName := names[s.SandboxID]
			if strings.Contains(strings.ToLower(sbName+" "+s.Name+" "+s.Dir), q) {
				out = append(out, s)
			}
		}
		return out
	}).WithEqual(nil)
	count := ggui.Combine(visible, m.Snapshots, func(v, all []sandbox.Snapshot) string { return showing(len(v), len(all)) })
	emptyText := ggui.Map(m.SnapshotQuery, func(q string) string {
		if strings.TrimSpace(q) != "" {
			return tr("No matching snapshots.")
		}
		return tr("No snapshots yet.")
	})
	snapState := ggui.Combine(m.SnapshotsLoaded, m.Snapshots, func(loaded bool, s []sandbox.Snapshot) int {
		if !loaded && len(s) == 0 {
			return 0
		}
		if len(s) == 0 {
			return 1
		}
		return 2
	})
	body := ggui.View(snapState, func(st int) ggui.Widget {
		switch st {
		case 0:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Reading snapshots…"))).Gap(8)
		case 1:
			return ui.Empty(tr("No snapshots yet"), tr("Checkpoint a running sandbox from its Snapshots tab to resume it later in milliseconds.")).
				Media(glyph(lucide.Camera, 32).Color(colorGray)).
				Action(ui.Button(tr("Open Sandboxes"), func() { m.Go(PageSandboxes) }).Outline()).Outline()
		}
		return widget.Column(
			ui.Card(filterBar(m.SnapshotQuery, tr("Filter snapshots"), count)).Pad(6, 10),
			snapshotList(m, visible, true, emptyText),
		).Gap(12).Stretch()
	})
	return page(
		pageHeader(tr("Snapshots"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh snapshots"), m.LoadSnapshots), tr("Refresh")),
		),
		hint,
		body,
	)
}

// imageAge says how long ago an image was made, in the app's language;
// rungv's own words for it are English.
func imageAge(i rungv.Image, now time.Time) string {
	t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", i.CreatedAt)
	if err != nil {
		return i.CreatedSince
	}
	return since(t, now)
}
