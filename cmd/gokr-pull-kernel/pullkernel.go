package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/gokrazy/autoupdate/internal/cienv"
	"github.com/google/go-github/v35/github"
)

var (
	updaterPath = flag.String("updater_path",
		"cmd/gokr-build-kernel/build.go",
		"build.go path to update")

	flavor = flag.String("flavor",
		"vanilla",
		"which kernel flavor to pull. one of vanilla (kernel.org) or raspberrypi (https://github.com/raspberrypi/linux/tags)")
)

func getUpstreamURL(ctx context.Context) (string, error) {
	resp, err := http.Get("https://www.kernel.org/releases.json")
	if err != nil {
		return "", err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return "", fmt.Errorf("unexpected HTTP status code: got %d, want %d", got, want)
	}
	var releases struct {
		LatestStable struct {
			Version string `json:"version"`
		} `json:"latest_stable"`
		Releases []struct {
			Version string `json:"version"`
			Source  string `json:"source"`
		} `json:"releases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return "", err
	}
	for _, release := range releases.Releases {
		if release.Version != releases.LatestStable.Version {
			continue
		}
		return release.Source, nil
	}
	return "", fmt.Errorf("malformed releases.json: latest stable release %q not found in releases list", releases.LatestStable.Version)
}

func getRaspberryPiURL(ctx context.Context, client *github.Client) (string, error) {
	// The raspberrypi/linux repository (currently) tags releases with names
	// like stable_20240423. Sort them in reverse order, then select the latest.
	const owner = "raspberrypi"
	const repo = "linux"
	tags, _, err := client.Repositories.ListTags(ctx, owner, repo, &github.ListOptions{})
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(tags))
	for _, tag := range tags {
		if strings.HasPrefix(*tag.Name, "stable_") {
			names = append(names, *tag.Name)
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("BUG: no stable_ tags found")
	}
	slices.Sort(names)
	slices.Reverse(names)
	return "https://github.com/raspberrypi/linux/archive/refs/tags/" + names[0] + ".tar.gz", nil
}

func updateKernel(ctx context.Context, client *github.Client, flavor, owner, repo string) error {
	var upstreamURL string
	var err error
	switch flavor {
	case "vanilla":
		upstreamURL, err = getUpstreamURL(ctx)
	case "raspberrypi":
		upstreamURL, err = getRaspberryPiURL(ctx, client)
	}
	if err != nil {
		return err
	}

	lastRef, _, err := client.Git.GetRef(ctx, owner, repo, "heads/main")
	if err != nil {
		return err
	}

	lastCommit, _, err := client.Git.GetCommit(ctx, owner, repo, *lastRef.Object.SHA)
	if err != nil {
		return err
	}

	log.Printf("lastCommit = %+v", lastCommit)

	baseTree, _, err := client.Git.GetTree(ctx, owner, repo, *lastCommit.SHA, true)
	if err != nil {
		return err
	}
	log.Printf("baseTree = %+v", baseTree)

	var updaterSHA string
	for _, entry := range baseTree.Entries {
		if *entry.Path == *updaterPath {
			updaterSHA = *entry.SHA
			break
		}
	}

	if updaterSHA == "" {
		return fmt.Errorf("%s not found in %s/%s", *updaterPath, owner, repo)
	}

	updaterBlob, _, err := client.Git.GetBlob(ctx, owner, repo, updaterSHA)
	if err != nil {
		return err
	}

	updaterContent, err := base64.StdEncoding.DecodeString(*updaterBlob.Content)
	if err != nil {
		return err
	}

	kernelURLRe := regexp.MustCompile(`var latest = "([^"]+)"`)
	matches := kernelURLRe.FindStringSubmatch(string(updaterContent))
	if matches == nil {
		return fmt.Errorf("regexp %v resulted in no matches", kernelURLRe)
	}
	if matches[1] == upstreamURL {
		log.Printf("already at latest commit")
		return nil
	}
	newContent := kernelURLRe.ReplaceAllLiteral(updaterContent,
		[]byte(fmt.Sprintf(`var latest = "%s"`, upstreamURL)))

	entries := []*github.TreeEntry{
		{
			Path:    github.String(*updaterPath),
			Mode:    github.String("100644"),
			Type:    github.String("blob"),
			Content: github.String(string(newContent)),
		},
	}

	newTree, _, err := client.Git.CreateTree(ctx, owner, repo, *baseTree.SHA, entries)
	if err != nil {
		return err
	}
	log.Printf("newTree = %+v", newTree)

	version := path.Base(upstreamURL)

	newCommit, _, err := client.Git.CreateCommit(ctx, owner, repo, &github.Commit{
		Message: github.String("auto-update to " + version),
		Tree:    newTree,
		Parents: []*github.Commit{lastCommit},
	})
	if err != nil {
		return err
	}
	log.Printf("newCommit = %+v", newCommit)

	newRef, _, err := client.Git.CreateRef(ctx, owner, repo, &github.Reference{
		Ref: github.String("refs/heads/pull-" + version),
		Object: &github.GitObject{
			SHA: newCommit.SHA,
		},
	})
	if err != nil {
		return err
	}
	log.Printf("newRef = %+v", newRef)

	pr, _, err := client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: github.String("auto-update to " + version),
		Head:  github.String("pull-" + version),
		Base:  github.String("main"),
	})
	if err != nil {
		return err
	}

	log.Printf("pr = %+v", pr)

	return nil
}

var (
	githubUser = cienv.MustGetGithubUser()
	authToken  = cienv.MustGetAuthToken()
	slug       = cienv.MustGetSlug()
)

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	parts := strings.Split(slug, "/")
	if got, want := len(parts), 2; got != want {
		log.Fatalf("unexpected number of /-separated parts in %q: got %d, want %d", slug, got, want)
	}

	ctx := context.Background()

	client := github.NewClient(&http.Client{
		Transport: &github.BasicAuthTransport{
			Username: githubUser,
			Password: authToken,
		},
	})

	if err := updateKernel(ctx, client, *flavor, parts[0], parts[1]); err != nil {
		log.Fatal(err)
	}
}
