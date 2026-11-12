//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

// imagesSection: devvm doctor, base's build and its record, a fixture
// layer built, edited, rebuilt, listed and deleted; the user's CAs in a
// build and out of its image, Java's cacerts too; the host's proxy kept
// out of a build's image; a failed and an interrupted build; base built
// again from its package cache with the mirror unreachable.
func imagesSection(s *S) {
	s.Section("images")
	kb := imagesBase(s)
	kb = imagesFixture(s, kb)
	imagesCA(s)
	imagesJava(s)
	imagesProxy(s)
	imagesFailed(s, kb)
	imagesPackages(s, kb)
	imagesGolden(s, kb)
}

// imagesBase checks doctor and base's build, and is base's key when this
// run built it.
func imagesBase(s *S) string {
	s.Check("devvm doctor finds everything it needs on this host", func() error {
		if _, err := s.Devvm("", "doctor"); err != nil {
			return err
		}
		r, err := s.Devvm("", "doctor", "--json")
		if err != nil {
			return err
		}
		if !HasLine(r.Stdout, `  "problems": [],`) {
			return errors.New("devvm doctor --json lists problems")
		}
		return nil
	})
	// base stays cached between runs (--clean deletes it); the fixture layers' images are this run's.
	var built Res
	s.Check("build base", func() error {
		var err error
		built, err = s.DevvmNames("", "build", "base")
		return err
	}, Builds())
	// Its key: the build's, else the newest base from Ubuntu's image (from's bases are cached too).
	kb := CaptureLine(built.Out, `^==> building_image image=base key=([0-9a-f]*)$`)
	// A newest base pins the newest release and boots from it: a fallback to the release before
	// (origin_no_boot) passes for a user, but is the drift this run looks for.
	if s.sb.o.newest {
		s.Check("base pinned Ubuntu's newest cloud image and booted from it, with no fallback to the release before", func() error {
			release, err := baseRelease(s)
			switch {
			case err != nil:
				return err
			case !HasLine(built.Out, "==> origin_pinned origin="+release):
				return errors.New("base didn't pin " + release)
			case strings.Contains(built.Out, "origin_no_boot"):
				return errors.New("base fell back to the release before")
			}
			return nil
		})
	}
	switch {
	case kb != "" && s.sb.o.every:
		runlog := CaptureLine(built.Out, `^ *log_details file=(.*)$`)
		s.Check("base ran base.sh, then every feature", func() error {
			b, err := os.ReadFile(runlog)
			if err != nil {
				return err
			}
			for _, f := range append([]string{"base"}, features(s)...) {
				if !Matches(string(b), `^# \[[0-9]*/[0-9]*\] running_script script=`+regexp.QuoteMeta(f)+`\.sh vm=devvm-img-`+kb+`$`) {
					return errors.New("base's build didn't run " + f + ".sh")
				}
			}
			return nil
		})
	case s.sb.o.every:
		s.Skip("base ran base.sh, then every feature", "base is cached; from checks the features")
	}
	return kb
}

// features is every feature of base's, as config.toml chooses them in the
// features tier.
func features(s *S) []string {
	m, _ := filepath.Glob(filepath.Join(s.sb.root, "assets", "features", "*.sh"))
	var fs []string
	for _, f := range m {
		fs = append(fs, strings.TrimSuffix(filepath.Base(f), ".sh"))
	}
	slices.Sort(fs)
	return fs
}

// baseRelease is the release of Ubuntu's cloud image base is to pin:
// e2eUbuntuRelease, or with --newest-base the newest Ubuntu lists now.
func baseRelease(s *S) (string, error) {
	if !s.sb.o.newest {
		return e2eUbuntuRelease, nil
	}
	return listedRelease(s.r.window().ctx)
}

