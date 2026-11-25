package engine

// Owner: workstream C. The seam signature refuseForeignImage is frozen
// (containers.go calls it); its body and everything else here are C's.

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/ironpark/gvx-desktop/internal/rungv"
)

// maxImageLoadBytes caps the body spooled to disk by POST /images/load.
// Package var so unit tests can exercise the cap without writing gigabytes.
var maxImageLoadBytes int64 = 4 << 30 // 4 GiB

type imageManifestSummary struct {
	ID         string                `json:"ID"`
	Descriptor imageOCIDescriptor    `json:"Descriptor"`
	Available  bool                  `json:"Available"`
	Size       imageManifestSize     `json:"Size"`
	Kind       string                `json:"Kind"`
	ImageData  *imageManifestImgData `json:"ImageData,omitempty"`
}

type imageOCIDescriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  *imageOCIPlatform `json:"platform,omitempty"`
}

type imageOCIPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

type imageManifestSize struct {
	Total   int64 `json:"Total"`
	Content int64 `json:"Content"`
}

type imageManifestImgData struct {
	Platform imageOCIPlatform   `json:"Platform"`
	Size     imageManifestSizes `json:"Size"`
}

type imageManifestSizes struct {
	Unpacked int64 `json:"Unpacked"`
}

func makeImageManifests(id, arch, osName string, size int64) []imageManifestSummary {
	if arch == "" {
		arch = hostImageArch
	}
	if osName == "" {
		osName = "linux"
	}
	plat := imageOCIPlatform{
		Architecture: arch,
		OS:           osName,
	}
	return []imageManifestSummary{{
		ID: id,
		Descriptor: imageOCIDescriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    id,
			Size:      size,
			Platform:  &plat,
		},
		Available: true,
		Size: imageManifestSize{
			Total:   size,
			Content: size,
		},
		Kind: "image",
		ImageData: &imageManifestImgData{
			Platform: plat,
			Size: imageManifestSizes{
				Unpacked: size,
			},
		},
	}}
}

func normalizeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	case "", "unknown":
		return ""
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}

// parsePlatformArch extracts the normalized architecture from a Docker platform
// string such as "linux/amd64", "linux/arm64/v8", or "amd64".
func parsePlatformArch(platform string) string {
	p := strings.TrimSpace(strings.ToLower(platform))
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	if len(parts) >= 2 && parts[0] == "linux" {
		return normalizeArch(parts[1])
	}
	if len(parts) == 1 {
		return normalizeArch(parts[0])
	}
	return ""
}

func (e *Engine) archRunner() (ImageArchRunner, bool) {
	ar, ok := e.runner.(ImageArchRunner)
	return ar, ok
}

// markForeignImagesPossible records that the foreign-architecture image
// namespace may contain images so GET /images/json queries it.
func (e *Engine) markForeignImagesPossible() {
	e.img.mu.Lock()
	e.img.foreignChecked = true
	e.img.hasForeign = true
	e.img.mu.Unlock()
}

// shouldListForeignImages reports whether GET /images/json needs to invoke
// ImagesArch for the foreign architecture. When the amd64 builder is off and a
// prior check found no foreign images (and none have been loaded/pulled since),
// we skip the extra rungv subprocess on every poll.
func (e *Engine) shouldListForeignImages() bool {
	if e.isAmd64Enabled() || e.isMicroVMEnabled() {
		return true
	}
	e.img.mu.Lock()
	need := !e.img.foreignChecked || e.img.hasForeign
	e.img.mu.Unlock()
	return need
}

func (e *Engine) recordForeignImagePresence(present bool) {
	e.img.mu.Lock()
	e.img.foreignChecked = true
	e.img.hasForeign = present
	e.img.mu.Unlock()
}

func (e *Engine) imagesInArch(ctx context.Context, arch string) ([]rungv.Image, error) {
	if arch == "" || arch == hostImageArch {
		return e.runner.Images(ctx)
	}
	ar, ok := e.archRunner()
	if !ok {
		return nil, nil
	}
	return ar.ImagesArch(ctx, arch)
}

func (e *Engine) inspectImagesInArch(ctx context.Context, arch string, refs ...string) ([]ImageInspect, error) {
	if arch == "" || arch == hostImageArch {
		return e.runner.InspectImages(ctx, refs...)
	}
	ar, ok := e.archRunner()
	if !ok {
		return nil, errNotFound("No such image: %s", strings.Join(refs, ", "))
	}
	return ar.InspectImagesArch(ctx, arch, refs...)
}

