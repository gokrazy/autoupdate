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

func compile(cross, flavor string) error {
	defconfig := exec.Command("make", "defconfig")
	if flavor == "raspberrypi" {
		// TODO(https://github.com/gokrazy/gokrazy/issues/223): is it
		// necessary/desirable to switch to bcm2712_defconfig?
		defconfig = exec.Command("make", "ARCH=arm64", "bcm2711_defconfig")
	}

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
	if cross == "arm64" {
		make = exec.Command("make", "Image.gz", "dtbs", "modules", "-j"+strconv.Itoa(runtime.NumCPU()))
	}
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
	cross := flag.String("cross",
		"",
		"if non-empty, cross-compile for the specified arch (one of 'arm64')")

	flavor := flag.String("flavor",
		"vanilla",
		"which kernel flavor to build. one of vanilla (kernel.org) or raspberrypi (https://github.com/raspberrypi/linux/tags)")

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
	if *flavor == "raspberrypi" {
		srcdir = strings.TrimSuffix("linux-"+filepath.Base(latest), ".tar.gz")
	}

	log.Printf("applying patches")
	if err := applyPatches(srcdir); err != nil {
		log.Fatal(err)
	}

	if err := os.Chdir(srcdir); err != nil {
		log.Fatal(err)
	}

	if *cross == "arm64" {
		log.Printf("exporting ARCH=arm64, CROSS_COMPILE=aarch64-linux-gnu-")
		os.Setenv("ARCH", "arm64")
		os.Setenv("CROSS_COMPILE", "aarch64-linux-gnu-")
	}

	log.Printf("compiling kernel")
	if err := compile(*cross, *flavor); err != nil {
		log.Fatal(err)
	}

	if *cross == "arm64" {
		if err := copyFile("/tmp/buildresult/vmlinuz", "arch/arm64/boot/Image"); err != nil {
			log.Fatal(err)
		}

		switch *flavor {
		case "vanilla":
			// copy device tree files from arch/arm64/boot/dts/broadcom/ to buildresult
			for dest, source := range map[string]string{
				"bcm2710-rpi-3-b.dtb":      "bcm2837-rpi-3-b.dtb",
				"bcm2710-rpi-3-b-plus.dtb": "bcm2837-rpi-3-b-plus.dtb",
				"bcm2710-rpi-cm3.dtb":      "bcm2837-rpi-cm3-io3.dtb",
				"bcm2711-rpi-4-b.dtb":      "bcm2711-rpi-4-b.dtb",
				"bcm2711-rpi-cm4-io.dtb":   "bcm2711-rpi-cm4-io.dtb",
				"bcm2710-rpi-zero-2-w.dtb": "bcm2837-rpi-zero-2-w.dtb",
				"bcm2710-rpi-zero-2.dtb":   "bcm2837-rpi-zero-2-w.dtb",
				"bcm2711-rpi-400.dtb":      "bcm2711-rpi-400.dtb",
			} {
				if err := copyFile("/tmp/buildresult/"+dest, "arch/arm64/boot/dts/broadcom/"+source); err != nil {
					log.Fatal(err)
				}
			}

		case "raspberrypi":
			// copy all dtb and dtbos (+ overlay_map) to buildresult
			dtbs, err := filepath.Glob("arch/arm64/boot/dts/broadcom/*.dtb")
			if err != nil {
				log.Fatal(err)
			}
			for _, fn := range dtbs {
				if err := copyFile(filepath.Join("/tmp/buildresult/", filepath.Base(fn)), fn); err != nil {
					log.Fatal(err)
				}
			}

			dtbos, err := filepath.Glob("arch/arm64/boot/dts/overlays/*.dtbo")
			if err != nil {
				log.Fatal(err)
			}
			dtbos = append(dtbos, "arch/arm64/boot/dts/overlays/overlay_map.dtb")
			if err := os.MkdirAll("/tmp/buildresult/overlays", 0755); err != nil {
				log.Fatal(err)
			}
			for _, fn := range dtbos {
				if err := copyFile(filepath.Join("/tmp/buildresult/overlays/", filepath.Base(fn)), fn); err != nil {
					log.Fatal(err)
				}
			}
		}
	} else {
		if err := copyFile("/tmp/buildresult/vmlinuz", "arch/x86/boot/bzImage"); err != nil {
			log.Fatal(err)
		}
	}
}
