package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/github"
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
	cmd := exec.Command("gokr-packer", "-hostname=bakery", "-overwrite_boot="+f.Name(), "github.com/gokrazy/bakery/cmd/bake")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return f.Name(), cmd.Run()
}

func testBoot(bootImg, booteryURL string) (string, error) {
	f, err := os.Open(bootImg)
	if err != nil {
		return "", err
	}
	defer f.Close()
	req, err := http.NewRequest(http.MethodPut, booteryURL, f)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return "", fmt.Errorf("unexpected HTTP status code: got %d, want %d", got, want)
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

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	for _, name := range []string{
		"TRAVIS_PULL_REQUEST",
		"TRAVIS_PULL_REQUEST_BRANCH",
		"TRAVIS_REPO_SLUG",
		"GITHUB_USER",
		"GITHUB_AUTH_TOKEN",
	} {
		if os.Getenv(name) == "" {
			log.Fatalf("required environment variable %q empty", name)
		}
	}

	if *booteryURL == "" {
		log.Fatal("-booteryURL is a required flag")
	}

	if *requireLabel == "" {
		log.Fatal("-require_label is a required flag")
	}

	if *setLabel == "" {
		log.Fatal("-set_label is a required flag")
	}

	//branch := os.Getenv("TRAVIS_PULL_REQUEST_BRANCH")
	slug := os.Getenv("TRAVIS_REPO_SLUG")

	parts := strings.Split(slug, "/")
	if got, want := len(parts), 2; got != want {
		log.Fatalf("unexpected number of /-separated parts in %q: got %d, want %d", slug, got, want)
	}

	i, err := strconv.ParseInt(os.Getenv("TRAVIS_PULL_REQUEST"), 0, 64)
	if err != nil {
		log.Fatalf("could not parse TRAVIS_PULL_REQUEST=%q as number: %v", os.Getenv("TRAVIS_PULL_REQUEST"), err)
	}
	issueNum := int(i)

	client := github.NewClient(&http.Client{
		Transport: &github.BasicAuthTransport{
			Username: os.Getenv("GITHUB_USER"),
			Password: os.Getenv("GITHUB_AUTH_TOKEN"),
		},
	})

	ctx := context.Background()

	if err := ensureLabel(ctx, client, parts[0], parts[1], issueNum, *requireLabel); err != nil {
		log.Fatal(err)
	}

	bootImg, err := writeBootImage()
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(bootImg)

	bootlog, err := testBoot(bootImg, *booteryURL)
	if err != nil {
		log.Fatal(err)
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