func (e *Engine) removeImageInArch(ctx context.Context, arch, ref string, force bool) error {
	if arch == "" || arch == hostImageArch {
		return e.runner.RemoveImage(ctx, ref, force)
	}
	ar, ok := e.archRunner()
	if !ok {
		return errNotFound("No such image: %s", ref)
	}
	return ar.RemoveImageArch(ctx, arch, ref, force)
}

func (e *Engine) tagImageInArch(ctx context.Context, arch, source, target string) error {
	if arch == "" || arch == hostImageArch {
		return e.runner.TagImage(ctx, source, target)
	}
	ar, ok := e.archRunner()
	if !ok {
		return errNotFound("No such image: %s", source)
	}
	return ar.TagImageArch(ctx, arch, source, target)
}

func (e *Engine) loadImagesInArch(ctx context.Context, arch string, archive io.Reader, line func(string)) error {
	if arch == "" || arch == hostImageArch {
		return e.runner.LoadImages(ctx, archive, line)
	}
	ar, ok := e.archRunner()
	if !ok {
		return e.runner.LoadImages(ctx, archive, line)
	}
	e.markForeignImagesPossible()
	return ar.LoadImagesArch(ctx, arch, archive, line)
}

func (e *Engine) saveImagesInArch(ctx context.Context, arch string, w io.Writer, refs ...string) error {
	if arch == "" || arch == hostImageArch {
		return e.runner.SaveImages(ctx, w, refs...)
	}
	ar, ok := e.archRunner()
	if !ok {
		return errNotFound("No such image: %s", strings.Join(refs, ", "))
	}
	return ar.SaveImagesArch(ctx, arch, w, refs...)
}

func (e *Engine) resolveImageIDPrefixInArch(ctx context.Context, arch, ref string) string {
	needle := strings.TrimPrefix(strings.ToLower(ref), "sha256:")
	if len(needle) < 3 || !isHexString(needle) {
		return ""
	}
	rows, err := e.imagesInArch(ctx, arch)
	if err != nil {
		return ""
	}
	var match string
	for _, row := range rows {
		id := row.ID
		hex := strings.TrimPrefix(strings.ToLower(id), "sha256:")
		if strings.HasPrefix(hex, needle) {
			if match != "" && match != id {
				return "" // ambiguous
			}
			match = id
		}
	}
	return match
}

func (e *Engine) lookupImageInArch(ctx context.Context, arch, ref string) (ImageInspect, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ImageInspect{}, errNotFound("No such image: %s", ref)
	}
	if err := validateImageRef(ref); err != nil {
		return ImageInspect{}, err
	}
	if imgs, err := e.inspectImagesInArch(ctx, arch, ref); err == nil && len(imgs) > 0 {
		img := imgs[0]
		img.ResolvedRef = resolvedCacheRef(ref, img)
		return img, nil
	} else if err != nil && errors.Is(err, rungv.ErrNotFound) {
		return ImageInspect{}, err
	}

	for _, alt := range imageRefAlternatives(ref) {
		if imgs, err := e.inspectImagesInArch(ctx, arch, alt); err == nil && len(imgs) > 0 {
			img := imgs[0]
			img.ResolvedRef = resolvedCacheRef(alt, img)
			return img, nil
		}
	}
	if id := e.resolveImageIDPrefixInArch(ctx, arch, ref); id != "" {
		if imgs, err := e.inspectImagesInArch(ctx, arch, id); err == nil && len(imgs) > 0 {
			img := imgs[0]
			img.ResolvedRef = resolvedCacheRef(id, img)
			return img, nil
		}
	}
	return ImageInspect{}, errNotFound("No such image: %s", ref)
}

// lookupImage resolves ref in the host namespace first, then the foreign
// namespace. It returns the inspected image and the namespace arch it came from.
func (e *Engine) lookupImage(ctx context.Context, ref string) (ImageInspect, string, error) {
	return e.lookupImageForPlatform(ctx, ref, "")
}

