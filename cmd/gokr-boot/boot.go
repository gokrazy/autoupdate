package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gokrazy/autoupdate/internal/cienv"
	"github.com/google/go-github/v35/github"
)

var (
	setLabel = flag.String("set_label",
		"",
		"if non-empty, name of a GitHub label to set on the pull request")

	requireLabel = flag.String("require_label",
		"",
		"name of the required label before the PR will be tested")

	booteryURL = flag.String("bootery_url",
		"",
		"/testboot URL to send boot images to")

	kernelPackage = flag.String("kernel_package",
		"github.com/gokrazy/kernel",
		"-kernel_package to pass to gokr-packer")

	firmwarePackage = flag.String("firmware_package",
		"github.com/gokrazy/firmware",
		"-firmware_package to pass to gokr-packer")

	serialConsole = flag.String("serial_console",
		"ttyAMA0,115200",
		"-serial_console to pass to gokr-packer")
)

func createGist(ctx context.Context, client *github.Client, log string) (string, error) {
	filename := "boot-log-" + time.Now().Format(time.RFC3339)
	gist, _, err := client.Gists.Create(ctx,
		&github.Gist{
			Description: github.String("gokrazy boot log"),
			Public:      github.Bool(false),
			Files: map[github.GistFilename]github.GistFile{
				github.GistFilename(filename): {Content: github.String(log)},
			},
		})
	if err != nil {
		return "", err
	}
	return *gist.HTMLURL, nil
}

func writeBootImage() (string, error) {
	f, err := ioutil.TempFile("", "gokr-boot")
	if err != nil {
		return "", err
	}
	f.Close()
	cmd := exec.Command("gokr-packer",
		"-hostname=bakery",
		"-overwrite_boot="+f.Name(),
		"-kernel_package="+*kernelPackage,
		"-firmware_package="+*firmwarePackage,
		"-serial_console="+*serialConsole,
		"github.com/gokrazy/bakery/cmd/bake",
		"github.com/gokrazy/timestamps")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return f.Name(), cmd.Run()
}

func testBoot(bootImg, booteryURL, slug, newer string) (string, error) {
	f, err := os.Open(bootImg)
	if err != nil {
		return "", err
	}
	defer f.Close()
	u, err := url.Parse(booteryURL)
	if err != nil {
		return "", err
	}
	v := u.Query()
	v.Set("slug", slug)
	v.Set("boot-newer", newer)
	u.RawQuery = v.Encode()
	req, err := http.NewRequest(http.MethodPut, u.String(), f)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		b, _ := ioutil.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected HTTP status code: got %d (%s), want %d", got, strings.TrimSpace(string(b)), want)
	}
	b, err := ioutil.ReadAll(resp.Body)
	return string(b), err
}

func ensureLabel(ctx context.Context, client *github.Client, owner, repo string, issueNum int, label string) error {
	labels, _, err := client.Issues.ListLabelsByIssue(ctx, owner, repo, issueNum, nil)
	if err != nil {
		return err
	}
	for _, l := range labels {
		if *l.Name == label {
			return nil
		}
	}
	return fmt.Errorf("label %q not found on issue %d", label, issueNum)
}

func addLabel(ctx context.Context, client *github.Client, owner, repo string, issueNum int, label string) error {
	_, _, err := client.Issues.AddLabelsToIssue(ctx, owner, repo, issueNum, []string{label})
	return err
}

func removeLabel(ctx context.Context, client *github.Client, owner, repo string, issueNum int, label string) error {
	_, err := client.Issues.RemoveLabelForIssue(ctx, owner, repo, issueNum, label)
	return err
}

func addComment(ctx context.Context, client *github.Client, owner, repo string, issueNum int, gistURL string) error {
	_, _, err := client.Issues.CreateComment(ctx, owner, repo, issueNum, &github.IssueComment{
		Body: github.String(fmt.Sprintf("Boot test successful, find the log at %s", gistURL)),
	})
	return err
}

var (
	githubUser        = cienv.MustGetGithubUser()
	authToken         = cienv.MustGetAuthToken()
	slug              = cienv.MustGetSlug()
	travisPullRequest = cienv.MustGetPullRequest()
)

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if *booteryURL == "" {
		log.Fatal("-bootery_url is a required flag")
	}

	if *requireLabel == "" {
		log.Fatal("-require_label is a required flag")
	}

	if *setLabel == "" {
		log.Fatal("-set_label is a required flag")
	}

	parts := strings.Split(slug, "/")
	if got, want := len(parts), 2; got != want {
		log.Fatalf("unexpected number of /-separated parts in %q: got %d, want %d", slug, got, want)
	}

	i, err := strconv.ParseInt(travisPullRequest, 0, 64)
	if err != nil {
		log.Fatalf("could not parse TRAVIS_PULL_REQUEST=%q as number: %v", os.Getenv("TRAVIS_PULL_REQUEST"), err)
	}
	issueNum := int(i)

	client := github.NewClient(&http.Client{
		Transport: &github.BasicAuthTransport{
			Username: githubUser,
			Password: authToken,
		},
	})

	ctx := context.Background()

	if err := ensureLabel(ctx, client, parts[0], parts[1], issueNum, *requireLabel); err != nil {
		// Exit with exit code 0 if there is nothing to do.
		log.Println(err.Error())
		return
	}

	newer := strconv.FormatInt(time.Now().Unix(), 10)

	bootImg, err := writeBootImage()
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(bootImg)

	bootlog, err := testBoot(bootImg, *booteryURL, slug, newer)
	if err != nil {
		log.Fatal(strings.Replace(err.Error(), *booteryURL, "<bootery_url>", -1))
	}

	gistURL, err := createGist(ctx, client, bootlog)
	if err != nil {
		log.Fatal(err)
	}

	if err := addComment(ctx, client, parts[0], parts[1], issueNum, gistURL); err != nil {
		log.Fatal(err)
	}

	if err := addLabel(ctx, client, parts[0], parts[1], issueNum, *setLabel); err != nil {
		log.Fatal(err)
	}

	if err := removeLabel(ctx, client, parts[0], parts[1], issueNum, *requireLabel); err != nil {
		log.Fatal(err)
	}
}