// listedRelease is the newest release Ubuntu lists of its cloud images,
// read as scripts/bump.sh e2e-ubuntu reads it.
func listedRelease(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloud-images.ubuntu.com/releases/resolute/", nil)
	if err != nil {
		return "", err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	rs := regexp.MustCompile(`release-[0-9]{8}`).FindAllString(string(b), -1)
	if res.StatusCode != http.StatusOK || len(rs) == 0 {
		return "", errors.New("couldn't read the newest release from Ubuntu's listing of its cloud images")
	}
	return slices.Max(rs), nil
}

// pinned is a field of the cloud image pinned in the sandbox, as devvm
// recorded it (images/ubuntu.json).
func pinned(s *S, field string) string {
	b, err := os.ReadFile(filepath.Join(s.Paths.Images, "ubuntu.json"))
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	v, _ := m[field].(string)
	return v
}

// first is the first of keys, nothing when there's none.
func first(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// imagesFixture builds a fixture layer on base, edits, rebuilds, lists and
// deletes it, and is base's key (the fixture's base's, when base is
// cached).
func imagesFixture(s *S, kb string) string {
	fixture := filepath.Join(s.Paths.Layers, "e2efix.sh")
	policy := filepath.Join(s.Paths.Layers, "e2efix.policy")
	token := fmt.Sprintf("e2e-fixture-ran-%d-%d", os.Getpid(), time.Now().Unix()) // this run's: the run logs keep earlier runs'
	_ = os.Remove(fixture)
	_ = os.Remove(policy)
	madeFile(s.sb, fixture)
	madeFile(s.sb, policy)
	s.Check("layer new writes a starter layer", func() error {
		_, err := s.Devvm("", "layer", "new", "e2efix")
		return err
	})
	// The layer runs after devvm's prelude: it prints the architecture and C library the prelude
	// found, and the pin devvm_sha picks of two fake ones, all of which the check compares.
	_ = AppendFile(fixture, "e2efix_sha256_arm64=fake-arm64\ne2efix_sha256_amd64=fake-amd64\n"+
		`echo "`+token+` $devvm_arch $devvm_libc $(devvm_sha e2efix)"`+"\necho fixture | sudo tee /etc/e2e-fixture\n")
	// The VM's architecture is this machine's, which Go names as the pins do; base is Ubuntu's.
	ran := fmt.Sprintf("%s %s gnu fake-%s", token, runtime.GOARCH, runtime.GOARCH)
	s.Check("build the fixture layer on base", func() error {
		_, err := s.Devvm("", "build", "e2efix")
		return err
	})
	s.Own("e2efix")
	kf := first(s.Paths.KeysOf("e2efix"))
	// A cached base's key: the one the fixture was built on (other selections' bases, with other
	// features, are cached too, and may be newer).
	if kb == "" {
		kb = CaptureLine(s.Paths.Record(kf, "chain"), `^base=([0-9a-f]*) e2efix=.*$`)
	}
	imagesRecord(s, kb)
	s.Check("its script ran in its image, cloned from base's", func() error {
		switch {
		case kf == "" || s.Status("devvm-img-"+kf) != "Stopped":
			return errors.New("the fixture's image isn't there, stopped")
		case s.Paths.Record(kf, "chain") != "base="+kb+" e2efix="+kf:
			return errors.New("the fixture's chain isn't base's and its own")
		case !s.Paths.RunLogHas(ran):
			return fmt.Errorf("no run log has the fixture's output, %q: the prelude's names, before it", ran)
		}
		return nil
	})
	_ = AppendFile(fixture, "# edited\n")
	s.Check("editing it builds a new image; base and the old one stay", func() error {
		if _, err := s.Devvm("", "build", "e2efix"); err != nil {
			return err
		}
		switch {
		case len(s.Paths.KeysOf("e2efix")) != 2:
			return errors.New("there aren't two e2efix images")
		case s.Status("devvm-img-"+kf) != "Stopped" || !Exists(filepath.Join(s.Paths.Images, kb, "record.json")):
			return errors.New("the old image or base's record is gone")
		}
		return nil
	})
	s.Own("e2efix")
	s.Check("rebuild makes a new one and deletes the stale ones, not base", func() error {
		if _, err := s.Devvm("", "rebuild", "--force", "e2efix"); err != nil {
			return err
		}
		keys := s.Paths.KeysOf("e2efix")
		rebuilds, _ := os.ReadFile(filepath.Join(s.Paths.Images, "rebuilds.json"))
		switch {
		case len(keys) != 1 || keys[0] == kf || s.Status("devvm-img-"+kf) != "":
			return errors.New("there isn't one new e2efix image alone")
		case s.Status("devvm-img-"+kb) != "Stopped":
			return errors.New("base isn't there, stopped")
		case !strings.Contains(string(rebuilds), `"name": "e2efix"`):
			return errors.New("rebuilds.json doesn't record e2efix")
		}
		return nil
	})
	s.Own("e2efix")
	imagesList(s, kb, first(s.Paths.KeysOf("e2efix")))
	return kb
}

// imagesRecord checks base's record, its mounts, devvm limactl, and that
// building it again reuses it.
func imagesRecord(s *S, kb string) {
	s.Check("base is a stopped image, recorded with Ubuntu's pinned cloud image and its C library", func() error {
		release, err := baseRelease(s)
		if err != nil {
			return err
		}
		rec := func(f string) string { return s.Paths.Record(kb, f) }
		switch {
		case kb == "" || s.Status("devvm-img-"+kb) != "Stopped":
			return errors.New("base isn't there, stopped")
		case rec("chain") != "base="+kb || rec("libc") != "gnu" || rec("from") != "":
			return errors.New("base's record has another chain, C library or --from")
		case !Matches(rec("built"), `^[0-9-]+T[0-9:]+Z$`) || pinned(s, "release") != release:
			return errors.New("base's record has no build time, or another release is pinned")
		case rec("digest") != "https://cloud-images.ubuntu.com/releases/resolute/"+release+"/"+pinned(s, "file")+" "+pinned(s, "digest"):
			return errors.New("base's record has another cloud image")
		case !Matches(pinned(s, "digest"), `^sha256:[0-9a-f]{64}$`):
			return errors.New("the pinned cloud image has no SHA-256")
		}
		return nil
	})
	s.Check("base mounts nothing from the host", func() error {
		if kb == "" {
			return errNo
		}
		if m := MountLine(s, "devvm-img-"+kb); m != "" {
			return errors.New("base mounts " + m)
		}
		return nil
	})
	s.Check("devvm limactl runs Lima on devvm's Lima home, whatever LIMA_HOME says, with its exit code", func() error {
		if kb == "" {
			return errNo
		}
		r, err := s.Run(Cmd{Args: []string{s.sb.devvm, "limactl", "list", "-q"}, Env: []string{"LIMA_HOME=" + filepath.Join(s.sb.work, "no-lima")}})
		if err != nil {
			return err
		}
		if !HasLine(r.Stdout, "devvm-img-"+kb) {
			return errors.New("devvm limactl list doesn't list base")
		}
		if _, err := s.Devvm("", "limactl", "e2e-no-such-command"); err == nil {
			return errors.New("devvm limactl e2e-no-such-command succeeded")
		}
		return nil
	})
	s.Check("building base again reuses it", func() error {
		r, err := s.Devvm("", "build", "base")
		if err != nil {
			return err
		}
		if strings.Contains(r.Out, s.Msg("building_image", "image=base", "key="+kb)) {
			return errors.New("base was built again")
		}
		return nil
	})
}

// imagesList checks devvm images and images rm, kf the fixture's key.
func imagesList(s *S, kb, kf string) {
	dg := CaptureLine(s.Paths.Record(kb, "digest"), `^.* sha256:([0-9a-f]{12})[0-9a-f]*$`)
	s.Check("devvm images lists base and the fixture, current, with Ubuntu's release, digest and size", func() error {
		r, err := s.Devvm("", "images")
		if err != nil {
			return err
		}
		switch {
		case !Matches(r.Stdout, `^base +ubuntu [0-9.]+ \([0-9]{8}\) +`+dg+` +`+kb+` +current +[0-9.]+[KMGT]$`):
			return errors.New("devvm images doesn't list base, current")
		case !Matches(r.Stdout, `^e2efix +ubuntu [0-9.]+ \([0-9]{8}\) +`+dg+` +`+kf+` +current +[0-9.]+[KMGT]$`):
			return errors.New("devvm images doesn't list e2efix, current")
		case !Matches(r.Stdout, `^NAME +TOOL +VERSION +LIBC +SIZE +VMS +STATE$`):
			return errors.New("devvm images has no tool images' heading")
		}
		return nil
	})
	s.Check("images rm deletes an image by name, with its record, not base", func() error {
		if kf == "" {
			return errNo
		}
		if _, err := s.Devvm("", "images", "rm", "--force", "e2efix"); err != nil {
			return err
		}
		switch {
		case s.Status("devvm-img-"+kf) != "" || Exists(filepath.Join(s.Paths.Images, kf)):
			return errors.New("e2efix or its record is still there")
		case s.Status("devvm-img-"+kb) != "Stopped":
			return errors.New("base isn't there, stopped")
		}
		return nil
	})
}

// buildScript is a layer's script: bash, failing on the first error, its
// lines, then echo token.
func buildScript(token string, lines ...string) string {
	return "#!/bin/bash\nset -euo pipefail\n" + strings.Join(lines, "\n") + "\necho " + token + "\n"
}

// runToken is a token of this run's, after prefix.
func runToken(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), time.Now().Unix())
}

