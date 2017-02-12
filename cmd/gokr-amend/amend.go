// gokr-amend is a tool to amend GitHub pull requests, to be used in
// continuous integration runs (e.g. on travis) to include build
// results.
package main

import (
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-github/github"
)

// updatePullRequest corresponds to the following git CLI operations:
//
// 1. git add <files>
// 2. git commit --amend
// 3. git push -f
func updatePullRequest(client *github.Client, owner, repo, branch string, files []string) error {
	lastRef, _, err := client.Git.GetRef(owner, repo, "heads/"+branch)
	if err != nil {
		return err
	}

	lastCommit, _, err := client.Git.GetCommit(owner, repo, *lastRef.Object.SHA)
	if err != nil {
		return err
	}

	log.Printf("lastCommit = %+v", lastCommit)

	baseTree, _, err := client.Git.GetTree(owner, repo, *lastCommit.SHA, true)
	if err != nil {
		return err
	}
	log.Printf("baseTree = %+v", baseTree)

	entries := make([]github.TreeEntry, len(files))
	for idx, fn := range files {
		b, err := ioutil.ReadFile(fn)
		if err != nil {
			return err
		}
		entries[idx] = github.TreeEntry{
			Path:    github.String(fn),
			Mode:    github.String("100644"),
			Type:    github.String("blob"),
			Content: github.String(string(b)),
		}
	}

	newTree, _, err := client.Git.CreateTree(owner, repo, *baseTree.SHA, entries)
	if err != nil {
		return err
	}
	log.Printf("newTree = %+v", newTree)

	lastCommit.Tree = newTree

	newCommit, _, err := client.Git.CreateCommit(owner, repo, lastCommit)
	if err != nil {
		return err
	}
	log.Printf("newCommit = %+v", newCommit)

	newRef, _, err := client.Git.UpdateRef(owner, repo, &github.Reference{
		Ref: github.String("refs/heads/" + branch),
		Object: &github.GitObject{
			SHA: newCommit.SHA,
		},
	}, true)
	if err != nil {
		return err
	}
	log.Printf("newRef = %+v", newRef)

	return nil
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	for _, name := range []string{
		"TRAVIS_PULL_REQUEST_BRANCH",
		"TRAVIS_REPO_SLUG",
		"GITHUB_USER",
		"GITHUB_AUTH_TOKEN",
	} {
		if os.Getenv(name) == "" {
			log.Fatalf("required environment variable %q empty", name)
		}
	}

	branch := os.Getenv("TRAVIS_PULL_REQUEST_BRANCH")
	slug := os.Getenv("TRAVIS_REPO_SLUG")

	parts := strings.Split(slug, "/")
	if got, want := len(parts), 2; got != want {
		log.Fatalf("unexpected number of /-separated parts in %q: got %d, want %d", slug, got, want)
	}

	client := github.NewClient(&http.Client{
		Transport: &github.BasicAuthTransport{
			Username: os.Getenv("GITHUB_USER"),
			Password: os.Getenv("GITHUB_AUTH_TOKEN"),
		},
	})

	if err := updatePullRequest(client, parts[0], parts[1], branch, flag.Args()); err != nil {
		log.Fatal(err)
	}
}
