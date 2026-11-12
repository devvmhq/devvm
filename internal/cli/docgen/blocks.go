package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/cli"
	"github.com/devvmhq/devvm/internal/state"
)

// filled returns page with b's text between its markers, which the page
// must have once each, in order.
func filled(page string, b cli.DocBlock) (string, error) {
	text, err := b.Text(assets.FS)
	if err != nil {
		return "", err
	}
	before, rest, ok := strings.Cut(page, b.Begin())
	_, after, ok2 := strings.Cut(rest, b.End())
	if !ok || !ok2 || strings.Contains(after, b.Begin()) {
		return "", fmt.Errorf("%s: no %q and %q around %s, once each", b.Page, b.Begin(), b.End(), b.Name)
	}
	return before + b.Begin() + text + b.End() + after, nil
}

// blocks fills every generated block of the pages below root, the repo's
// root, writing a page only when its block changed, and returns the pages
// that changed. With check, it writes none.
func blocks(root string, check bool) ([]string, error) {
	var stale []string
	for _, b := range cli.DocBlocks() {
		path := filepath.Join(root, filepath.FromSlash(b.Page))
		data, err := os.ReadFile(path) //nolint:gosec // a page of the developer's repo
		if err != nil {
			return nil, err
		}
		page, err := filled(string(data), b)
		if err != nil {
			return nil, err
		}
		if page == string(data) {
			continue
		}
		stale = append(stale, b.Page)
		if !check {
			if err := state.WriteFileMode(path, []byte(page), 0o644); err != nil {
				return nil, err
			}
		}
	}
	return stale, nil
}