// checkVM makes name a copy of the image with key, booted without devvm
// (no setup), recorded for the run's cleanup: what the image holds.
func checkVM(s *S, key, name string) error {
	if key == "" {
		return errNo
	}
	if _, err := s.Limactl("clone", "--tty=false", "--mount-none", "devvm-img-"+key, name); err != nil {
		return err
	}
	_, err := s.Limactl("start", "--tty=false", name)
	return err
}

// imagesCA: the user's CAs. A build trusts them while its scripts run,
// with the variables for tools that don't read the system's store, and
// its image keeps none of them, so its key is the same without them. A
// copy of the image, booted without devvm (no setup), shows what the
// image holds.
func imagesCA(s *S) {
	ca, caErr := NewCA(s)
	if caErr == nil {
		caErr = ca.Trust()
	}
	catoken := runToken("e2e-ca-trusted")
	lf, _ := s.Layer("e2eca.sh", buildScript(catoken,
		`[[ $NODE_EXTRA_CA_CERTS == /etc/ssl/devvm.pem && $SSL_CERT_FILE == /etc/ssl/certs/ca-certificates.crt ]]`,
		`openssl verify -CAfile /etc/ssl/certs/ca-certificates.crt /etc/ssl/devvm.pem`))
	s.Check("a build trusts the CAs in devvm's CA folder while its scripts run", func() error {
		if caErr != nil {
			return caErr
		}
		if _, err := s.Devvm("", "build", "e2eca"); err != nil {
			return err
		}
		if !s.Paths.RunLogHas(catoken) {
			return errors.New("no run log has the layer's output")
		}
		return nil
	})
	s.Own("e2eca")
	kc := first(s.Paths.KeysOf("e2eca"))
	_ = os.RemoveAll(s.Paths.CAs)
	s.Check("its key is the same without them: nothing to build", func() error {
		if kc == "" {
			return errNo
		}
		r, err := s.Devvm("", "build", "e2eca")
		switch {
		case err != nil:
			return err
		case strings.Contains(r.Out, s.Msg("building_image", "image=e2eca", "key="+kc)):
			return errors.New("e2eca was built again")
		case !slices.Equal(s.Paths.KeysOf("e2eca"), []string{kc}):
			return errors.New("e2eca has another key")
		}
		return nil
	})
	s.sb.created = append(s.sb.created, "devvm-e2e-cacheck")
	s.Check("its image keeps none of them", func() error {
		if err := checkVM(s, kc, "devvm-e2e-cacheck"); err != nil {
			return err
		}
		if ca == nil {
			return caErr
		}
		return ca.GoneFrom(s, "devvm-e2e-cacheck")
	})
	_, _ = s.Limactl("delete", "-f", "devvm-e2e-cacheck")
	_ = os.Remove(lf)
}

