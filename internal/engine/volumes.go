package engine

// Owner: workstream C.

import (
	"context"
	"maps"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// volumeState is the engine's volumes, as volumes.json keeps them: the
// rungv volumes it made, with what docker keeps of them besides.
type volumeState struct {
	mu     sync.Mutex
	byName map[string]*Volume
}

// Volume is a volume the engine made, through the API or for a container.
type Volume struct {
	Name      string            `json:"name"`
	Driver    string            `json:"driver"` // rungv's: ext4lite or hostfs; docker sees "local"
	Labels    map[string]string `json:"labels,omitempty"`
	Options   map[string]string `json:"options,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	// Anonymous marks a volume made for an image's VOLUME, named by 64 hex
	// digits as docker names them.
	Anonymous bool `json:"anonymous,omitempty"`
}

type volumesFile struct {
	Version int       `json:"version"`
	Volumes []*Volume `json:"volumes"`
}

type dockerVolume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Scope      string            `json:"Scope"`
	Options    map[string]string `json:"Options"`
	UsageData  *volumeUsageData  `json:"UsageData,omitempty"`
}

type volumeUsageData struct {
	Size     int64 `json:"Size"`
	RefCount int64 `json:"RefCount"`
}

// ---- seams: called by engine.go and workstream B; frozen ----

// loadVolumes reads volumes.json.
func (e *Engine) loadVolumes() error {
	var f volumesFile
	if err := loadFile(e.path("volumes.json"), &f); err != nil {
		return err
	}
	e.vol.mu.Lock()
	defer e.vol.mu.Unlock()
	e.vol.byName = make(map[string]*Volume, len(f.Volumes))
	for _, v := range f.Volumes {
		if v == nil || v.Name == "" {
			continue
		}
		e.vol.byName[v.Name] = v
	}
	return nil
}

// saveVolumesLocked writes volumes.json. Caller must hold e.vol.mu.
func (e *Engine) saveVolumesLocked() error {
	list := make([]*Volume, 0, len(e.vol.byName))
	for _, v := range e.vol.byName {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return saveFile(e.path("volumes.json"), volumesFile{
		Version: storeVersion,
		Volumes: list,
	})
}

// ensureVolume makes the named volume if it is missing, as a container's
// create does for a volume it names: a volume the engine made, or another
// rungv volume of that name, is used as it is; a new one is recorded with
// labels. anonymous marks a volume made for an image's VOLUME.
func (e *Engine) ensureVolume(ctx context.Context, name string, labels map[string]string, anonymous bool) error {
	if !volumeNameRe.MatchString(name) {
		return errInvalid("invalid volume name %q", name)
	}
	if name == e.builderVolumeName() {
		return errConflict("volume %q is reserved for the sandbox engine's builder cache", name)
	}
	e.vol.mu.Lock()
	if _, ok := e.vol.byName[name]; ok {
		e.vol.mu.Unlock()
		return nil
	}
	e.vol.mu.Unlock()

	// If rungv already has this volume (e.g. a user sandbox volume), use it
	// without recording it as an engine-managed volume.
	existing, err := e.runner.Volumes(ctx)
	if err != nil {
		return err
	}
	for _, rv := range existing {
		if rv.Name == name {
			return nil
		}
	}

	if err := e.runner.CreateVolume(ctx, name, "ext4lite"); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return nil
		}
		return err
	}

	e.vol.mu.Lock()
	if e.vol.byName == nil {
		e.vol.byName = map[string]*Volume{}
	}
	if _, exists := e.vol.byName[name]; !exists {
		e.vol.byName[name] = &Volume{
			Name:      name,
			Driver:    "ext4lite",
			Labels:    maps.Clone(labels),
			CreatedAt: time.Now().UTC(),
			Anonymous: anonymous,
		}
		if err := e.saveVolumesLocked(); err != nil {
			e.vol.mu.Unlock()
			return err
		}
	}
	e.vol.mu.Unlock()

	e.emit(Event{
		Type:       "volume",
		Action:     "create",
		ID:         name,
		Attributes: map[string]string{"driver": "local"},
	})
	e.notify()
	return nil
}

// removeVolume deletes a volume the engine made, as rm -v does for a
// container's anonymous volumes. A volume another container mounts is a
// 409 unless force; one the engine did not make is a 403.
func (e *Engine) removeVolume(ctx context.Context, name string, force bool) error {
	return e.removeVolumeExcluding(ctx, name, force, "")
}

func (e *Engine) removeVolumeExcluding(ctx context.Context, name string, force bool, excludeContainerID string) error {
	if name == e.builderVolumeName() {
		return &apiError{http.StatusForbidden, "volume " + name + " is reserved for the sandbox engine's builder cache"}
	}
	e.vol.mu.Lock()
	_, ok := e.vol.byName[name]
	e.vol.mu.Unlock()
	if !ok {
		return &apiError{http.StatusForbidden, "volume " + name + " was not created by the sandbox engine"}
	}

	if !force {
		if users := e.volumeUsersExcept(name, excludeContainerID); len(users) > 0 {
			return errConflict("remove %s: volume is in use - [%s]", name, strings.Join(users, ", "))
		}
	}

	if err := e.runner.RemoveVolume(ctx, name); err != nil {
		m := strings.ToLower(err.Error())
		if strings.Contains(m, "in use") || strings.Contains(m, "locked") {
			return errConflict("remove %s: %s", name, err.Error())
		}
		if !strings.Contains(m, "not found") {
			return err
		}
	}

	e.vol.mu.Lock()
	delete(e.vol.byName, name)
	err := e.saveVolumesLocked()
	e.vol.mu.Unlock()
	if err != nil {
		return err
	}

	e.emit(Event{
		Type:       "volume",
		Action:     "destroy",
		ID:         name,
		Attributes: map[string]string{"driver": "local"},
	})
	e.notify()
	return nil
}

func (e *Engine) volumeUsers(name string) []string {
	return e.volumeUsersExcept(name, "")
}

func (e *Engine) volumeUsersExcept(name, excludeContainerID string) []string {
	var ids []string
	for _, c := range e.allContainers() {
		if excludeContainerID != "" && c.ID == excludeContainerID {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name == name {
				ids = append(ids, c.ID)
				break
			}
		}
	}
	return ids
}

func (e *Engine) volumeRefCounts() map[string]int64 {
	counts := map[string]int64{}
	for _, c := range e.allContainers() {
		seen := map[string]bool{}
		for _, m := range c.Mounts {
			if m.Type == "volume" && m.Name != "" && !seen[m.Name] {
				seen[m.Name] = true
				counts[m.Name]++
			}
		}
	}
	return counts
}

func (e *Engine) snapshotVolumes() []*Volume {
	e.vol.mu.Lock()
	defer e.vol.mu.Unlock()
	out := make([]*Volume, 0, len(e.vol.byName))
	for _, v := range e.vol.byName {
		cp := *v
		cp.Labels = maps.Clone(v.Labels)
		cp.Options = maps.Clone(v.Options)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (e *Engine) rungvVolumeMap(ctx context.Context) map[string]rungv.Volume {
	vols, err := e.runner.Volumes(ctx)
	if err != nil {
		return nil
	}
	m := make(map[string]rungv.Volume, len(vols))
	for _, rv := range vols {
		m[rv.Name] = rv
	}
	return m
}

func toDockerVolume(v *Volume, rv rungv.Volume, usage *volumeUsageData) dockerVolume {
	labels := maps.Clone(v.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	opts := maps.Clone(v.Options)
	if opts == nil {
		opts = map[string]string{}
	}
	created := v.CreatedAt
	if created.IsZero() {
		if t, err := parseRungvTime(rv.CreatedAt); err == nil {
			created = t
		}
	}
	return dockerVolume{
		Name:       v.Name,
		Driver:     "local",
		Mountpoint: rv.Mountpoint,
		CreatedAt:  created.UTC().Format(time.RFC3339),
		Labels:     labels,
		Scope:      "local",
		Options:    opts,
		UsageData:  usage,
	}
}

// ---- handlers ----

func (e *Engine) volumeList(w http.ResponseWriter, r *http.Request) {
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	vols := e.snapshotVolumes()
	rvMap := e.rungvVolumeMap(r.Context())
	refs := e.volumeRefCounts()

	out := make([]dockerVolume, 0, len(vols))
	for _, v := range vols {
		if !f.matchAny("name", func(s string) bool { return strings.Contains(v.Name, s) }) {
			continue
		}
		if !f.matchAny("driver", func(s string) bool { return s == "local" }) {
			continue
		}
		if !f.matchLabels(v.Labels) {
			continue
		}
		if f.has("dangling") {
			isDangling := refs[v.Name] == 0
			if !f.matchAny("dangling", func(s string) bool {
				want := strings.EqualFold(s, "true") || s == "1"
				return isDangling == want
			}) {
				continue
			}
		}
		out = append(out, toDockerVolume(v, rvMap[v.Name], nil))
	}
	writeJSON(w, http.StatusOK, struct {
		Volumes  []dockerVolume `json:"Volumes"`
		Warnings []string       `json:"Warnings"`
	}{
		Volumes:  out,
		Warnings: []string{},
	})
}

func (e *Engine) volumeCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string            `json:"Name"`
		Driver     string            `json:"Driver"`
		DriverOpts map[string]string `json:"DriverOpts"`
		Labels     map[string]string `json:"Labels"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.Driver != "" && req.Driver != "local" {
		writeError(w, r, errInvalid("volume driver %q is not supported (only \"local\")", req.Driver))
		return
	}
	rungvDriver := "ext4lite"
	if strings.EqualFold(req.DriverOpts["type"], "hostfs") {
		rungvDriver = "hostfs"
	}

	name := strings.TrimSpace(req.Name)
	anonymous := false
	if name == "" {
		name = randomHex64()
		anonymous = true
	}
	if !volumeNameRe.MatchString(name) {
		writeError(w, r, errInvalid("invalid volume name %q: only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", name))
		return
	}
	if name == e.builderVolumeName() {
		writeError(w, r, errConflict("volume %q is reserved for the sandbox engine's builder cache", name))
		return
	}

	// Returning an existing engine volume matches dockerd behavior.
	e.vol.mu.Lock()
	if existing, ok := e.vol.byName[name]; ok {
		cp := *existing
		cp.Labels = maps.Clone(existing.Labels)
		cp.Options = maps.Clone(existing.Options)
		e.vol.mu.Unlock()
		rvMap := e.rungvVolumeMap(r.Context())
		writeJSON(w, http.StatusCreated, toDockerVolume(&cp, rvMap[name], nil))
		return
	}
	e.vol.mu.Unlock()

	// Refuse to adopt a pre-existing rungv volume created by a user sandbox.
	if rvMap := e.rungvVolumeMap(r.Context()); rvMap != nil {
		if _, exists := rvMap[name]; exists {
			writeError(w, r, errConflict("volume %q already exists outside the sandbox engine", name))
			return
		}
	}

	if err := e.runner.CreateVolume(r.Context(), name, rungvDriver); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already exists") {
			writeError(w, r, errConflict("volume %q already exists", name))
			return
		}
		writeError(w, r, err)
		return
	}

	v := &Volume{
		Name:      name,
		Driver:    rungvDriver,
		Labels:    maps.Clone(req.Labels),
		Options:   maps.Clone(req.DriverOpts),
		CreatedAt: time.Now().UTC(),
		Anonymous: anonymous,
	}
	e.vol.mu.Lock()
	if e.vol.byName == nil {
		e.vol.byName = map[string]*Volume{}
	}
	e.vol.byName[name] = v
	err := e.saveVolumesLocked()
	cp := *v
	cp.Labels = maps.Clone(v.Labels)
	cp.Options = maps.Clone(v.Options)
	e.vol.mu.Unlock()
	if err != nil {
		writeError(w, r, err)
		return
	}

	e.emit(Event{
		Type:       "volume",
		Action:     "create",
		ID:         name,
		Attributes: map[string]string{"driver": "local"},
	})
	e.notify()

	rvMap := e.rungvVolumeMap(r.Context())
	writeJSON(w, http.StatusCreated, toDockerVolume(&cp, rvMap[name], nil))
}

