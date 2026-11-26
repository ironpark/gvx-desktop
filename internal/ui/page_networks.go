package ui

import (
	"strconv"
	"strings"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/icons/lucide"
	"github.com/ironpark/ggui/ui"
	"github.com/ironpark/ggui/ui/icons"
	"github.com/ironpark/ggui/widget"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/recipes"
)

// networksPage lists the engine's networks as the Containers page lists
// containers: a table to filter, and the chosen network under it with the
// containers on it.
func networksPage(m *Model) ggui.Widget {
	listing := m.DockerNetworks
	summary := ui.CaptionOf(ggui.Map(listing, func(l Listing[dockerapi.Network]) string {
		if !l.Loaded || l.Err != "" {
			return tr("The networks containers join, and the containers on each.")
		}
		user := 0
		for _, n := range l.Items {
			if !n.Builtin() {
				user++
			}
		}
		return trn(user, "%d network of your own, and the built-in ones.", "%d networks of your own, and the built-in ones.")
	}))

	type netViewState struct {
		state int
		err   string
	}
	state := ggui.Combine(m.ContainersAccess, listing, func(a ContainersAccess, l Listing[dockerapi.Network]) netViewState {
		switch {
		case a != ContainersOpen:
			return netViewState{state: 0}
		case l.Err != "":
			return netViewState{state: containersFailed, err: l.Err}
		case !l.Loaded:
			return netViewState{state: containersLoading}
		}
		return netViewState{state: containersListed}
	})
	body := ggui.View(state, func(s netViewState) ggui.Widget {
		switch s.state {
		case 0:
			stage := ggui.Map(m.ContainersAccess, func(a ContainersAccess) int { return int(a) })
			return ggui.View(gateOf(m, recipes.Docker, stage), func(g engineGate) ggui.Widget { return engineNotRunning(m, g, networksGate()) })
		case containersFailed:
			return ui.Alert(tr("Could not list networks"), s.err).Destructive().Icon(icons.Alert)
		case containersLoading:
			return widget.Row(ui.Spinner(), ui.Caption(tr("Loading networks…"))).Gap(8)
		}
		return networksSection(m)
	})

	return page(
		pageHeader(tr("Networks"), summary,
			tip(iconButton(lucide.RefreshCw, tr("Refresh networks"), func() {
				m.LoadDockerNetworks()
				m.LoadDocker()
			}).BindDisabled(ggui.Not(m.ContainersOpen)), tr("Refresh")),
			ui.Button(tr("Remove unused"), m.AskPruneNetworks).Outline().Icon(glyph(lucide.Eraser, 16)).BindDisabled(ggui.Derived(func() bool {
				return !m.ContainersOpen.Get() || m.BusyOps.Get()["net:prune"]
			})),
		),
		body,
	)
}

// networksGate words the Networks page while the engine does not run.
func networksGate() gateWords {
	g := dockerGate()
	g.icon = lucide.Network
	g.notSetUp[1] = tr("Set it up to run containers with the docker CLI on this machine. Their networks show here while it runs.")
	g.starting[1] = tr("Its networks show here once dockerd is ready.")
	g.stopped[1] = tr("Start it to see its networks here.")
	g.paused[1] = tr("Resume it to see its networks here.")
	return g
}