// imagesJava: Java's cacerts. ca-certificates-java's hook adds the CAs a
// build trusts (debian:devvm-extra-N.pem) to it, and it's told of each
// one devvm takes out again, so the image's cacerts keeps none. A project
// VM on that image gets the CA in cacerts at its launch, and drops it at
// the launch after it's removed from devvm's CA folder.
func imagesJava(s *S) {
	ca, caErr := NewCA(s)
	if caErr == nil {
		caErr = ca.Trust()
	}
	jtoken := runToken("e2e-java-ca")
	jlist := `keytool -list -cacerts -storepass changeit 2>/dev/null | grep -i "^debian:devvm-extra-"`
	jnone := "keytool -list -cacerts -storepass changeit >/dev/null 2>&1 && ! { " + jlist + "; }"
	lf, _ := s.Layer("e2ejava.sh", buildScript(jtoken,
		`sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends default-jre-headless ca-certificates-java >/dev/null`,
		jlist))
	s.Check("a layer that installs Java has the CAs in Java's cacerts while it builds", func() error {
		if caErr != nil {
			return caErr
		}
		if _, err := s.Devvm("", "build", "e2ejava"); err != nil {
			return err
		}
		if !s.Paths.RunLogHas(jtoken) {
			return errors.New("no run log has the layer's output")
		}
		return nil
	})
	s.Own("e2ejava")
	kj := first(s.Paths.KeysOf("e2ejava"))
	s.sb.created = append(s.sb.created, "devvm-e2e-javacheck")
	inVM := func(vm, script string) error {
		_, err := s.Limactl("shell", "--workdir", "/", vm, "bash", "-c", script)
		return err
	}
	s.Check("its image's cacerts keeps none of them", func() error {
		if err := checkVM(s, kj, "devvm-e2e-javacheck"); err != nil {
			return err
		}
		return inVM("devvm-e2e-javacheck", jnone)
	})
	_, _ = s.Limactl("delete", "-f", "devvm-e2e-javacheck")
	if kj != "" {
		var vm string
		pj, err := s.NewProject("e2ejava")
		if err == nil {
			vm, _, _ = s.Launch(pj, "exit", "--network", "open", "--layers", "e2ejava")
		}
		s.Check("a project VM with Java has the CA in cacerts", func() error {
			if vm == "" {
				return errNo
			}
			return inVM(vm, jlist)
		})
		_ = os.RemoveAll(s.Paths.CAs)
		s.Check("removed from devvm's CA folder, it's gone from cacerts from the VM's next launch", func() error {
			if vm == "" {
				return errNo
			}
			if _, _, err := s.Launch(pj, "exit"); err != nil {
				return err
			}
			return inVM(vm, jnone)
		})
		if vm != "" {
			_, _ = s.Devvm(pj, "rm", "--force", "--no-export")
		}
	} else {
		s.Skip("a project VM with Java has the CA in cacerts", "the Java layer didn't build")
		s.Skip("removed from devvm's CA folder, it's gone from cacerts from the VM's next launch", "the Java layer didn't build")
	}
	_ = os.RemoveAll(s.Paths.CAs)
	_ = os.Remove(lf)
}

