// Command devvm-immutable sets or clears Linux's immutable attribute
// (FS_IMMUTABLE_FL, chattr +i) on the user's own files, for devvm's .git
// protection on Linux hosts. It's a separate program, installed root-owned
// with the one file capability CAP_LINUX_IMMUTABLE, which setting that
// attribute needs, so it does as little as it can:
//
//	devvm-immutable check            exit 0 when it holds CAP_LINUX_IMMUTABLE, printing its protocol version
//	devvm-immutable set PATH...      set the attribute on each PATH and everything under it
//	devvm-immutable clear PATH...    clear it on each PATH and everything under it
//
// Each PATH is absolute and clean. It's reached from / one folder at a
// time, never through a symlink, and acted on only when it's a regular
// file or a folder that the user who ran it owns, through the file it
// opened. set refuses a file with other names (hard links), which may be
// outside the folder. Inside a folder, symlinks are skipped and other file
// systems aren't entered, and a walk is bounded. Only the immutable
// attribute changes: the others are written back as they were. It gives
// up every other capability first, takes nothing from its environment and
// runs no other program.
package main

import "os"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }
