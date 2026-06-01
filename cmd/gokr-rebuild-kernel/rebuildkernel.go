package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

const dockerFileContents = `
FROM debian:bookworm

RUN apt-get update && apt-get install -y \
{{ if .CrossPkg -}}
  {{ .CrossPkg }} \
{{ end -}}
  build-essential bc libssl-dev bison flex libelf-dev ncurses-dev ca-certificates zstd kmod python3

COPY {{ .ContainerBinary }} /usr/bin/gokr-rebuild-kernel
COPY config.addendum.txt /usr/src/config.addendum.txt
{{- range $idx, $path := .Patches }}
COPY {{ $path }} /usr/src/{{ $path }}
{{- end }}

RUN echo 'builduser:x:{{ .Uid }}:{{ .Gid }}:nobody:/:/bin/sh' >> /etc/passwd && \
    chown -R {{ .Uid }}:{{ .Gid }} /usr/src

USER builduser
WORKDIR /usr/src
ENV GOKRAZY_IN_DOCKER=1
ENTRYPOINT ["/usr/bin/gokr-rebuild-kernel"]
`

var dockerFileTmpl = template.Must(template.New("dockerfile").
	Funcs(map[string]interface{}{
		"basename": func(path string) string {
			return filepath.Base(path)
		},
	}).
	Parse(dockerFileContents))

func copyFile(dest, src string) error {
	log.Printf("copyFile(dest=%s, src=%s)", dest, src)
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	n, err := io.Copy(out, in)
	if err != nil {
		return err
	}
	log.Printf("  -> %d bytes copied", n)

	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := out.Chmod(st.Mode()); err != nil {
		return err
	}
	return out.Close()
}

func find(filename string) (string, error) {
	if _, err := os.Stat(filename); err == nil {
		return filename, nil
	}

	return "", fmt.Errorf("could not find file %q", filename)
}

// findModuleRoot locates the root directory of the main module by inspecting
// the absolute source-file paths that Go embeds in binaries built without
// -trimpath (the default for local/development builds). It walks up from the
// directory of the current source file until it finds a go.mod.
func findModuleRoot() (string, error) {
	pc := make([]uintptr, 1)
	if runtime.Callers(1, pc) == 0 {
		return "", fmt.Errorf("runtime.Callers returned no frames")
	}
	frames := runtime.CallersFrames(pc)
	f, _ := frames.Next()
	if f.File == "" || !filepath.IsAbs(f.File) {
		return "", fmt.Errorf("source path %q is not absolute (binary may have been built with -trimpath)", f.File)
	}
	dir := filepath.Dir(f.File)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found walking up from %s", f.File)
		}
		dir = parent
	}
}

// containerGOOS is the OS of the container image; always linux.
const containerGOOS = "linux"

// crossPackage returns the Debian package that provides the cross-compilation
// toolchain when the host arch differs from the target kernel arch.
// Returns empty string when no cross-compilation is needed.
func crossPackage(containerArch, targetArch string) string {
	if containerArch == targetArch {
		return ""
	}
	switch targetArch {
	case "arm64":
		return "crossbuild-essential-arm64"
	case "amd64":
		return "crossbuild-essential-amd64"
	default:
		return ""
	}
}

// containerBinaryName returns the filename of the gokr-rebuild-kernel binary
// that should be copied into the container image. When the host already
// produces a binary for the container's platform the running executable is
// used directly; otherwise we cross-compile one first.
func containerBinaryName(goarch string) (string, error) {
	if runtime.GOOS == containerGOOS && runtime.GOARCH == goarch {
		return "gokr-rebuild-kernel", nil
	}

	linuxBin := "gokr-rebuild-kernel.linux-" + goarch

	moduleRoot, err := findModuleRoot()
	if err != nil {
		return "", fmt.Errorf("locating module root for cross-compilation: %v", err)
	}

	outPath, err := filepath.Abs(linuxBin)
	if err != nil {
		return "", err
	}

	log.Printf("cross-compiling %s/%s container binary from %s", containerGOOS, goarch, moduleRoot)
	cmd := exec.Command("go", "build", "-o", outPath, "./cmd/gokr-rebuild-kernel")
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(),
		"GOOS="+containerGOOS,
		"GOARCH="+goarch,
		"CGO_ENABLED=0",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("cross-compiling %s/%s binary: %v", containerGOOS, goarch, err)
	}
	return linuxBin, nil
}