// imagesProxy: the host's proxy. A build goes through a proxy of devvm's
// for it alone, which alone has the host's settings. The build's commands
// get only that proxy's address with a password of the build's, sudo
// keeping them, and its image keeps none of it (Lima writes the settings
// it propagates into the guest's /etc/environment). The host's proxy is a
// closed port with a password of this run's, and NO_PROXY=* keeps every
// tool off the proxies, as the layer needs no network. Its disk and
// Lima's files for it are read wherever they have data, free space
// included, skipping only the disk's holes.
func imagesProxy(s *S) {
	pxpass := runToken("e2e-proxy-pw")
	pxtoken := runToken("e2e-proxy-seen")
	lf, _ := s.Layer("e2eproxy.sh", buildScript(pxtoken,
		`[[ $HTTPS_PROXY == http://devvm:*@192.168.5.2:* && $http_proxy == "$HTTPS_PROXY" && $(sudo printenv HTTPS_PROXY) == "$HTTPS_PROXY" ]]`,
		`[[ $HTTPS_PROXY != *e2e-proxy-pw-* && $NO_PROXY == "*" ]]`))
	builds := filepath.Join(s.Paths.Devvm, "run", "builds")
	s.Check("a build's commands get a proxy of devvm's for the build, never the host's own settings", func() error {
		host := "http://corp:" + pxpass + "@127.0.0.1:9"
		if _, err := s.Run(Cmd{Args: []string{s.sb.devvm, "build", "e2eproxy"}, Env: []string{"HTTPS_PROXY=" + host, "HTTP_PROXY=" + host, "NO_PROXY=*"}}); err != nil {
			return err
		}
		ps, err := psAll(s.r.window().ctx)
		if err != nil {
			return err
		}
		left, _ := filepath.Glob(filepath.Join(builds, "*", "run", "build-proxy"))
		switch {
		case !s.Paths.RunLogHas(pxtoken):
			return errors.New("no run log has the layer's output")
		case Matches(string(ps), `_proxy -config `+regexp.QuoteMeta(builds)+`/.*/build-proxy/proxy\.json`):
			return errors.New("the build's proxy still runs")
		case len(left) > 0:
			return errors.New("the build's proxy left its folder: " + strings.Join(left, " "))
		}
		return nil
	})
	s.Own("e2eproxy")
	kp := first(s.Paths.KeysOf("e2eproxy"))
	s.Check("its image keeps none of them: not its disk, Lima's files for it or devvm's logs", func() error {
		disk := filepath.Join(s.sb.lima, "devvm-img-"+kp)
		if fi, err := os.Stat(disk); kp == "" || err != nil || !fi.IsDir() {
			return errNo
		}
		return NoLeak([]string{pxpass}, disk, filepath.Join(s.Paths.Devvm, "logs"), filepath.Join(s.Paths.VMDir("devvm-img-"+kp), "logs"))
	})
	_ = os.Remove(lf)
}

