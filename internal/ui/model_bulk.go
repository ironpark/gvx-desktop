package ui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/ggui"
	"github.com/ironpark/ggui/ui"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
	"github.com/ironpark/gvx-desktop/internal/localreg"
	"github.com/ironpark/gvx-desktop/internal/recipes"
	"github.com/ironpark/gvx-desktop/internal/rungv"
	"github.com/ironpark/gvx-desktop/internal/sandbox"
)

// ---- Bulk Image Removal -------------------------------------------------

// AskRemoveCachedImages confirms and removes multiple selected cached images
// in unified Sandbox Engine mode.
func (m *Model) AskRemoveCachedImages(rows []cachedImageRow, onDone func()) {
	const busyKey = "dimg:bulk"
	if len(rows) == 0 || m.isBusy(busyKey) {
		return
	}

	ctrs := ggui.Peek(m.DockerContainers).Items
	sbRows := ggui.Peek(m.Rows)
	var removable []cachedImageRow
	skippedRunning := 0
	anySandboxUser := false
	anyCtrUser := false
	for _, row := range rows {
		if !row.fromGVX {
			sbs := sandboxImageUsers(sbRows, row.docker)
			if len(sbs) > 0 {
				if running := dockerImageRunningUsers(ctrs, row.docker); len(running) > 0 {
					skippedRunning++
					continue
				}
				anySandboxUser = true
			}
			if len(dockerImageUsers(ctrs, row.docker)) > 0 {
				anyCtrUser = true
			}
		} else {
			if len(sandboxImageUsers(sbRows, row.docker)) > 0 {
				anySandboxUser = true
			}
		}
		removable = append(removable, row)
	}
	if len(removable) == 0 {
		m.Toasts.Push(ui.Toast(
			tr("Could not remove some images"),
			tr("All selected images are currently used by running containers. Stop those containers first."),
		).Warning().Duration(8 * time.Second))
		return
	}

	var notes []string
	notes = append(notes, trn(len(removable),
		"%d selected image is removed from the cache.",
		"%d selected images are removed from the cache.",
	))
	if skippedRunning > 0 {
		notes = append(notes, tr("Some selected images are in use by running containers and will be skipped."))
	}
	if anySandboxUser {
		notes = append(notes, tr("Some selected images are referenced by sandboxes and will be pulled again on their next start."))
	}
	if anyCtrUser {
		notes = append(notes, tr("Some selected images are used by containers. Force removing untags or deletes them."))
	}

	m.Confirm(
		trn(len(removable), "Remove %d image?", "Remove %d images?"),
		strings.Join(notes, " "),
		tr("Remove images"),
		func() {
			if m.isBusy(busyKey) {
				return
			}
			m.setBusy(busyKey, true)
			host := m.dockerBackendHost()
			rungvClient := m.Client()
			sbSnap := slices.Clone(ggui.Peek(m.Rows))
			removed := 0
			m.async(func(ctx context.Context) error {
				var dc *dockerapi.Client
				defer func() {
					if dc != nil {
						dc.Close()
					}
				}()
				var errs []error
				for _, row := range removable {
					useRungv := row.fromGVX || len(sandboxImageUsers(sbSnap, row.docker)) > 0
					var err error
					if useRungv {
						target := row.docker.Target()
						if row.fromGVX {
							target = row.gvx.Ref()
						}
						err = rungvClient.RemoveImage(ctx, target, true)
					} else {
						if dc == nil {
							dc, err = dockerapi.New(host)
						}
						if err == nil {
							rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
							err = dc.RemoveImagePlatform(rctx, row.docker.Target(), true, row.docker.Platform())
							cancel()
						}
					}
					if err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", row.Ref(), err))
						continue
					}
					removed++
				}
				return errors.Join(errs...)
			}, func(err error) {
				m.setBusy(busyKey, false)
				if onDone != nil {
					onDone()
				}
				if err != nil {
					m.toastError(tr("Could not remove some images"), err)
				} else if removed > 0 {
					m.toastOK(trn(removed, "Removed %d image", "Removed %d images"), "")
				}
				m.LoadImages()
				m.LoadDockerImages()
				m.LoadDocker()
				m.LoadDisk()
			})
		},
	)
}

