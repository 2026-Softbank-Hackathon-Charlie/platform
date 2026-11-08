package fileguard

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// Listings and intermediate runs are private, per-operation files, never a
// reusable inspection cache. Only a fixed chunk and one item per run are held.
const inventoryChunk = 256
const maxListingBytes int64 = 8 << 20

type inventory struct {
	path   string
	count  int
	bytes  int64
	digest [32]byte
}
type objectWalk func(func(object) error) error

func (v inventory) walk(visit func(object) error) error {
	f, err := os.Open(v.path)
	if err != nil {
		return ErrRead
	}
	defer f.Close()
	h := sha256.New()
	d := json.NewDecoder(io.TeeReader(io.LimitReader(f, 16<<20+1), h))
	count := 0
	var total int64
	for {
		var o object
		err := d.Decode(&o)
		if err == io.EOF {
			var actual [32]byte
			copy(actual[:], h.Sum(nil))
			if count != v.count || total != v.bytes || actual != v.digest {
				return ErrChanged
			}
			return nil
		}
		if err != nil {
			return ErrRead
		}
		count++
		if count > MaxObjects || o.IsDir || !safePath(o.Path) || o.Size < 0 || o.Size > MaxObjectBytes || o.Size > MaxTotalBytes-total {
			return ErrChanged
		}
		total += o.Size
		if err = visit(o); err != nil {
			return err
		}
	}
}

func inventoryDigest(path string) ([32]byte, error) {
	var result [32]byte
	f, err := os.Open(path)
	if err != nil {
		return result, ErrRead
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, 16<<20+1))
	if err != nil {
		return result, ErrRead
	}
	if n > 16<<20 {
		return result, ErrLimit
	}
	copy(result[:], h.Sum(nil))
	return result, nil
}

func streamListing(ctx context.Context, source string, execute command, visit func(object) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		err := execute(ctx, &boundedWriter{writer, maxListingBytes}, "lsjson", source, "--recursive", "--files-only", "--no-modtime", "--no-mimetype")
		_ = writer.CloseWithError(err)
		done <- err
	}()
	d := json.NewDecoder(reader)
	token, err := d.Token()
	if err != nil || token != json.Delim('[') {
		return ErrRead
	}
	for d.More() {
		var o object
		if d.Decode(&o) != nil {
			return ErrRead
		}
		if err := visit(o); err != nil {
			return err
		}
	}
	token, err = d.Token()
	if err != nil || token != json.Delim(']') {
		return ErrRead
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrRead
	}
	if <-done != nil {
		return ErrRead
	}
	return nil
}

func lessObject(a, b object, fold bool) int {
	if fold {
		return strings.Compare(foldPath(a.Path), foldPath(b.Path))
	}
	return strings.Compare(a.Path, b.Path)
}

func foldPath(path string) string {
	return strings.Map(func(r rune) rune {
		minimum := r
		for c := unicode.SimpleFold(r); c != r; c = unicode.SimpleFold(c) {
			if c < minimum {
				minimum = c
			}
		}
		return minimum
	}, path)
}

func writeChunk(root string, objects []object, fold bool) (string, error) {
	slices.SortFunc(objects, func(a, b object) int { return lessObject(a, b, fold) })
	f, err := os.CreateTemp(root, "run-")
	if err != nil {
		return "", ErrRead
	}
	enc := json.NewEncoder(f)
	for _, o := range objects {
		if enc.Encode(o) != nil {
			f.Close()
			return "", ErrRead
		}
	}
	if f.Close() != nil {
		return "", ErrRead
	}
	return f.Name(), nil
}

type mergeItem struct {
	o       object
	decoder *json.Decoder
	file    *os.File
}
type objectHeap struct {
	items []*mergeItem
	fold  bool
}

func (h objectHeap) Len() int           { return len(h.items) }
func (h objectHeap) Less(i, j int) bool { return lessObject(h.items[i].o, h.items[j].o, h.fold) < 0 }
func (h objectHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *objectHeap) Push(v any)        { h.items = append(h.items, v.(*mergeItem)) }
func (h *objectHeap) Pop() any {
	i := len(h.items) - 1
	v := h.items[i]
	h.items = h.items[:i]
	return v
}