// imagesFailed: a failed script keeps its image's VM, stopped and without
// a record, for inspection; Ctrl+C during a build deletes it.
func imagesFailed(s *S, kb string) {
	lf, _ := s.Layer("e2efail.sh", "#!/bin/bash\nexit 3\n")
	fail, err := s.DevvmNames("", "build", "e2efail")
	rc := ExitStatus(err)
	kx := CaptureLine(fail.Out, `^==> building_image image=e2efail key=([0-9a-f]*)$`)
	if kx != "" {
		s.sb.created = append(s.sb.created, "devvm-img-"+kx)
	}
	s.Check("a failed script keeps its VM, stopped, and says how to look inside", func() error {
		switch {
		case kx == "" || rc != 1:
			return fmt.Errorf("the build exited %d, its key %q", rc, kx)
		case !Matches(fail.Out, `^devvm: script_failed script=e2efail\.sh vm=devvm-img-`+kx+`$`):
			return errors.New("the build didn't say script_failed")
		case s.Status("devvm-img-"+kx) != "Stopped" || Exists(filepath.Join(s.Paths.Images, kx)):
			return errors.New("its VM isn't there, stopped, without a record")
		}
		return nil
	})
	s.Check("devvm images lists it as unfinished, and images rm deletes it by its key", func() error {
		if kx == "" {
			return errNo
		}
		r, err := s.Devvm("", "images")
		if err != nil {
			return err
		}
		if !Matches(r.Stdout, `^- +- +- +`+kx+` +unfinished +`) {
			return errors.New("devvm images doesn't list it as unfinished")
		}
		if _, err := s.Devvm("", "images", "rm", "--force", kx); err != nil {
			return err
		}
		if s.Status("devvm-img-"+kx) != "" {
			return errors.New("its VM is still there")
		}
		return nil
	})
	_ = os.Remove(lf)
	imagesInterrupted(s, kb)
}

// imagesInterrupted: Ctrl+C during a build. The run's names
// (DEVVM_MESSAGES) say which image it builds, its run log, and (in that
// log) when its script runs.
func imagesInterrupted(s *S, kb string) {
	lf, _ := s.Layer("e2eslow.sh", "#!/bin/bash\nsleep 600\n")
	rc := -1
	var out string
	p, err := s.Start(Cmd{Args: []string{s.sb.devvm, "build", "e2eslow"}, Env: []string{"DEVVM_MESSAGES=names"}})
	if err == nil {
		_ = s.Until(300*time.Second, func() error {
			runlog := CaptureLine(p.Out(), `^ *log_details file=(.*)$`)
			if b, err := os.ReadFile(runlog); runlog != "" && err == nil && Matches(string(b), `^# \[[0-9]+/[0-9]+\] running_script script=e2eslow\.sh `) {
				return nil
			}
			return errors.New("the script isn't running yet")
		})
		rc = interrupt(p)
		out = p.Out()
	}
	ks := CaptureLine(out, `^==> building_image image=e2eslow key=([0-9a-f]*)$`)
	if ks != "" {
		s.sb.created = append(s.sb.created, "devvm-img-"+ks)
	}
	s.Check("Ctrl+C during a build deletes the unfinished image, and exits 130", func() error {
		switch {
		case ks == "" || rc != 130:
			return fmt.Errorf("the build exited %d, its key %q", rc, ks)
		case !HasLine(out, "==> interrupted_image vm=devvm-img-"+ks):
			return errors.New("the build didn't say interrupted_image")
		case s.Status("devvm-img-"+ks) != "" || Exists(filepath.Join(s.Paths.Images, ks)):
			return errors.New("the unfinished image or its record is still there")
		case s.Status("devvm-img-"+kb) != "Stopped":
			return errors.New("base isn't there, stopped")
		}
		return nil
	})
	_ = os.Remove(filepath.Join(s.Paths.Layers, "e2efix.sh"))
	_ = os.Remove(lf)
}