// networksSection pairs the networks table, its filtered-empty state, and
// the detail card below.
func networksSection(m *Model) ggui.Widget {
	all := ggui.Map(m.DockerNetworks, func(l Listing[dockerapi.Network]) []dockerapi.Network { return sortNetworks(l.Items) }).WithEqual(nil)
	rows := ggui.Combine(all, m.NetworkQuery, filterNetworks).WithEqual(nil)
	count := ggui.Combine(rows, all, func(v, a []dockerapi.Network) string { return showing(len(v), len(a)) })
	hasVisible := ggui.Map(rows, func(v []dockerapi.Network) bool { return len(v) > 0 })
	selected := ggui.Bind(ggui.Map(m.Network, func(n dockerapi.Network) string { return n.ID }).Get, m.SelectNetwork)
	netKey := func(n dockerapi.Network) string { return n.ID }
	netLabel := func(n dockerapi.Network) string { return n.Name }
	netCanSelect := func(n dockerapi.Network) (bool, string) {
		if why := networkRemovable(n); why != "" {
			return false, why
		}
		return true, ""
	}
	checked := newBulkSelection()
	checkedItems := ggui.Combine(rows, checked, func(items []dockerapi.Network, sel map[string]bool) []dockerapi.Network {
		return selectedItems(items, sel, netKey, netCanSelect)
	}).WithEqual(nil)
	checkedCount := ggui.Map(checkedItems, func(items []dockerapi.Network) int { return len(items) })
	clearChecked := func() { clearSelection(checked) }
	bulkRemove := ui.Button(tr("Remove…"), func() {
		m.AskRemoveNetworks(ggui.Peek(checkedItems), clearChecked)
	}).Destructive().Size(ui.ButtonExtraSmall).Icon(glyph(lucide.Trash, 13)).BindDisabled(ggui.Map(m.BusyOps, func(b map[string]bool) bool {
		return b["net:bulk"]
	}))
	netToolbar := bulkToolbar(filterBar(m.NetworkQuery, tr("Filter by name, subnet or project"), count), checkedCount, clearChecked, bulkRemove)
	table := ui.Table(rows, netKey,
		selectCheckboxColumn(rows, checked, netKey, netLabel, netCanSelect),
		ui.Col(tr("Name"), func(r ggui.Readable[dockerapi.Network]) ggui.Widget {
			return ggui.View(r, func(n dockerapi.Network) ggui.Widget {
				caption := n.ShortID()
				switch {
				case n.Builtin():
					caption = tr("Built in")
				case n.Project() != "":
					caption = "compose: " + n.Project()
				}
				return widget.Row(
					glyph(lucide.Network, 16).Color(colorGray),
					widget.Flexible(widget.Column(
						widget.Text(n.Name).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis(),
						ui.Caption(caption).Size(11).NoWrap().Ellipsis(),
					).Gap(1)),
				).Gap(10).Align(widget.AlignCenter)
			})
		}).Grow(3),
		ui.Col(tr("Driver"), func(r ggui.Readable[dockerapi.Network]) ggui.Widget {
			return ggui.View(r, func(n dockerapi.Network) ggui.Widget {
				parts := []ggui.Widget{ui.Caption(n.Driver).Size(12).NoWrap()}
				if n.Internal {
					parts = append(parts, tip(ui.Badge(tr("Internal")).Outline(), tr("Containers on it have no way out to the internet.")))
				}
				return widget.Row(parts...).Gap(6).Align(widget.AlignCenter)
			})
		}).W(150),
		ui.Col(tr("Subnet"), func(r ggui.Readable[dockerapi.Network]) ggui.Widget {
			return ui.MonoOf(ggui.Map(r, func(n dockerapi.Network) string {
				if sub, _ := n.Subnet(); sub != "" {
					return sub
				}
				return "—"
			})).Size(12).NoWrap()
		}).Grow(2),
		ui.TextCol(tr("Containers"), func(n dockerapi.Network) string {
			if len(n.Containers) == 0 {
				return "—"
			}
			return strconv.Itoa(len(n.Containers))
		}).W(100).Right(),
		ui.Col(tr("Created"), func(r ggui.Readable[dockerapi.Network]) ggui.Widget {
			return ui.CaptionOf(ggui.Combine(r, m.Now, func(n dockerapi.Network, now time.Time) string {
				if n.Builtin() || n.CreatedAt().IsZero() {
					return "—"
				}
				return since(n.CreatedAt(), now)
			})).Size(12).NoWrap()
		}).W(130),
		ui.Col("", func(r ggui.Readable[dockerapi.Network]) ggui.Widget {
			type netActState struct {
				n    dockerapi.Network
				busy bool
			}
			return ggui.View(ggui.Combine(r, m.BusyOps, func(n dockerapi.Network, b map[string]bool) netActState {
				return netActState{n: n, busy: b["net:"+n.ID]}
			}), func(st netActState) ggui.Widget {
				return widget.Row(networkRemoveButton(m, st.n, st.busy), networkMenu(m, st.n, st.busy)).Gap(2).Justify(widget.JustifyEnd)
			})
		}).W(84).Identified("actions"),
	).BindSelected(selected).RowName(func(n dockerapi.Network) string { return n.Name }).RowHeight(48).MaxRows(6).
		Toolbar(netToolbar)

	filteredEmpty := ggui.If(ggui.Not(hasVisible), func() ggui.Widget {
		return widget.Padding(widget.Row(
			ui.Caption(tr("No networks match the current filter.")),
			ui.Button(tr("Clear filter"), func() { m.NetworkQuery.Set("") }).Outline().Size(ui.ButtonExtraSmall),
		).Gap(10).Align(widget.AlignCenter).Justify(widget.JustifyCenter), 16, 12)
	})

	return widget.Column(
		tableCard(widget.Column(table, filteredEmpty).Stretch()),
		ggui.If(hasVisible, func() ggui.Widget { return networkDetail(m) }),
	).Gap(16).Stretch()
}