func getContainerExecutable() (string, error) {
	// Probe podman first, because the docker binary might actually
	// be a thin podman wrapper with podman behavior.
	choices := []string{"podman", "docker"}
	for _, exe := range choices {
		p, err := exec.LookPath(exe)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		return resolved, nil
	}
	return "", fmt.Errorf("none of %v found in $PATH", choices)
}

func rebuildKernel() error {
	overwriteContainerExecutable := flag.String("overwrite_container_executable",
		"",
		"E.g. docker or podman to overwrite the automatically detected container executable")

	keepBuildContainer := flag.Bool("keep_build_container",
		false,
		"do not delete build container after building the kernel")

	cross := flag.String("cross",
		"",
		"if non-empty, cross-compile for the specified arch (one of 'arm64','amd64')")

	flavor := flag.String("flavor",
		"vanilla",
		"which kernel flavor to build. one of vanilla (kernel.org) or raspberrypi (https://github.com/raspberrypi/linux/tags)")

	dtbs := flag.String("dtbs",
		"raspberrypi",
		"which device tree files (.dtb files) to copy. 'raspberrypi' or empty")

	flag.Parse()

	if *cross != "" && *cross != "arm64" && *cross != "amd64" {
		return fmt.Errorf("invalid -cross value %q: expected one of 'arm64','amd64',''", *cross)
	}

	abs, err := os.Getwd()
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSuffix(abs, "/"), "/_build") {
		return fmt.Errorf("gokr-rebuild-kernel is not run from a _build directory")
	}

	series, err := os.ReadFile("series")
	if err != nil {
		return err
	}
	patches := strings.Split(strings.TrimSpace(string(series)), "\n")

	executable, err := getContainerExecutable()
	if err != nil {
		return err
	}
	if *overwriteContainerExecutable != "" {
		executable = *overwriteContainerExecutable
	}

	execName := filepath.Base(executable)

	var patchPaths []string
	for _, filename := range patches {
		path, err := find(filename)
		if err != nil {
			return err
		}
		patchPaths = append(patchPaths, path)
	}

	kernelPath, err := find("../vmlinuz")
	if err != nil {
		return err
	}

	libPath, err := find("../lib")
	if err != nil {
		return err
	}

	// TODO: just ensure the file exists, i.e. we are in _build
	if _, err := find("config.addendum.txt"); err != nil {
		return err
	}

	u, err := user.Current()
	if err != nil {
		return err
	}

	upstreamURL, err := os.ReadFile("upstream-url.txt")
	if err != nil {
		return err
	}

	// The architecture of the host environment.
	hostArch := runtime.GOARCH

	// The architecture of the target kernel.
	var targetArch string

	// Determine the target architecture based on the cross-compilation flag.
	switch *cross {
	case "arm64":
		targetArch = "arm64"
	case "amd64", "":
		// Default to building for amd64 if no cross is specified.
		targetArch = "amd64"
	default:
		return fmt.Errorf("invalid -cross value %q: expected one of 'arm64','amd64',''", *cross)
	}

	// When defining what container platform to compile into, match the host arch for better container performance.
	// To compile for a target arch that differs from the container arch use cross-compilation.
	// This should be faster than natively (non-cross) compiling in a container arch that does not match the host arch.
	containerArch := hostArch
	containerPlatform := containerGOOS + "/" + containerArch

	crossPkg := crossPackage(containerArch, targetArch)

	containerBin, err := containerBinaryName(containerArch)
	if err != nil {
		return err
	}

	dockerFile, err := os.Create("Dockerfile")
	if err != nil {
		return err
	}

	if err := dockerFileTmpl.Execute(dockerFile, struct {
		Uid             string
		Gid             string
		Patches         []string
		ContainerBinary string
		CrossPkg        string
	}{
		Uid:             u.Uid,
		Gid:             u.Gid,
		Patches:         patches,
		ContainerBinary: containerBin,
		CrossPkg:        crossPkg,
	}); err != nil {
		return err
	}

	if err := dockerFile.Close(); err != nil {
		return err
	}

	log.Printf("building %s container for kernel compilation", execName)

	dockerBuild := exec.Command(execName,
		"build",
		"--platform="+containerPlatform,
		"--rm=true",
		"--tag=gokr-rebuild-kernel",
		".")
	dockerBuild.Stdout = os.Stdout
	dockerBuild.Stderr = os.Stderr
	log.Printf("%v", dockerBuild.Args)
	if err := dockerBuild.Run(); err != nil {
		return fmt.Errorf("%s build: %v (cmd: %v)", execName, err, dockerBuild.Args)
	}

	log.Printf("compiling kernel")

	var dockerRun *exec.Cmd

	volumeMount := abs + ":/tmp/buildresult"
	if runtime.GOOS == "linux" {
		// :Z re-labels the volume for SELinux, only relevant on Linux hosts.
		volumeMount += ":Z"
	}

	dockerArgs := []string{
		"run",
		"--platform=" + containerPlatform,
		"--volume", volumeMount,
	}

	if !*keepBuildContainer {
		dockerArgs = append(dockerArgs, "--rm")
	}
	if execName == "podman" && runtime.GOOS == "linux" {
		// --userns=keep-id maps the in-container user to the host UID via
		// Linux user namespaces; not applicable on macOS where Podman runs
		// inside a Linux VM with its own namespace handling.
		dockerArgs = append(dockerArgs, "--userns=keep-id")
	}
	dockerArgs = append(dockerArgs,
		"gokr-rebuild-kernel",
		"-cross="+*cross,
		"-flavor="+*flavor,
		strings.TrimSpace(string(upstreamURL)))

	dockerRun = exec.Command(executable, dockerArgs...)

	dockerRun.Stdout = os.Stdout
	dockerRun.Stderr = os.Stderr
	log.Printf("%v", dockerRun.Args)
	if err := dockerRun.Run(); err != nil {
		return fmt.Errorf("%s run: %v (cmd: %v)", execName, err, dockerRun.Args)
	}

	if err := copyFile(kernelPath, "vmlinuz"); err != nil {
		return err
	}

	// remove symlinks that only work when source/build directory are present
	for _, subdir := range []string{"build", "source"} {
		matches, err := filepath.Glob(filepath.Join("lib/modules", "*", subdir))
		if err != nil {
			return err
		}
		for _, match := range matches {
			log.Printf("removing build/source symlink %s", match)
			if err := os.Remove(match); err != nil {
				return err
			}
		}
	}

	// replace kernel modules directory
	rm := exec.Command("rm", "-rf", filepath.Join(libPath, "modules"))
	rm.Stdout = os.Stdout
	rm.Stderr = os.Stderr
	log.Printf("%v", rm.Args)
	if err := rm.Run(); err != nil {
		return fmt.Errorf("%v: %v", rm.Args, err)
	}
	cp := exec.Command("cp", "-r", filepath.Join("lib/modules"), libPath)
	cp.Stdout = os.Stdout
	cp.Stderr = os.Stderr
	log.Printf("%v", cp.Args)
	if err := cp.Run(); err != nil {
		return fmt.Errorf("%v: %v", cp.Args, err)
	}

	if *cross == "arm64" {
		if *dtbs != "" {
			// replace device tree files
			rm = exec.Command("sh", "-c", "rm ../*.dtb")
			rm.Stdout = os.Stdout
			rm.Stderr = os.Stderr
			log.Printf("%v", rm.Args)
			if err := rm.Run(); err != nil {
				return fmt.Errorf("%v: %v", rm.Args, err)
			}
			cp = exec.Command("sh", "-c", "cp *.dtb ..")
			cp.Stdout = os.Stdout
			cp.Stderr = os.Stderr
			log.Printf("%v", cp.Args)
			if err := cp.Run(); err != nil {
				return fmt.Errorf("%v: %v", cp.Args, err)
			}
		}

		if *flavor == "raspberrypi" {
			// replace overlays directory
			overlaysPath, err := find("../overlays")
			if err != nil {
				return err
			}
			rm = exec.Command("rm", "-rf", overlaysPath)
			rm.Stdout = os.Stdout
			rm.Stderr = os.Stderr
			log.Printf("%v", rm.Args)
			if err := rm.Run(); err != nil {
				return fmt.Errorf("%v: %v", rm.Args, err)
			}
			cp = exec.Command("cp", "-r", "overlays", overlaysPath)
			cp.Stdout = os.Stdout
			cp.Stderr = os.Stderr
			log.Printf("%v", cp.Args)
			if err := cp.Run(); err != nil {
				return fmt.Errorf("%v: %v", cp.Args, err)
			}
		}
	}

	return nil
}

func main() {
	if os.Getenv("GOKRAZY_IN_DOCKER") == "1" {
		indockerMain()
	} else {
		if err := rebuildKernel(); err != nil {
			log.Fatal(err)
		}
	}
}