// AskRemoveGVXImages confirms and removes multiple selected GVX cached images.
func (m *Model) AskRemoveGVXImages(imgs []rungv.Image, onDone func()) {
	const busyKey = "img:bulk"
	if len(imgs) == 0 || m.isBusy(busyKey) {
		return
	}

	anyUsers := false
	sbRows := ggui.Peek(m.Rows)
	for _, img := range imgs {
		norm := normalizeImageRef(img.Ref())
		for _, r := range sbRows {
			if normalizeImageRef(r.Spec.Image) == norm {
				anyUsers = true
				break
			}
		}
	}
	desc := trn(len(imgs),
		"%d selected image is removed from the cache.",
		"%d selected images are removed from the cache.",
	)
	if anyUsers {
		desc += " " + tr("Some selected images are referenced by sandboxes and will be pulled again on their next start.")
	}
	m.Confirm(
		trn(len(imgs), "Remove %d image?", "Remove %d images?"),
		desc,
		tr("Remove images"),
		func() {
			if m.isBusy(busyKey) {
				return
			}
			m.setBusy(busyKey, true)
			c := m.Client()
			removed := 0
			m.async(func(ctx context.Context) error {
				var errs []error
				for _, img := range imgs {
					ref := img.Ref()
					if err := c.RemoveImage(ctx, ref, true); err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", ref, err))
						continue
					}
					removed++
				}
				return errors.Join(errs...)
			}, func(err error) {
				m.setBusy(busyKey, false)
				if onDone != nil {
					onDone()
				}
				if err != nil {
					m.toastError(tr("Could not remove some images"), err)
				} else if removed > 0 {
					m.toastOK(trn(removed, "Removed %d image", "Removed %d images"), "")
				}
				m.LoadImages()
				m.LoadDisk()
				if ggui.Peek(m.Settings).SandboxEngine {
					m.LoadDockerImages()
				}
			})
		},
	)
}

// AskRemoveDockerImages confirms and removes multiple selected Docker images.
func (m *Model) AskRemoveDockerImages(rows []DockerImageRow, onDone func()) {
	const busyKey = "dimg:bulk"
	if len(rows) == 0 || m.isBusy(busyKey) {
		return
	}

	se := ggui.Peek(m.Settings).SandboxEngine
	sbRows := ggui.Peek(m.Rows)
	ctrs := ggui.Peek(m.DockerContainers).Items
	var removable []DockerImageRow
	skippedSandbox := 0
	anyCtrUser := false
	var totalBytes int64
	for _, img := range rows {
		if se && len(sandboxImageUsers(sbRows, img)) > 0 {
			skippedSandbox++
			continue
		}
		if len(dockerImageUsers(ctrs, img)) > 0 {
			anyCtrUser = true
		}
		totalBytes += img.Size
		removable = append(removable, img)
	}
	if len(removable) == 0 {
		m.Toasts.Push(ui.Toast(
			tr("Could not remove some images"),
			tr("Some selected images are used by sandboxes and cannot be removed from the Docker tab."),
		).Warning().Duration(8 * time.Second))
		return
	}

	var notes []string
	notes = append(notes, trn(len(removable),
		"%d selected image is removed from the Docker Engine (%s).",
		"%d selected images are removed from the Docker Engine (%s).",
		formatBytes(totalBytes),
	))
	if skippedSandbox > 0 {
		notes = append(notes, tr("Some selected images are used by sandboxes and cannot be removed from the Docker tab."))
	}
	if anyCtrUser {
		notes = append(notes, tr("Some selected images are used by containers. Force removing untags or deletes them."))
	}

	m.Confirm(
		trn(len(removable), "Remove %d image?", "Remove %d images?"),
		strings.Join(notes, " "),
		tr("Remove images"),
		func() {
			if m.isBusy(busyKey) {
				return
			}
			m.setBusy(busyKey, true)
			host := m.dockerBackendHost()
			removed := 0
			m.async(func(ctx context.Context) error {
				c, err := dockerapi.New(host)
				if err != nil {
					return err
				}
				defer c.Close()
				var errs []error
				for _, img := range removable {
					rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					err := c.RemoveImagePlatform(rctx, img.Target(), true, img.Platform())
					cancel()
					if err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", img.Ref(), err))
						continue
					}
					removed++
				}
				return errors.Join(errs...)
			}, func(err error) {
				m.setBusy(busyKey, false)
				if onDone != nil {
					onDone()
				}
				if err != nil {
					m.toastError(tr("Could not remove some images"), err)
				} else if removed > 0 {
					m.toastOK(trn(removed, "Removed %d image", "Removed %d images"), "")
				}
				m.LoadDockerImages()
				m.LoadDocker()
				if ggui.Peek(m.Settings).SandboxEngine {
					m.LoadImages()
					m.LoadDisk()
				}
			})
		},
	)
}

