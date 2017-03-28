// gokr-merge merges GitHub pull requests with the right labels.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/google/go-github/github"
)

var (
	requireLabel = flag.String("require_label",
		"",
		"name of the required label before the PR will be merged")
)

func ensureLabel(ctx context.Context, client *github.Client, owner, repo string, issueNum int, label string) (bool, error) {
	labels, _, err := client.Issues.ListLabelsByIssue(ctx, owner, repo, issueNum, nil)
	if err != nil {
		return true, err
	}
	for _, l := range labels {
		if *l.Name == label {
			return true, nil
		}
	}
	return false, nil
}

func merge(ctx context.Context, client *github.Client, owner, repo string, issueNum int) error {
	_, _, err := client.PullRequests.Merge(ctx, owner, repo, issueNum, "automatically merged", &github.PullRequestOptions{
		MergeMethod: "squash",
	})
	return err
}

func deleteRef(ctx context.Context, client *github.Client, owner, repo string, ref string) error {
	_, err := client.Git.DeleteRef(ctx, owner, repo, ref)
	return err
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if *requireLabel == "" {
		log.Fatal("-require_label is a required flag")
	}

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

	slug := os.Getenv("TRAVIS_REPO_SLUG")

	parts := strings.Split(slug, "/")
	if got, want := len(parts), 2; got != want {
		log.Fatalf("unexpected number of /-separated parts in %q: got %d, want %d", slug, got, want)
	}

	ctx := context.Background()

	client := github.NewClient(&http.Client{
		Transport: &github.BasicAuthTransport{
			Username: os.Getenv("GITHUB_USER"),
			Password: os.Getenv("GITHUB_AUTH_TOKEN"),
		},
	})

	issueNum, err := strconv.ParseInt(os.Getenv("TRAVIS_PULL_REQUEST"), 0, 64)
	if err != nil {
		log.Fatal(err)
	}

	found, err := ensureLabel(ctx, client, parts[0], parts[1], int(issueNum), *requireLabel)
	if err != nil {
		log.Fatal(err)
	}
	if !found {
		os.Exit(2) // label not present
	}

	if err := merge(ctx, client, parts[0], parts[1], int(issueNum)); err != nil {
		log.Fatal(err)
	}

	if err := deleteRef(ctx, client, parts[0], parts[1], os.Getenv("TRAVIS_PULL_REQUEST_BRANCH")); err != nil {
		log.Fatal(err)
	}
}
