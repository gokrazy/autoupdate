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
	"strings"
	"text/template"
)

const dockerFileContents = `
FROM debian:bookworm

RUN apt-get update && apt-get install -y \
{{ if (eq .Cross "arm64") -}}
  crossbuild-essential-arm64 \
{{ end -}}
  build-essential bc libssl-dev bison flex libelf-dev ncurses-dev ca-certificates zstd kmod python3

COPY gokr-rebuild-kernel /usr/bin/gokr-rebuild-kernel
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

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

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
		"if non-empty, cross-compile for the specified arch (one of 'arm64')")

	flag.Parse()

	if *cross != "" && *cross != "arm64" {
		return fmt.Errorf("invalid -cross value %q: expected one of 'arm64'")
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

	dockerFile, err := os.Create("Dockerfile")
	if err != nil {
		return err
	}

	if err := dockerFileTmpl.Execute(dockerFile, struct {
		Uid     string
		Gid     string
		Patches []string
		Cross   string
	}{
		Uid:     u.Uid,
		Gid:     u.Gid,
		Patches: patches,
		Cross:   *cross,
	}); err != nil {
		return err
	}

	if err := dockerFile.Close(); err != nil {
		return err
	}

	log.Printf("building %s container for kernel compilation", execName)

	dockerBuild := exec.Command(execName,
		"build",
		"--platform=linux/amd64",
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

	dockerArgs := []string{
		"run",
		"--platform=linux/amd64",
		"--volume", abs + ":/tmp/buildresult:Z",
	}

	if !*keepBuildContainer {
		dockerArgs = append(dockerArgs, "--rm")
	}
	if execName == "podman" {
		dockerArgs = append(dockerArgs, "--userns=keep-id")
	}
	dockerArgs = append(dockerArgs,
		"gokr-rebuild-kernel",
		"-cross="+*cross,
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