// AskDeleteLocalRegistryImages confirms and deletes multiple tagged images from
// the built-in local registry.
func (m *Model) AskDeleteLocalRegistryImages(imgs []localreg.ImageEntry, onDone func()) {
	const busyKey = "lreg:bulk"
	if len(imgs) == 0 || m.isBusy(busyKey) {
		return
	}

	var totalBytes int64
	for _, img := range imgs {
		totalBytes += img.Size
	}
	m.Confirm(
		trn(len(imgs), "Delete %d image?", "Delete %d images?"),
		trn(len(imgs),
			"%d selected tag is removed from the local registry (%s) and unused layers are reclaimed.",
			"%d selected tags are removed from the local registry (%s) and unused layers are reclaimed.",
			formatBytes(totalBytes),
		),
		tr("Delete images"),
		func() {
			if m.isBusy(busyKey) {
				return
			}
			srv := m.ensureLocalRegServer()
			if srv == nil {
				return
			}
			m.setBusy(busyKey, true)
			deleted := 0
			var reclaimed int64
			m.async(func(context.Context) error {
				var errs []error
				for _, img := range imgs {
					res, err := srv.DeleteTag(img.Repository, img.Tag)
					if err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", img.Key(), err))
						continue
					}
					deleted++
					reclaimed += res.ReclaimedBytes
				}
				return errors.Join(errs...)
			}, func(err error) {
				m.setBusy(busyKey, false)
				if onDone != nil {
					onDone()
				}
				if err != nil {
					m.toastError(tr("Could not delete some images"), err)
				} else if deleted > 0 {
					detail := ""
					if reclaimed > 0 {
						detail = tr("Reclaimed %s.", formatBytes(reclaimed))
					}
					m.toastOK(trn(deleted, "Deleted %d image", "Deleted %d images"), detail)
				}
				m.LoadLocalRegistry()
			})
		},
	)
}

// ---- Bulk Container Actions ---------------------------------------------

// BulkContainerAction runs start or stop across all eligible selected containers.
func (m *Model) BulkContainerAction(action string, items []dockerapi.Container, onDone func()) {
	targets := projectTargets(action, items)
	const busyKey = "ctr:bulk"
	if len(targets) == 0 || m.Offline || m.isBusy(busyKey) {
		return
	}
	m.setBusy(busyKey, true)
	host := m.dockerBackendHost()
	doneCount := 0
	m.async(func(ctx context.Context) error {
		c, err := dockerapi.New(host)
		if err != nil {
			return err
		}
		defer c.Close()
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		var mu sync.Mutex
		var wg sync.WaitGroup
		var errs []error
		for _, t := range targets {
			wg.Go(func() {
				act := action
				if act == "start" && t.State == "paused" {
					act = "unpause"
				}
				if err := dockerDo(ctx, c, act, t.ID); err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
					mu.Unlock()
					return
				}
				if act == "unpause" {
					m.drainContainerSweeps(ctx, c, t.ID)
				}
				mu.Lock()
				doneCount++
				mu.Unlock()
			})
		}
		wg.Wait()
		return errors.Join(errs...)
	}, func(err error) {
		m.setBusy(busyKey, false)
		if onDone != nil {
			onDone()
		}
		if err != nil {
			title := tr("Could not stop some containers")
			if action == "start" {
				title = tr("Could not start some containers")
			}
			m.toastError(title, err)
		} else if doneCount > 0 {
			if action == "start" {
				m.toastOK(trn(doneCount, "Started %d container", "Started %d containers"), "")
			} else {
				m.toastOK(trn(doneCount, "Stopped %d container", "Stopped %d containers"), "")
			}
		}
		m.LoadDocker()
		if ggui.Peek(m.Page) == PageContainers {
			m.LoadContainerLogs()
		}
	})
}

