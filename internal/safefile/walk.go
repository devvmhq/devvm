package safefile

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// ErrTooMany is Walk's error once it has read more entries than its Bound.
var ErrTooMany = errors.New("more entries than the walk reads")

// Bound is a walk's bound on the entries it reads: Seen so far, Max at most. Walks that share
// one Bound are bounded together.
type Bound struct{ Max, Seen int }

// walkBatch is how many entries Walk reads of a folder at a time.
const walkBatch = 256

// walkMaxDepth and walkMaxPath bound how deep Walk goes below its root
// and how long a path it makes: each folder is opened by its path from
// the root, one name at a time, and its path kept, so a chain of folders
// that a VM nests deep would cost each open and each path its depth, and
// the walk the square of it.
const (
	walkMaxDepth = 128
	walkMaxPath  = 4096
)

// Walk walks the tree at root in fsys as fs.WalkDir does, in lexical order, giving fn each entry
// and taking SkipDir and SkipAll as it does. But it reads each folder walkBatch entries at a
// time, counting each entry it reads, the root's own included, in b, and once b.Seen passes
// b.Max it stops with ErrTooMany, before fn sees any of that folder's entries. So a folder that
// a VM filled with millions of entries costs a walk no more than b.Max entries and a batch,
// where fs.WalkDir reads a folder whole before it looks at any bound. It stops with ErrTooMany
// too at a folder more than walkMaxDepth below root, or a path longer than walkMaxPath. fsys
// opens a folder as an fs.ReadDirFile, as os.DirFS, an os.Root's FS and a Root's FS do. It
// stops with ctx's error once ctx ends, as at Ctrl-C or a deadline, before it reads a folder or
// its next batch.
func Walk(ctx context.Context, fsys fs.FS, root string, b *Bound, fn fs.WalkDirFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := fs.Stat(fsys, root)
	if err != nil {
		err = fn(root, nil, err)
	} else if b.Seen++; b.Seen > b.Max {
		return ErrTooMany
	} else {
		err = walk(ctx, fsys, root, fs.FileInfoToDirEntry(info), 0, b, fn)
	}
	if errors.Is(err, fs.SkipDir) || errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

// walk is Walk below its root: fn on the entry d at name, depth below the root, then on what's
// in it when it's a folder.
func walk(ctx context.Context, fsys fs.FS, name string, d fs.DirEntry, depth int, b *Bound, fn fs.WalkDirFunc) error {
	if depth > walkMaxDepth || len(name) > walkMaxPath {
		return ErrTooMany
	}
	if err := fn(name, d, nil); err != nil || !d.IsDir() {
		if errors.Is(err, fs.SkipDir) && d.IsDir() {
			err = nil // what's in it is skipped, and the walk goes on
		}
		return err
	}
	es, err := readDir(ctx, fsys, name, b)
	if errors.Is(err, ErrTooMany) {
		return err
	} else if done := ctx.Err(); done != nil {
		return done
	} else if err != nil {
		// As fs.WalkDir does: fn on the folder again, with the error.
		if err = fn(name, d, err); err != nil {
			if errors.Is(err, fs.SkipDir) {
				err = nil
			}
			return err
		}
	}
	for _, e := range es {
		if err := walk(ctx, fsys, path.Join(name, e.Name()), e, depth+1, b, fn); err != nil {
			if errors.Is(err, fs.SkipDir) {
				break // the rest of this folder skipped
			}
			return err
		}
	}
	return nil
}

// readDir reads the folder name in fsys a batch at a time, counting each entry in b, and returns
// its entries sorted by name, or ErrTooMany once b.Seen passes b.Max, or ctx's error once ctx
// ends.
func readDir(ctx context.Context, fsys fs.FS, name string, b *Bound) ([]fs.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // only read
	d, ok := f.(fs.ReadDirFile)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errors.New("not a folder that can be read")}
	}
	var es []fs.DirEntry
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := d.ReadDir(walkBatch)
		if b.Seen += len(batch); b.Seen > b.Max {
			return nil, ErrTooMany
		}
		es = append(es, batch...)
		if errors.Is(err, io.EOF) || err == nil && len(batch) == 0 {
			break
		} else if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(es, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return es, nil
}