// lookupImageForPlatform resolves ref strictly in platform's architecture
// namespace when platform is non-empty (e.g. "linux/amd64"), preventing a
// colliding repo:tag from hitting the host-arch image. When platform is empty,
// it resolves host-first then foreign (and full image IDs match uniquely in
// whichever namespace owns them).
func (e *Engine) lookupImageForPlatform(ctx context.Context, ref, platform string) (ImageInspect, string, error) {
	ref = strings.TrimSpace(ref)
	if err := validateImageRef(ref); err != nil {
		return ImageInspect{}, "", err
	}
	platform = strings.TrimSpace(platform)
	if platform != "" {
		if err := validateImagePlatform(platform); err != nil {
			return ImageInspect{}, "", err
		}
		wantArch := parsePlatformArch(platform)
		if wantArch == "" {
			return ImageInspect{}, "", errInvalid("invalid platform: %q", platform)
		}
		img, err := e.lookupImageInArch(ctx, wantArch, ref)
		if err != nil {
			return ImageInspect{}, "", err
		}
		return img, wantArch, nil
	}

	img, err := e.lookupImageInArch(ctx, hostImageArch, ref)
	if err == nil {
		return img, hostImageArch, nil
	}
	if errors.Is(err, rungv.ErrNotFound) {
		return ImageInspect{}, "", err
	}
	if _, ok := e.archRunner(); ok {
		foreign := foreignImageArch()
		if fimg, ferr := e.lookupImageInArch(ctx, foreign, ref); ferr == nil {
			return fimg, foreign, nil
		}
	}
	return ImageInspect{}, "", errNotFound("No such image: %s", ref)
}

// resolveCreateImage resolves the image, execution runtime ("gvx" or
// "microvm"), and target platform ("linux/arm64" or "linux/amd64") for
// POST /containers/create according to the §3.2 runtime decision matrix.
func (e *Engine) resolveCreateImage(ctx context.Context, ref, platform, requestedRuntime string) (ImageInspect, string, string, error) {
	ref = strings.TrimSpace(ref)
	platform = strings.TrimSpace(platform)
	rawRuntime := strings.TrimSpace(requestedRuntime)
	reqRuntime := strings.ToLower(rawRuntime)

	switch reqRuntime {
	case "", RuntimeGVX, RuntimeMicroVM:
	default:
		return ImageInspect{}, "", "", errInvalid("Unknown runtime specified %s", rawRuntime)
	}
	if reqRuntime == RuntimeMicroVM && !e.isMicroVMEnabled() {
		return ImageInspect{}, "", "", errInvalid("Unknown or disabled runtime %q; enable Micro-VM container runtime in Labs", RuntimeMicroVM)
	}
	if err := validateImageRef(ref); err != nil {
		return ImageInspect{}, "", "", err
	}

	reqArch := ""
	if platform != "" {
		if err := validateImagePlatform(platform); err != nil {
			return ImageInspect{}, "", "", err
		}
		reqArch = parsePlatformArch(platform)
		if reqArch == "" {
			return ImageInspect{}, "", "", errInvalid("invalid platform: %q", platform)
		}
	}

	resolveForeign := func(arch string, cached *ImageInspect) (ImageInspect, string, string, error) {
		if reqRuntime == RuntimeGVX {
			return ImageInspect{}, "", "", errInvalid("linux/%s containers require the %s runtime", arch, RuntimeMicroVM)
		}
		if !e.isMicroVMEnabled() {
			return ImageInspect{}, "", "", errInvalid(foreignRunMessage+"; enable Micro-VM container runtime in Labs", arch, ref, arch, hostImageArch)
		}
		if arch != "amd64" && arch != "arm64" {
			return ImageInspect{}, "", "", errInvalid("unsupported architecture %q; micro-VM runtime supports linux/arm64 and linux/amd64", arch)
		}
		if arch == "amd64" && !e.isMicroVMRosettaReady() {
			return ImageInspect{}, "", "", errInvalid("Rosetta is required to run linux/amd64 containers; install Rosetta in Labs")
		}
		if cached != nil {
			img := *cached
			e.enrichImageHealthcheck(ctx, &img)
			return img, RuntimeMicroVM, "linux/" + arch, nil
		}
		img, err := e.lookupImageInArch(ctx, arch, ref)
		if err != nil {
			return ImageInspect{}, "", "", err
		}
		e.enrichImageHealthcheck(ctx, &img)
		return img, RuntimeMicroVM, "linux/" + arch, nil
	}

	if reqArch != "" {
		if reqArch != hostImageArch {
			return resolveForeign(reqArch, nil)
		}
		targetRuntime := RuntimeGVX
		if reqRuntime == RuntimeMicroVM {
			targetRuntime = RuntimeMicroVM
		}
		img, err := e.lookupImageInArch(ctx, hostImageArch, ref)
		if err != nil {
			return ImageInspect{}, "", "", err
		}
		if reqRuntime == "" && e.isMicroVMEnabled() && e.matchesMicroVMImage(ref, img.RepoTags) {
			targetRuntime = RuntimeMicroVM
		}
		e.enrichImageHealthcheck(ctx, &img)
		return img, targetRuntime, "linux/" + hostImageArch, nil
	}

	// No explicit platform: try host namespace first.
	img, err := e.lookupImageInArch(ctx, hostImageArch, ref)
	if err == nil {
		targetRuntime := RuntimeGVX
		if reqRuntime == RuntimeMicroVM {
			targetRuntime = RuntimeMicroVM
		}
		if reqRuntime == "" && e.isMicroVMEnabled() && e.matchesMicroVMImage(ref, img.RepoTags) {
			targetRuntime = RuntimeMicroVM
		}
		e.enrichImageHealthcheck(ctx, &img)
		return img, targetRuntime, "linux/" + hostImageArch, nil
	}
	if errors.Is(err, rungv.ErrNotFound) {
		return ImageInspect{}, "", "", err
	}

	// Check foreign namespace before returning 404.
	if _, ok := e.archRunner(); ok {
		foreign := foreignImageArch()
		if fimg, ferr := e.lookupImageInArch(ctx, foreign, ref); ferr == nil {
			arch := normalizeArch(fimg.Architecture)
			if arch == "" {
				arch = foreign
			}
			return resolveForeign(arch, &fimg)
		}
	}

	return ImageInspect{}, "", "", errNotFound("No such image: %s", ref)
}