func (e *Engine) volumeInspect(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	e.vol.mu.Lock()
	v, ok := e.vol.byName[name]
	var cp Volume
	if ok {
		cp = *v
		cp.Labels = maps.Clone(v.Labels)
		cp.Options = maps.Clone(v.Options)
	}
	e.vol.mu.Unlock()
	if !ok {
		writeError(w, r, errNotFound("get %s: no such volume", name))
		return
	}
	rvMap := e.rungvVolumeMap(r.Context())
	writeJSON(w, http.StatusOK, toDockerVolume(&cp, rvMap[name], nil))
}

func (e *Engine) volumeDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == e.builderVolumeName() {
		writeError(w, r, &apiError{http.StatusForbidden, "volume " + name + " is reserved for the sandbox engine's builder cache"})
		return
	}
	e.vol.mu.Lock()
	_, ok := e.vol.byName[name]
	e.vol.mu.Unlock()
	if !ok {
		writeError(w, r, errNotFound("remove %s: no such volume", name))
		return
	}
	if users := e.volumeUsers(name); len(users) > 0 {
		writeError(w, r, errConflict("remove %s: volume is in use - [%s]", name, strings.Join(users, ", ")))
		return
	}
	if err := e.removeVolume(r.Context(), name, true); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (e *Engine) volumePrune(w http.ResponseWriter, r *http.Request) {
	f, err := queryFilters(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	all := queryBool(r, "all")
	if f.has("all") {
		for _, v := range f["all"] {
			if strings.EqualFold(v, "true") || v == "1" {
				all = true
			}
		}
	}

	// Never call rungv volume prune: only remove unused volumes this engine made.
	vols := e.snapshotVolumes()
	rvMap := e.rungvVolumeMap(r.Context())
	refs := e.volumeRefCounts()

	deleted := []string{}
	var reclaimed int64
	for _, v := range vols {
		if v.Name == e.builderVolumeName() {
			continue
		}
		if refs[v.Name] > 0 {
			continue
		}
		if !all && !v.Anonymous {
			continue
		}
		if !f.matchLabels(v.Labels) {
			continue
		}
		if err := e.removeVolume(r.Context(), v.Name, false); err != nil {
			continue
		}
		deleted = append(deleted, v.Name)
		if rv, ok := rvMap[v.Name]; ok {
			reclaimed += parseHumanSize(rv.Size)
		}
	}

	writeJSON(w, http.StatusOK, struct {
		VolumesDeleted []string `json:"VolumesDeleted"`
		SpaceReclaimed int64    `json:"SpaceReclaimed"`
	}{
		VolumesDeleted: deleted,
		SpaceReclaimed: reclaimed,
	})
}
