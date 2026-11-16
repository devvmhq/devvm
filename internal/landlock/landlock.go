// Package landlock confines a process on Linux to the folders it needs, as a VM's proxy's child
// confines itself (Self) once it has opened its logs and the Nix cache, before it reads a byte of
// the VM's: no_new_privs on every thread, Landlock on the files it may read and write, and a
// seccomp filter on the system calls that would reach a unix socket, another process or around
// the filter. Where the kernel lacks Landlock or seccomp, it leaves that part out, and its Result
// says what it got, which the proxy turns into its confinement's report (proxy.Confinement).
//
// The proxy imports this package, so it's the standard library alone. Its code is Linux's alone,
// behind build tags here, not in internal/platform, which the proxy can't import, as safefile's
// flags files are.
package landlock

// Floor is the oldest Landlock ABI the child confines its files with: ABI 2, whose REFER lets the
// Nix cache rename a file from its top folder into nar/. A kernel below it confines no files.
const Floor = 2

// The Landlock ABIs from which a kernel refuses more: a TCP port's bind (ABI 4), a signal to a
// process outside the child and an abstract unix socket not its own (ABI 6), and a named unix
// socket, by Landlock itself (ABI 9).
const (
	ABIPorts   = 4
	ABISignals = 6
	ABIUnix    = 9
)

// Result is what Self confined a process with: Landlock at its ABI (0: none), whether that scopes
// its signals (from ABI 6, but at ABI 6 or 7 only on a kernel with the fix for signals between a
// process's threads), and the seccomp filter.
type Result struct {
	ABI           int
	SignalsScoped bool
	Seccomp       bool
}