func mergeRuns(ctx context.Context, paths []string, destination string, fold bool) error {
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrRead
	}
	defer out.Close()
	files := []*os.File{}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	h := &objectHeap{fold: fold}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return ErrRead
		}
		files = append(files, f)
		item := &mergeItem{decoder: json.NewDecoder(f), file: f}
		if err := item.decoder.Decode(&item.o); err != nil {
			return ErrRead
		}
		heap.Push(h, item)
	}
	enc := json.NewEncoder(out)
	previous := ""
	for h.Len() > 0 {
		if ctx.Err() != nil {
			return ErrRead
		}
		item := heap.Pop(h).(*mergeItem)
		if fold && previous != "" && strings.EqualFold(previous, item.o.Path) {
			return ErrPath
		}
		if fold && previous != "" {
			before, after := strings.Split(previous, "/"), strings.Split(item.o.Path, "/")
			for i := 0; i < len(before) && i < len(after); i++ {
				if !strings.EqualFold(before[i], after[i]) {
					break
				}
				if before[i] != after[i] {
					return ErrPath
				}
			}
		}
		if !fold && previous != "" && strings.HasPrefix(item.o.Path, previous+"/") {
			return ErrPath
		}
		previous = item.o.Path
		if enc.Encode(item.o) != nil {
			return ErrRead
		}
		err := item.decoder.Decode(&item.o)
		if err == nil {
			heap.Push(h, item)
		} else if err != io.EOF {
			return ErrRead
		}
	}
	if out.Close() != nil {
		return ErrRead
	}
	return nil
}

func makeInventory(ctx context.Context, source, root string, execute command) (inventory, error) {
	result := inventory{path: filepath.Join(root, "inventory.jsonl")}
	var paths []string
	chunk := make([]object, 0, inventoryChunk)
	flush := func(fold bool) error {
		if len(chunk) == 0 {
			return nil
		}
		p, err := writeChunk(root, chunk, fold)
		if err != nil {
			return err
		}
		paths = append(paths, p)
		chunk = chunk[:0]
		return nil
	}
	err := streamListing(ctx, source, execute, func(o object) error {
		if ctx.Err() != nil {
			return ErrRead
		}
		result.count++
		if result.count > MaxObjects {
			return ErrLimit
		}
		if o.IsDir || !safePath(o.Path) {
			return ErrPath
		}
		if o.Size < 0 || o.Size > MaxObjectBytes || o.Size > MaxTotalBytes-result.bytes {
			return ErrLimit
		}
		result.bytes += o.Size
		chunk = append(chunk, o)
		if len(chunk) == inventoryChunk {
			return flush(true)
		}
		return nil
	})
	if err != nil {
		return inventory{}, err
	}
	if err = flush(true); err != nil {
		return inventory{}, err
	}
	folded := filepath.Join(root, "folded.jsonl")
	if err = mergeRuns(ctx, paths, folded, true); err != nil {
		return inventory{}, err
	}
	foldDigest, err := inventoryDigest(folded)
	if err != nil {
		return inventory{}, err
	}
	for _, p := range paths {
		if os.Remove(p) != nil {
			return inventory{}, ErrCleanup
		}
	}
	paths = nil
	if err = (inventory{path: folded, count: result.count, bytes: result.bytes, digest: foldDigest}).walk(func(o object) error {
		chunk = append(chunk, o)
		if len(chunk) == inventoryChunk {
			return flush(false)
		}
		return nil
	}); err != nil {
		return inventory{}, err
	}
	if err = flush(false); err != nil {
		return inventory{}, err
	}
	if err = mergeRuns(ctx, paths, result.path, false); err != nil {
		return inventory{}, err
	}
	for _, p := range append(paths, folded) {
		if os.Remove(p) != nil {
			return inventory{}, ErrCleanup
		}
	}
	result.digest, err = inventoryDigest(result.path)
	if err != nil {
		return inventory{}, err
	}
	return result, nil
}

func sameInventory(a, b inventory) bool {
	return a.count == b.count && a.bytes == b.bytes && a.digest == b.digest
}