// spoolImageArchive writes body into a temporary file under Config.Dir/tmp
// bounded by maxImageLoadBytes so the engine can inspect its architecture
// before feeding it to rungv load. On any error, the temp file is closed and
// removed before returning.
func (e *Engine) spoolImageArchive(body io.Reader) (f *os.File, cleanup func(), err error) {
	f, err = e.tempFile("load-*.tar")
	if err != nil {
		return nil, nil, err
	}
	name := f.Name()
	cleanup = func() {
		_ = f.Close()
		_ = os.Remove(name)
	}
	limit := maxImageLoadBytes
	if limit <= 0 {
		limit = 4 << 30
	}
	n, err := io.Copy(f, io.LimitReader(body, limit+1))
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if n > limit {
		cleanup()
		return nil, nil, errInvalid("image archive exceeds size limit (%d bytes)", limit)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, nil, err
	}
	return f, cleanup, nil
}

// detectArchiveArches reads a docker-archive (manifest.json) or OCI layout
// (index.json) tar (plain or gzip-compressed) and returns the deduplicated list
// of image architectures found, in order. Returns nil when the archive format
// cannot be recognized (callers fall back to hostImageArch).
func detectArchiveArches(rs io.ReadSeeker) []string {
	const maxMetaBytes = 4 << 20

	readWanted := func(want map[string]bool) map[string][]byte {
		if len(want) == 0 {
			return nil
		}
		if _, err := rs.Seek(0, io.SeekStart); err != nil {
			return nil
		}
		br := bufio.NewReaderSize(rs, 4096)
		var r io.Reader = br
		var gzr *gzip.Reader
		if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
			if gz, err := gzip.NewReader(br); err == nil {
				gzr = gz
				r = gz
				defer gzr.Close()
			}
		}
		tr := tar.NewReader(r)
		got := map[string][]byte{}
		for {
			hdr, err := tr.Next()
			if err != nil {
				break
			}
			if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
				continue
			}
			clean := cleanArchivePath(hdr.Name)
			if !want[clean] {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(tr, maxMetaBytes))
			if err == nil {
				got[clean] = b
			}
			if len(got) == len(want) {
				break
			}
		}
		return got
	}

	// Pass 1: read manifest.json (docker-archive) and index.json (OCI layout).
	top := readWanted(map[string]bool{
		"manifest.json": true,
		"index.json":    true,
	})
	if len(top) == 0 {
		return nil
	}

	var arches []string
	addArch := func(a string) {
		a = normalizeArch(a)
		if a != "" && !slices.Contains(arches, a) {
			arches = append(arches, a)
		}
	}

	// Pass 2: collect referenced config blobs (docker-archive) and manifest/index blobs (OCI).
	wantPass2 := map[string]bool{}
	var dockerConfigPaths []string
	if raw, ok := top["manifest.json"]; ok {
		var entries []struct {
			Config string `json:"Config"`
		}
		if json.Unmarshal(raw, &entries) == nil {
			for _, ent := range entries {
				if p := cleanArchivePath(ent.Config); p != "" {
					wantPass2[p] = true
					dockerConfigPaths = append(dockerConfigPaths, p)
				}
			}
		}
	}

	type ociDesc struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Platform  *struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
		Annotations map[string]string `json:"annotations"`
	}
	type ociIndex struct {
		Manifests []ociDesc `json:"manifests"`
	}

	var ociIndexPaths []string
	var ociManifestPaths []string
	processIndex := func(raw []byte, allowNested bool) {
		var idx ociIndex
		if json.Unmarshal(raw, &idx) != nil {
			return
		}
		for _, desc := range idx.Manifests {
			if desc.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
				continue
			}
			if desc.Platform != nil {
				if a := normalizeArch(desc.Platform.Architecture); a != "" {
					addArch(a)
					continue
				}
			}
			blobPath := ociDigestPath(desc.Digest)
			if blobPath == "" {
				continue
			}
			if allowNested && isIndexMediaType(desc.MediaType) {
				wantPass2[blobPath] = true
				ociIndexPaths = append(ociIndexPaths, blobPath)
			} else {
				wantPass2[blobPath] = true
				ociManifestPaths = append(ociManifestPaths, blobPath)
			}
		}
	}

	if raw, ok := top["index.json"]; ok {
		processIndex(raw, true)
	}

	if len(wantPass2) == 0 {
		return arches
	}
	pass2 := readWanted(wantPass2)

	readConfigArch := func(raw []byte) {
		var cfg struct {
			Architecture string `json:"architecture"`
		}
		if json.Unmarshal(raw, &cfg) == nil {
			addArch(cfg.Architecture)
		}
	}

	for _, p := range dockerConfigPaths {
		if raw, ok := pass2[p]; ok {
			readConfigArch(raw)
		}
	}

	// Collect config blobs referenced by OCI manifests (or nested indexes).
	wantPass3 := map[string]bool{}
	var nestedManifestPaths []string
	for _, p := range ociIndexPaths {
		raw, ok := pass2[p]
		if !ok {
			continue
		}
		var idx ociIndex
		if json.Unmarshal(raw, &idx) != nil {
			continue
		}
		for _, desc := range idx.Manifests {
			if desc.Annotations["vnd.docker.reference.type"] == "attestation-manifest" {
				continue
			}
			if desc.Platform != nil {
				if a := normalizeArch(desc.Platform.Architecture); a != "" {
					addArch(a)
					continue
				}
			}
			if bp := ociDigestPath(desc.Digest); bp != "" {
				wantPass3[bp] = true
				nestedManifestPaths = append(nestedManifestPaths, bp)
			}
		}
	}

	var configPathsPass3 []string
	extractManifestConfig := func(raw []byte) string {
		var mf struct {
			Config struct {
				Digest string `json:"digest"`
			} `json:"config"`
		}
		if json.Unmarshal(raw, &mf) != nil {
			return ""
		}
		return ociDigestPath(mf.Config.Digest)
	}

	for _, p := range ociManifestPaths {
		if raw, ok := pass2[p]; ok {
			if cp := extractManifestConfig(raw); cp != "" {
				if cRaw, already := pass2[cp]; already {
					readConfigArch(cRaw)
				} else {
					wantPass3[cp] = true
					configPathsPass3 = append(configPathsPass3, cp)
				}
			}
		}
	}

	if len(wantPass3) > 0 {
		pass3 := readWanted(wantPass3)
		for _, cp := range configPathsPass3 {
			if raw, ok := pass3[cp]; ok {
				readConfigArch(raw)
			}
		}
		wantPass4 := map[string]bool{}
		for _, mp := range nestedManifestPaths {
			if raw, ok := pass3[mp]; ok {
				if cp := extractManifestConfig(raw); cp != "" {
					wantPass4[cp] = true
				}
			}
		}
		if len(wantPass4) > 0 {
			pass4 := readWanted(wantPass4)
			for _, raw := range pass4 {
				readConfigArch(raw)
			}
		}
	}

	return arches
}

func cleanArchivePath(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		return ""
	}
	return path.Clean(name)
}

func ociDigestPath(digest string) string {
	alg, hex, ok := strings.Cut(strings.TrimSpace(digest), ":")
	if !ok || alg == "" || hex == "" {
		return ""
	}
	return path.Join("blobs", alg, hex)
}

func isIndexMediaType(mt string) bool {
	return mt == "application/vnd.oci.image.index.v1+json" ||
		mt == "application/vnd.docker.distribution.manifest.list.v2+json"
}