// AskRemoveContainers confirms and stops/removes multiple selected containers.
func (m *Model) AskRemoveContainers(items []dockerapi.Container, onDone func()) {
	const busyKey = "ctr:bulk"
	if len(items) == 0 || m.isBusy(busyKey) {
		return
	}

	m.Confirm(
		trn(len(items), "Remove %d container?", "Remove %d containers?"),
		trn(len(items),
			"%d selected container is stopped and removed from the engine. Its volumes, networks and images stay.",
			"%d selected containers are stopped and removed from the engine. Their volumes, networks and images stay.",
		),
		tr("Remove containers"),
		func() {
			if m.Offline || m.isBusy(busyKey) {
				return
			}
			m.setBusy(busyKey, true)
			host := m.dockerBackendHost()
			removed := 0
			m.async(func(ctx context.Context) error {
				c, err := dockerapi.New(host)
				if err != nil {
					return err
				}
				defer c.Close()
				ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				var mu sync.Mutex
				var wg sync.WaitGroup
				var errs []error
				for _, t := range items {
					wg.Go(func() {
						if err := dockerDo(ctx, c, "remove", t.ID); err != nil {
							mu.Lock()
							errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
							mu.Unlock()
							return
						}
						mu.Lock()
						removed++
						mu.Unlock()
					})
				}
				wg.Wait()
				return errors.Join(errs...)
			}, func(err error) {
				m.setBusy(busyKey, false)
				if onDone != nil {
					onDone()
				}
				if err != nil {
					m.toastError(tr("Could not remove some containers"), err)
				} else if removed > 0 {
					m.toastOK(trn(removed, "Removed %d container", "Removed %d containers"), "")
				}
				m.LoadDocker()
				if ggui.Peek(m.Page) == PageContainers {
					m.LoadContainerLogs()
				}
			})
		},
	)
}

// ---- Bulk Sandbox Actions -----------------------------------------------

// BulkStartSandboxes starts every stopped sandbox in ids.
func (m *Model) BulkStartSandboxes(ids []string, onDone func()) {
	for _, id := range ids {
		if !m.sup.State(id).Status.Active() {
			m.StartSandbox(id)
		}
	}
	if onDone != nil {
		onDone()
	}
}

// BulkStopSandboxes stops every active sandbox in ids.
func (m *Model) BulkStopSandboxes(ids []string, onDone func()) {
	for _, id := range ids {
		if m.sup.State(id).Status.Active() {
			m.StopSandbox(id)
		}
	}
	if onDone != nil {
		onDone()
	}
}

// AskDeleteSandboxes confirms and deletes multiple selected sandboxes.
func (m *Model) AskDeleteSandboxes(ids []string, onDone func()) {
	if len(ids) == 0 {
		return
	}

	specsAll := ggui.Peek(m.Specs)
	var gone []string
	seen := map[string]bool{}
	anyRunning := false
	extraAgents := 0
	for _, id := range ids {
		spec, ok := m.spec(id)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		gone = append(gone, id)
		if m.sup.State(id).Status.Active() {
			anyRunning = true
		}
		if spec.Recipe == recipes.Kubernetes {
			for _, ag := range agentsOf(specsAll) {
				if !seen[ag] {
					seen[ag] = true
					gone = append(gone, ag)
					extraAgents++
					if m.sup.State(ag).Status.Active() {
						anyRunning = true
					}
				}
			}
		}
	}
	if len(gone) == 0 {
		return
	}

	text := trn(len(ids),
		"%d selected sandbox and its logs are removed. Volumes and images stay.",
		"%d selected sandboxes and their logs are removed. Volumes and images stay.",
	)
	if anyRunning {
		text = tr("Running sandboxes will be stopped first.") + " " + text
	}
	if extraAgents > 0 {
		text = trn(extraAgents, "Its %d other node is deleted with it. ", "Its %d other nodes are deleted with it. ") + text
	}

	m.Confirm(
		trn(len(gone), "Delete %d sandbox?", "Delete %d sandboxes?"),
		text,
		tr("Delete sandboxes"),
		func() {
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
				specs := slices.DeleteFunc(slices.Clone(ggui.Peek(m.Specs)), func(s sandbox.Spec) bool {
					return slices.Contains(gone, s.ID)
				})
				if slices.Contains(gone, ggui.Peek(m.Selected)) {
					next := ""
					if len(specs) > 0 {
						next = specs[0].ID
					}
					m.Select(next)
				}
				m.saveSpecs(specs)
				if onDone != nil {
					onDone()
				}
				m.Toasts.Push(ui.Toast(
					trn(len(gone), "Deleted %d sandbox", "Deleted %d sandboxes"),
					tr("Its volumes are still listed under Volumes."),
				).Info())
			})
		},
	)
}

