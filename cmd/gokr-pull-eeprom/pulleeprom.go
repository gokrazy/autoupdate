package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/google/go-github/v35/github"
)

// getUpstreamCommit returns the SHA of the most recent
// github.com/raspberrypi/firmware git commit which touches
// boot/*.{elf,bin,dat}.
func getUpstreamCommit(ctx context.Context, client *github.Client) (string, error) {
	_, dirContents, _, err := client.Repositories.GetContents(ctx, "raspberrypi", "rpi-eeprom", "firmware/stable", &github.RepositoryContentGetOptions{})
	if err != nil {
		return "", err
	}

	var latestCommit *github.RepositoryCommit

	for _, c := range dirContents {
		if !strings.HasSuffix(*c.Name, ".bin") {
			continue
		}
		commits, _, err := client.Repositories.ListCommits(ctx, "raspberrypi", "rpi-eeprom", &github.CommitsListOptions{
			Path: *c.Path,
			ListOptions: github.ListOptions{
				Page:    1,
				PerPage: 1,
			},
		})
		if err != nil {
			return "", err
		}
		if got, want := len(commits), 1; got != want {
			return "", fmt.Errorf("unexpected number of commits for file %q: got %d, want %d", *c.Path, got, want)
		}
		// NOTE that the assumption is that
		// https://github.com/raspberrypi/firmware uses correct commit
		// dates. In case they stop doing that, we’ll need to list all
		// commits to find which commit is newer.
		if latestCommit == nil || commits[0].Commit.Committer.Date.After(*latestCommit.Commit.Committer.Date) {
			latestCommit = commits[0]
		}
		log.Printf("at %s (%v): %s", *commits[0].SHA, *commits[0].Commit.Committer.Date, *c.Path)
	}

	log.Printf("picked %s as most recent upstream firmware commit", *latestCommit.SHA)
	return *latestCommit.SHA, nil
}

func updateEeprom(ctx context.Context, client *github.Client, owner, repo string) error {
	upstreamCommit, err := getUpstreamCommit(ctx, client)
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

	var (
		updaterSHA  string
		updaterPath = "cmd/gokr-update-eeprom/eeprom.go"
	)
	for _, entry := range baseTree.Entries {
		if *entry.Path == updaterPath {
			updaterSHA = *entry.SHA
			break
		}
	}

	if updaterSHA == "" {
		return fmt.Errorf("%s not found in %s/%s", updaterPath, owner, repo)
	}

	updaterBlob, _, err := client.Git.GetBlob(ctx, owner, repo, updaterSHA)
	if err != nil {
		return err
	}

	updaterContent, err := base64.StdEncoding.DecodeString(*updaterBlob.Content)
	if err != nil {
		return err
	}

	eepromRefRe := regexp.MustCompile(`const eepromRef = "([0-9a-f]+)"`)
	matches := eepromRefRe.FindStringSubmatch(string(updaterContent))
	if matches == nil {
		return fmt.Errorf("regexp %v resulted in no matches", eepromRefRe)
	}
	if matches[1] == upstreamCommit {
		log.Printf("already at latest commit")
		return nil
	}
	newContent := eepromRefRe.ReplaceAllLiteral(updaterContent,
		[]byte(fmt.Sprintf(`const eepromRef = "%s"`, upstreamCommit)))

	entries := []*github.TreeEntry{
		{
			Path:    github.String(updaterPath),
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

	newCommit, _, err := client.Git.CreateCommit(ctx, owner, repo, &github.Commit{
		Message: github.String("auto-update to https://github.com/raspberrypi/rpi-eeprom/commit/" + upstreamCommit),
		Tree:    newTree,
		Parents: []*github.Commit{lastCommit},
	})
	if err != nil {
		return err
	}
	log.Printf("newCommit = %+v", newCommit)

	newRef, _, err := client.Git.CreateRef(ctx, owner, repo, &github.Reference{
		Ref: github.String("refs/heads/pull-" + upstreamCommit),
		Object: &github.GitObject{
			SHA: newCommit.SHA,
		},
	})
	if err != nil {
		return err
	}
	log.Printf("newRef = %+v", newRef)

	pr, _, err := client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: github.String("auto-update to " + upstreamCommit),
		Head:  github.String("pull-" + upstreamCommit),
		Base:  github.String("main"),
	})
	if err != nil {
		return err
	}

	log.Printf("pr = %+v", pr)

	return nil
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	for _, name := range []string{
		"GITHUB_REPOSITORY",
		"GH_USER",
		"GH_AUTH_TOKEN",
	} {
		if os.Getenv(name) == "" {
			log.Fatalf("required environment variable %q empty", name)
		}
	}

	slug := os.Getenv("GITHUB_REPOSITORY")

	parts := strings.Split(slug, "/")
	if got, want := len(parts), 2; got != want {
		log.Fatalf("unexpected number of /-separated parts in %q: got %d, want %d", slug, got, want)
	}

	ctx := context.Background()

	client := github.NewClient(&http.Client{
		Transport: &github.BasicAuthTransport{
			Username: os.Getenv("GH_USER"),
			Password: os.Getenv("GH_AUTH_TOKEN"),
		},
	})

	if err := updateEeprom(ctx, client, parts[0], parts[1]); err != nil {
		log.Fatal(err)
	}
}
