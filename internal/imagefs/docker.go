package imagefs

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/ironpark/gvx-desktop/internal/dockerapi"
)

// dockerFS holds an in-memory directory index built from a Docker Engine
// image's merged rootfs, fetching individual file previews on demand.
type dockerFS struct {
	host    string
	imageID string
	layers  []Layer
	dirs    map[string][]Entry
}

// OpenDocker indexes the merged filesystem of imageID from a Docker Engine
// reachable at host, and reads its layer history. progress, when non-nil, is
// called periodically with the number of entries indexed so far.
func OpenDocker(ctx context.Context, host, imageID string, progress func(entries int)) (FS, error) {
	c, err := dockerapi.New(host)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	// Layer history (Docker returns newest first; flip to oldest-first 1..N).
	hist, err := c.ImageHistory(ctx, imageID)
	var layers []Layer
	if err == nil {
		layers = make([]Layer, 0, len(hist))
		for _, h := range slices.Backward(hist) {

			var created time.Time
			if h.Created > 0 {
				created = time.Unix(h.Created, 0)
			}
			layers = append(layers, Layer{
				Index:   len(layers) + 1,
				Digest:  h.ID,
				Size:    h.Size,
				Created: created,
				Command: CleanCommand(h.CreatedBy),
				Empty:   h.Size == 0,
			})
		}
	}

	dirMap := map[string]map[string]Entry{
		"/": {},
	}
	ensureDir := func(p string) {
		p = CleanPath(p)
		if p == "/" {
			return
		}
		parts := strings.Split(strings.Trim(p, "/"), "/")
		cur := "/"
		for _, seg := range parts {
			next := path.Join(cur, seg)
			if _, ok := dirMap[cur]; !ok {
				dirMap[cur] = map[string]Entry{}
			}
			if _, exists := dirMap[cur][seg]; !exists {
				dirMap[cur][seg] = Entry{
					Name: seg,
					Path: next,
					Kind: KindDir,
					Mode: 0o040755,
				}
			}
			if _, ok := dirMap[next]; !ok {
				dirMap[next] = map[string]Entry{}
			}
			cur = next
		}
	}

	count := 0
	err = c.ExportImageRootFS(ctx, imageID, func(tr *tar.Reader) error {
		for {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			name := strings.TrimPrefix(hdr.Name, "./")
			name = strings.Trim(name, "/")
			if name == "" || name == "." {
				continue
			}
			full := "/" + path.Clean(name)
			if full == "/" {
				continue
			}
			parent := path.Dir(full)
			base := path.Base(full)
			ensureDir(parent)

			var kind Kind
			mode := uint32(hdr.Mode) & 0o7777
			switch hdr.Typeflag {
			case tar.TypeDir:
				kind = KindDir
				mode |= 0o040000
				if _, ok := dirMap[full]; !ok {
					dirMap[full] = map[string]Entry{}
				}
			case tar.TypeSymlink:
				kind = KindSymlink
				mode |= 0o120000
			case tar.TypeReg, tar.TypeLink:
				kind = KindFile
				mode |= 0o100000
			case tar.TypeChar:
				kind = KindOther
				mode |= 0o020000
			case tar.TypeBlock:
				kind = KindOther
				mode |= 0o060000
			case tar.TypeFifo:
				kind = KindOther
				mode |= 0o010000
			default:
				continue
			}

			dirMap[parent][base] = Entry{
				Name:    base,
				Path:    full,
				Kind:    kind,
				Mode:    mode,
				Size:    hdr.Size,
				UID:     uint32(hdr.Uid),
				GID:     uint32(hdr.Gid),
				ModTime: hdr.ModTime,
				Target:  hdr.Linkname,
			}
			count++
			if progress != nil && count%500 == 0 {
				progress(count)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if progress != nil {
		progress(count)
	}

	dirs := make(map[string][]Entry, len(dirMap))
	for dir, children := range dirMap {
		list := make([]Entry, 0, len(children))
		for _, e := range children {
			list = append(list, e)
		}
		SortEntries(list)
		dirs[dir] = list
	}
	return &dockerFS{
		host:    host,
		imageID: imageID,
		layers:  layers,
		dirs:    dirs,
	}, nil
}

func (f *dockerFS) Layers() []Layer {
	out := make([]Layer, len(f.layers))
	copy(out, f.layers)
	return out
}

func (f *dockerFS) ReadDir(_ context.Context, dir string, _ int) ([]Entry, error) {
	list := f.dirs[CleanPath(dir)]
	out := make([]Entry, len(list))
	copy(out, list)
	return out, nil
}

func (f *dockerFS) ReadFile(ctx context.Context, filePath string, _ int, limit int64) (FileContent, error) {
	c, err := dockerapi.New(f.host)
	if err != nil {
		return FileContent{}, err
	}
	defer c.Close()
	data, total, err := c.ReadImageFile(ctx, f.imageID, CleanPath(filePath), limit)
	if err != nil {
		return FileContent{}, err
	}
	return FileContent{
		Data:      data,
		TotalSize: total,
		Truncated: limit > 0 && total > limit,
		Binary:    IsBinary(data),
	}, nil
}

func (f *dockerFS) Close() error { return nil }
