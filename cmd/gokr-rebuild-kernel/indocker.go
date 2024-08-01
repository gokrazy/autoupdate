package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func downloadKernel(latest string) error {
	out, err := os.Create(filepath.Base(latest))
	if err != nil {
		return err
	}
	defer out.Close()
	resp, err := http.Get(latest)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return fmt.Errorf("unexpected HTTP status code for %s: got %d, want %d", latest, got, want)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return out.Close()
}

func applyPatches(srcdir string) error {
	patches, err := filepath.Glob("*.patch")
	if err != nil {
		return err
	}
	for _, patch := range patches {
		log.Printf("applying patch %q", patch)
		f, err := os.Open(patch)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd := exec.Command("patch", "-p1")
		cmd.Dir = srcdir
		cmd.Stdin = f
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		f.Close()
	}

	return nil
}

func compile() error {
	defconfig := exec.Command("make", "defconfig")
	defconfig.Stdout = os.Stdout
	defconfig.Stderr = os.Stderr
	if err := defconfig.Run(); err != nil {
		return fmt.Errorf("make defconfig: %v", err)
	}

	// Change answers from mod to no if possible, i.e. disable all modules so
	// that we end up with a minimal set of modules (from the config addendum).
	mod2noconfig := exec.Command("make", "mod2noconfig")
	mod2noconfig.Stdout = os.Stdout
	mod2noconfig.Stderr = os.Stderr
	if err := mod2noconfig.Run(); err != nil {
		return fmt.Errorf("make olddefconfig: %v", err)
	}

	f, err := os.OpenFile(".config", os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	addendum, err := os.ReadFile("/usr/src/config.addendum.txt")
	if err != nil {
		return err
	}
	if _, err := f.Write(addendum); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	olddefconfig := exec.Command("make", "olddefconfig")
	olddefconfig.Stdout = os.Stdout
	olddefconfig.Stderr = os.Stderr
	if err := olddefconfig.Run(); err != nil {
		return fmt.Errorf("make olddefconfig: %v", err)
	}

	env := append(os.Environ(),
		"KBUILD_BUILD_USER=gokrazy",
		"KBUILD_BUILD_HOST=docker",
		"KBUILD_BUILD_TIMESTAMP=Wed Mar  1 20:57:29 UTC 2017",
	)
	make := exec.Command("make", "bzImage", "modules", "-j"+strconv.Itoa(runtime.NumCPU()))
	make.Env = env
	make.Stdout = os.Stdout
	make.Stderr = os.Stderr
	if err := make.Run(); err != nil {
		return fmt.Errorf("make: %v", err)
	}

	make = exec.Command("make", "INSTALL_MOD_PATH=/tmp/buildresult", "modules_install", "-j"+strconv.Itoa(runtime.NumCPU()))
	make.Env = env
	make.Stdout = os.Stdout
	make.Stderr = os.Stderr
	if err := make.Run(); err != nil {
		return fmt.Errorf("make: %v", err)
	}

	return nil
}

func indockerMain() {
	flag.Parse()
	latest := flag.Arg(0)
	if latest == "" {
		log.Fatalf("syntax: %s <upstream-URL>", os.Args[0])
	}
	log.Printf("downloading kernel source: %s", latest)
	if err := downloadKernel(latest); err != nil {
		log.Fatal(err)
	}

	log.Printf("unpacking kernel source")
	untar := exec.Command("tar", "xf", filepath.Base(latest))
	untar.Stdout = os.Stdout
	untar.Stderr = os.Stderr
	if err := untar.Run(); err != nil {
		log.Fatalf("untar: %v", err)
	}

	srcdir := strings.TrimSuffix(filepath.Base(latest), ".tar.xz")

	log.Printf("applying patches")
	if err := applyPatches(srcdir); err != nil {
		log.Fatal(err)
	}

	if err := os.Chdir(srcdir); err != nil {
		log.Fatal(err)
	}

	log.Printf("compiling kernel")
	if err := compile(); err != nil {
		log.Fatal(err)
	}

	if err := copyFile("/tmp/buildresult/vmlinuz", "arch/x86/boot/bzImage"); err != nil {
		log.Fatal(err)
	}
}
