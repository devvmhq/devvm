package lima

import (
	"context"
	"errors"
	"io"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/vmname"
)

// sendPackages unpacks what p.Send writes into the build VM's
// backend.PackagesDir, as root, emptied first: before the recipe's steps.
func sendPackages(ctx context.Context, l *Lima, vm vmname.VM, p *backend.Packages) error {
	if p == nil || p.Send == nil {
		return nil
	}
	d := shell.Quote(backend.PackagesDir)
	pr, pw := io.Pipe()
	sent := make(chan error, 1)
	go func() {
		err := p.Send(pw)
		_ = pw.CloseWithError(err)
		sent <- err
	}()
	err := l.Exec(ctx, vm, []string{"sudo", "sh", "-c", "rm -rf " + d + " && mkdir -p " + d + " && tar -x -C " + d}, backend.ExecOpts{Stdin: pr})
	_ = pr.CloseWithError(errors.New("lima: the build VM stopped reading its packages"))
	if serr := <-sent; serr != nil {
		return serr
	}
	if err != nil {
		return msg.PackagesSendFailed(vm).Because(err)
	}
	return nil
}

// takePackages hands p.Take a tar stream of the build VM's
// backend.PackagesDir/out, made as root, then deletes PackagesDir: after
// the recipe's steps, so the image keeps none of it.
func takePackages(ctx context.Context, l *Lima, vm vmname.VM, p *backend.Packages) error {
	if p == nil {
		return nil
	}
	d := shell.Quote(backend.PackagesDir)
	if p.Take != nil {
		pr, pw := io.Pipe()
		taken := make(chan error, 1)
		go func() {
			err := p.Take(pr)
			// What's left unread is drained, so the VM's tar ends.
			_, _ = io.Copy(io.Discard, pr)
			taken <- err
		}()
		err := l.Exec(ctx, vm, []string{"sudo", "sh", "-c", "mkdir -p " + d + "/out && tar -c -C " + d + "/out ."}, backend.ExecOpts{Stdout: pw})
		_ = pw.CloseWithError(err)
		if terr := <-taken; terr != nil {
			return terr
		}
		if err != nil {
			return msg.PackagesTakeFailed(vm).Because(err)
		}
	}
	if err := l.Exec(ctx, vm, []string{"sudo", "rm", "-rf", backend.PackagesDir}, backend.ExecOpts{}); err != nil {
		return msg.PackagesTakeFailed(vm).Because(err)
	}
	return nil
}