// interrupt sends p INT, as Ctrl+C does, p alone and not its group, and
// is its exit status once it ends (-1 when it doesn't within 5 minutes,
// and is stopped).
func interrupt(p *Proc) int {
	_ = p.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-p.done:
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(5 * time.Minute):
		p.Stop()
		return -1
	}
}

// imagesPackages: base's packages. Its first build recorded each package
// apt downloaded, kept in devvm's cache under its SHA-256. Built again
// under the same key, with the mirror set to an address nothing answers
// on, it installs them from the cache alone: apt reads only the build's
// own folder.
func imagesPackages(s *S, kb string) {
	packages := filepath.Join(s.Paths.Devvm, "packages")
	s.Check("base recorded its packages, each in the cache under its own SHA-256", func() error {
		b, err := os.ReadFile(filepath.Join(packages, "sets", kb+".json"))
		if kb == "" || err != nil {
			return errors.New("base recorded no packages")
		}
		sums := regexp.MustCompile(`(?m)^ *"sha256": "([0-9a-f]*)",$`).FindAllStringSubmatch(string(b), -1)
		if len(sums) == 0 {
			return errors.New("base's package set lists no SHA-256")
		}
		for _, m := range sums {
			if got, err := fileSHA256(filepath.Join(packages, m[1]+".deb")); err != nil || got != m[1] {
				return fmt.Errorf("the cache's %s.deb isn't there, or has another SHA-256", m[1])
			}
		}
		return nil
	})
	cfg := filepath.Join(s.sb.config(), "devvm", "config.toml")
	saved, _ := os.ReadFile(cfg)
	_ = AppendFile(cfg, "ubuntu-mirror = \"http://127.0.0.1:9/ubuntu\"\n")
	if kb != "" {
		_, _ = s.Limactl("delete", "-f", "devvm-img-"+kb)
		_ = os.RemoveAll(filepath.Join(s.Paths.Images, kb))
	}
	s.Check("with the mirror unreachable, base builds again under the same key from the cache, no network for its packages", func() error {
		if kb == "" {
			return errNo
		}
		r, err := s.DevvmNames("", "build", "base")
		if err != nil {
			return err
		}
		runlog := CaptureLine(r.Out, `^ *log_details file=(.*)$`)
		b, err := os.ReadFile(runlog)
		switch {
		case !HasLine(r.Out, "==> building_image image=base key="+kb):
			return errors.New("base wasn't built under the same key")
		case runlog == "" || err != nil:
			return errors.New("the build named no run log")
		case !strings.Contains(string(b), " file:/var/cache/devvm/packages/in "):
			return errors.New("apt didn't read the build's own folder")
		case Matches(string(b), `^(Get|Hit|Err|Ign):[0-9]+ https?://`):
			return errors.New("apt reached a mirror")
		case s.Status("devvm-img-"+kb) != "Stopped" || !Exists(filepath.Join(s.Paths.Images, kb, "record.json")):
			return errors.New("base isn't there, stopped, with its record")
		}
		return nil
	})
	_ = os.WriteFile(cfg, saved, 0o600)
}

// fileSHA256 is the SHA-256 of f's bytes, in hex.
func fileSHA256(f string) (string, error) {
	fh, err := os.Open(f)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// imagesGolden captures the golden fixtures (--golden): real Lima's
// dummy instances and its version, base's disk, and the Keychain's and
// Time Machine's answers.
func imagesGolden(s *S, kb string) {
	if !s.sb.o.golden {
		return
	}
	dir := filepath.Join(s.sb.root, "testdata", "golden", runtime.GOOS)
	if s.GoE2E("TestLimactlStubContract", "backend/lima", "STUB_GOLDEN="+dir) == nil {
		s.r.out.line("golden: limactl-version.txt limactl-list-stopped.json")
	} else {
		s.r.out.line("golden: capturing real Lima's dummy instances failed")
	}
	if runtime.GOOS == "linux" && kb != "" {
		s.Golden("qemu-img-info.json", "qemu-img", "info", "-U", "--output=json", filepath.Join(s.sb.lima, "devvm-img-"+kb, "disk"))
	}
	if runtime.GOOS == "darwin" {
		s.Golden("security-missing.txt", "bash", "-c", `security find-generic-password -s devvm-go-e2e-golden -a none -w; echo "exit $?"`)
		s.Golden("tmutil-isexcluded.txt", "tmutil", "isexcluded", s.sb.work)
	}
}