// ---- Bulk Volume Removal ------------------------------------------------

// AskRemoveVolumes confirms and deletes multiple selected unused volumes.
func (m *Model) AskRemoveVolumes(vols []rungv.Volume, onDone func()) {
	const busyKey = "vol:bulk"
	if len(vols) == 0 || m.isBusy(busyKey) {
		return
	}
	var targets []rungv.Volume
	for _, v := range vols {
		if !v.InUse {
			targets = append(targets, v)
		}
	}
	if len(targets) == 0 {
		return
	}

	m.Confirm(
		trn(len(targets), "Delete %d volume?", "Delete %d volumes?"),
		trn(len(targets),
			"All data in %d selected volume is deleted. This cannot be undone.",
			"All data in %d selected volumes is deleted. This cannot be undone.",
		),
		tr("Delete volumes"),
		func() {
			if m.isBusy(busyKey) {
				return
			}
			m.setBusy(busyKey, true)
			c := m.Client()
			deleted := 0
			m.async(func(ctx context.Context) error {
				var errs []error
				for _, v := range targets {
					if err := c.RemoveVolume(ctx, v.Name); err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", v.Name, err))
						continue
					}
					deleted++
				}
				return errors.Join(errs...)
			}, func(err error) {
				m.setBusy(busyKey, false)
				if onDone != nil {
					onDone()
				}
				if err != nil {
					m.toastError(tr("Could not delete some volumes"), err)
				} else if deleted > 0 {
					m.toastOK(trn(deleted, "Deleted %d volume", "Deleted %d volumes"), "")
				}
				m.LoadVolumes()
				m.LoadDisk()
			})
		},
	)
}

// ---- Bulk Network Removal -----------------------------------------------

// AskRemoveNetworks confirms and removes multiple selected removable networks.
func (m *Model) AskRemoveNetworks(nets []dockerapi.Network, onDone func()) {
	const busyKey = "net:bulk"
	if len(nets) == 0 || m.isBusy(busyKey) {
		return
	}
	var targets []dockerapi.Network
	for _, n := range nets {
		if networkRemovable(n) == "" {
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		return
	}

	m.Confirm(
		trn(len(targets), "Remove %d network?", "Remove %d networks?"),
		trn(len(targets),
			"%d selected network is removed from the engine.",
			"%d selected networks are removed from the engine.",
		),
		tr("Remove networks"),
		func() {
			removed := 0
			m.networkCall(busyKey, func(ctx context.Context, c *dockerapi.Client) error {
				var errs []error
				for _, n := range targets {
					if err := c.RemoveNetwork(ctx, n.ID); err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", n.Name, err))
						continue
					}
					removed++
				}
				return errors.Join(errs...)
			}, tr("Could not remove some networks"), func() {
				if onDone != nil {
					onDone()
				}
				if removed > 0 {
					m.toastOK(trn(removed, "Removed %d network", "Removed %d networks"), "")
				}
			})
		},
	)
}

// ---- Bulk Snapshot Deletion ---------------------------------------------

// AskDeleteSnapshots confirms and removes multiple selected snapshot bundles.
func (m *Model) AskDeleteSnapshots(snaps []sandbox.Snapshot, onDone func()) {
	if len(snaps) == 0 {
		return
	}
	var totalBytes int64
	for _, s := range snaps {
		totalBytes += s.Size
	}
	m.Confirm(
		trn(len(snaps), "Delete %d snapshot?", "Delete %d snapshots?"),
		trn(len(snaps),
			"%d selected snapshot bundle (%s) is removed from disk.",
			"%d selected snapshot bundles (%s) are removed from disk.",
			formatBytes(totalBytes),
		),
		tr("Delete snapshots"),
		func() {
			deleted := 0
			m.async(func(context.Context) error {
				var errs []error
				for _, s := range snaps {
					if err := removeAll(s.Dir); err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
						continue
					}
					deleted++
				}
				return errors.Join(errs...)
			}, func(err error) {
				if onDone != nil {
					onDone()
				}
				if err != nil {
					m.toastError(tr("Could not delete some snapshots"), err)
				} else if deleted > 0 {
					m.toastOK(trn(deleted, "Deleted %d snapshot", "Deleted %d snapshots"), "")
				}
				m.LoadSnapshots()
			})
		},
	)
}