// networkRemovable says why n cannot be removed, or "" when it can.
func networkRemovable(n dockerapi.Network) string {
	switch {
	case n.Builtin():
		return tr("A built-in network cannot be removed.")
	case len(n.Containers) > 0:
		return tr("Containers are on it. Remove them or take them off it first.")
	}
	return ""
}

func networkRemoveButton(m *Model, n dockerapi.Network, busy bool) ggui.Widget {
	why := networkRemovable(n)
	b := iconButton(lucide.Trash, tr("Remove %s", n.Name), func() { m.AskRemoveNetwork(n) }).Disabled(why != "" || busy)
	if why != "" {
		return tip(b, why)
	}
	return tip(b, tr("Remove"))
}

// networkMenu is what can be done with a network.
func networkMenu(m *Model, n dockerapi.Network, busy bool) ggui.Widget {
	return moreMenu(tr("More actions for %s", n.Name),
		ui.MenuItem(tr("Copy name"), func() { m.Copy(n.Name, n.Name) }),
		ui.MenuItem(tr("Copy ID"), func() { m.Copy(n.ID, n.ShortID()) }),
		ui.MenuItem(tr("Copy docker run command"), func() {
			m.Copy("docker run --rm -it --network "+n.Name+" alpine sh", "docker run")
		}).Disabled(n.Name == "none"),
		ui.MenuItem(tr("Copy docker network inspect command"), func() {
			m.Copy("docker network inspect "+n.Name, "docker network inspect")
		}),
		ui.MenuDivider(),
		ui.MenuItem(tr("Remove…"), func() { m.AskRemoveNetwork(n) }).Destructive().Disabled(networkRemovable(n) != "" || busy),
	)
}

// networkDetail is the chosen network: what it is, how its containers
// reach each other, and who is on it.
func networkDetail(m *Model) ggui.Widget {
	id := ggui.Map(m.Network, func(n dockerapi.Network) string { return n.ID })
	return entering(id, func(string) ggui.Widget {
		type view struct {
			n       dockerapi.Network
			members []NetworkMember
			se      bool
			busy    bool
		}
		v := ggui.Derived(func() view {
			n := m.Network.Get()
			return view{n, networkMembers(n, m.DockerContainers.Get().Items), m.Settings.Get().SandboxEngine, m.BusyOps.Get()["net:"+n.ID]}
		}).WithEqual(func(a, b view) bool {
			return a.n.Equal(b.n) && a.se == b.se && a.busy == b.busy && slicesEqualMembers(a.members, b.members)
		})
		return ggui.View(v, func(v view) ggui.Widget {
			if v.n.ID == "" {
				return widget.Box()
			}
			return ui.Card(networkDetailBody(m, v.n, v.members, v.se, v.busy))
		})
	})
}

func slicesEqualMembers(a, b []NetworkMember) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Name != b[i].Name || a[i].Address != b[i].Address || a[i].State != b[i].State ||
			strings.Join(a[i].Names, ",") != strings.Join(b[i].Names, ",") {
			return false
		}
	}
	return true
}

