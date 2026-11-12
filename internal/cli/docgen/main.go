// Command docgen writes the CLI reference, docs/content/docs/cli/, from
// devvm's commands: one Hugo page per command users can run, and the
// section's _index.md. It also writes the hand-written pages' generated
// blocks (cli.DocBlocks), such as the docs' table of the agents. Run it
// with mise run cli-docs, which gives it the repo's root.
package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/cli"
	"github.com/devvmhq/devvm/internal/ui"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: docgen ROOT")
	}
	if err := run(os.Args[1]); err != nil {
		log.Fatal(err)
	}
}

// run writes the reference and the pages' generated blocks below root, the
// repo's root.
func run(root string) error {
	if err := write(newRoot(), filepath.Join(root, "docs", "content", "docs", "cli")); err != nil {
		return err
	}
	_, err := blocks(root, false)
	return err
}

// newRoot is devvm's command, for its pages only: none of it runs, so its
// UI has no input and prints nowhere.
func newRoot() *cobra.Command {
	return cli.NewRoot(ui.New(strings.NewReader(""), io.Discard, io.Discard, func(string) string { return "" }))
}
