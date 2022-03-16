package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gokrazy/autoupdate/internal/cienv"
	"github.com/google/go-github/v35/github"
)

func hasLabel(ctx context.Context, client *github.Client, owner, repo string, issueNum int, label string) bool {
	labels, _, err := client.Issues.ListLabelsByIssue(ctx, owner, repo, issueNum, nil)
	if err != nil {
		log.Print(err)
		return false
	}
	for _, l := range labels {
		if *l.Name == label {
			log.Printf("gokr-has-label %s? %v", label, true)
			return true
		}
	}
	log.Printf("gokr-has-label %s? %v", label, false)
	return false
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

	if flag.NArg() < 1 {
		log.Fatal("syntax: gokr-has-label <label>")
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

	if hasLabel(ctx, client, parts[0], parts[1], issueNum, flag.Arg(0)) {
		os.Exit(0)
	}
	os.Exit(1)
}