func networkDetailBody(m *Model, n dockerapi.Network, members []NetworkMember, se, busy bool) ggui.Widget {
	meta := []string{n.Driver}
	if sub, gw := n.Subnet(); sub != "" {
		meta = append(meta, sub)
		if gw != "" {
			meta = append(meta, tr("gateway %s", gw))
		}
	}
	if n.Internal {
		meta = append(meta, tr("internal"))
	}
	if p := n.Project(); p != "" {
		meta = append(meta, "compose: "+p)
	}
	state := stateLabel(colorGray, tr("No containers"), false, "")
	if len(members) > 0 {
		running := 0
		for _, mem := range members {
			if mem.State == "running" {
				running++
			}
		}
		col := colorGray
		if running > 0 {
			col = colorGreen
		}
		state = stateLabel(col, trn(len(members), "%d container", "%d containers"), false, tr("%d running", running))
	}
	var actions []ggui.Widget
	if why := networkRemovable(n); why == "" {
		actions = append(actions, ui.Button(tr("Remove"), func() { m.AskRemoveNetwork(n) }).Outline().Icon(glyph(lucide.Trash, 14)).Disabled(busy))
	}
	actions = append(actions, networkMenu(m, n, busy))
	head := detailHead(n.Name, state, ui.Caption(strings.Join(meta, " · ")).Size(12), actions...)

	parts := []ggui.Widget{head, ui.Caption(networkReach(n, se)).Size(12)}
	parts = append(parts, sectionTitle(tr("Containers"), ""))
	if len(members) == 0 {
		parts = append(parts, ui.Caption(tr("No container is on this network. Put one on it with docker run --network %s.", n.Name)).Size(12))
	}
	for _, mem := range members {
		parts = append(parts, networkMemberRow(m, n, mem, se))
	}
	parts = append(parts, sectionTitle(tr("Commands"), tr("For a terminal where docker uses the gvx context.")))
	if n.Name != "none" {
		parts = append(parts, copyable(m, "docker run --rm -it --network "+n.Name+" alpine sh", "docker run"))
	}
	parts = append(parts, copyable(m, "docker network inspect "+n.Name, "docker network inspect"))
	return widget.Column(parts...).Gap(10).Stretch()
}

// networkReach says how the containers on n reach each other.
func networkReach(n dockerapi.Network, se bool) string {
	switch {
	case n.Name == "host" && se:
		return tr("Host networking is not available in the sandbox engine: containers asking for it get a network of their own.")
	case n.Name == "host":
		return tr("Containers on it share the network of the Docker Engine's sandbox.")
	case n.Name == "none" || n.Driver == "null":
		return tr("Containers on it have no network at all.")
	case n.Builtin() && se:
		return tr("Containers only on the default bridge each have a network of their own and cannot reach each other. Put them on a network of their own to have them reach each other by name.")
	case n.Builtin():
		return tr("Containers on the default bridge reach each other by address, not by name.")
	case n.Internal:
		return tr("Containers on it reach each other by name (container name, Compose service and aliases), and have no way out to the internet.")
	}
	return tr("Containers on it reach each other by name: their container name, Compose service and aliases.")
}

// networkMemberRow is a container on a network: its state, name, address
// and the names others reach it by.
func networkMemberRow(m *Model, n dockerapi.Network, mem NetworkMember, se bool) ggui.Widget {
	col := colorGray
	switch mem.State {
	case "running":
		col = colorGreen
	case "paused", "restarting":
		col = colorAmber
	case "dead":
		col = colorRed
	}
	addr := mem.Address
	if se && n.Builtin() {
		addr = "" // the sandbox's own NAT, the same in every container
	}
	displayName := mem.Name
	if displayName == "" && mem.ID != "" {
		displayName = mem.ID[:min(12, len(mem.ID))]
	}
	var names []string
	for _, nm := range mem.Names {
		if nm != displayName && !(len(nm) == 12 && strings.HasPrefix(mem.ID, nm)) {
			names = append(names, nm)
		}
	}
	label := []ggui.Widget{widget.Text(displayName).Size(13).Weight(ggui.WeightMedium).NoWrap().Ellipsis()}
	if len(names) > 0 {
		label = append(label, ui.Caption(tr("also %s", strings.Join(names, ", "))).Size(11).NoWrap().Ellipsis())
	}
	row := []ggui.Widget{
		widget.Box(dot(col, 8)).Pad(2),
		widget.Expanded(widget.Column(label...).Gap(1)),
	}
	if addr != "" {
		row = append(row,
			ui.Mono(addr).Size(12).NoWrap(),
			tip(iconButton(lucide.Copy, tr("Copy address"), func() { m.Copy(addr, addr) }), tr("Copy address")),
		)
	}
	if mem.State != "" {
		id := mem.ID
		row = append(row, tip(iconButton(lucide.ArrowRight, tr("Show %s", displayName), func() {
			m.ShowContainer(id, 1)
		}), tr("Show container")))
	}
	return widget.Row(row...).Gap(10).Align(widget.AlignCenter)
}
