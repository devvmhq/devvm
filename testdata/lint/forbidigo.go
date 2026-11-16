// Package lint breaks each of .golangci.yml's forbidigo rules once, on a
// line ending in "// want forbidigo". mise run lint:code checks that
// forbidigo reports those lines and no others.
package lint

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"syscall"
	"time"
	"unicode"

	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/testbuild"
)

func exits() { os.Exit(1) } // want forbidigo

func prints() { fmt.Println("x") } // want forbidigo

func writes() { _, _ = os.Stdout.WriteString("x") } // want forbidigo

func contexts() context.Context { return context.Background() } // want forbidigo

func cpus() int { return runtime.NumCPU() } // want forbidigo

func testBuild() bool { return testbuild.On() } // want forbidigo

func signals(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) } // want forbidigo

func finds(pid int) (*os.Process, error) { return os.FindProcess(pid) } // want forbidigo

func kills(p *os.Process) error { return p.Kill() } // want forbidigo

func signalsProcess(p *os.Process) error { return p.Signal(os.Interrupt) } // want forbidigo

func client() any { return &http.Client{} } // want forbidigo

func transport() any { return &http.Transport{} } // want forbidigo

func home() (string, error) { return os.UserHomeDir() } // want forbidigo

func writesFile() error { return os.WriteFile("f", nil, 0o600) } // want forbidigo

func creates() (*os.File, error) { return os.Create("f") } // want forbidigo

func truncates() int { return os.O_WRONLY | os.O_TRUNC } // want forbidigo

func reads() ([]byte, error) { return os.ReadFile("f") } // want forbidigo

func setsEnv() error { return os.Setenv("SSH_AUTH_SOCK", "/s") } // want forbidigo

func unsetsEnv() error { return os.Unsetenv("SSH_AUTH_SOCK") } // want forbidigo

func shows(r rune) bool { return unicode.IsGraphic(r) } // want forbidigo

func private(a netip.Addr) bool { return a.IsPrivate() } // want forbidigo

func reveals(s redact.Secret) string { return s.Reveal() } // want forbidigo

func chmods() error { return os.Chmod("f", 0o600) } // want forbidigo

func chtimes(t time.Time) error { return os.Chtimes("f", t, t) } // want forbidigo

func chowns() error { return os.Chown("f", 0, 0) } // want forbidigo

func lchowns() error { return os.Lchown("f", 0, 0) } // want forbidigo

func truncatesPath() error { return os.Truncate("f", 0) } // want forbidigo

// Changes through what opened the file are what the rule asks for.
func changesOpen(f *os.File, r *os.Root) error {
	return errors.Join(f.Chmod(0o600), f.Chown(0, 0), f.Truncate(0), r.Chmod("f", 0o600))
}
