// gokr-amend is a tool to amend GitHub pull requests, to be used in
// continuous integration runs (e.g. on travis) to include build
// results.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gokrazy/autoupdate/internal/cienv"
	"github.com/google/go-github/v35/github"
)

var (
	setLabel = flag.String("set_label",
		"",
		"if non-empty, name of a GitHub label to set on the pull request")
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

func addLabel(ctx context.Context, client *github.Client, owner, repo string, issueNum int, label string) error {
	found, err := ensureLabel(ctx, client, owner, repo, issueNum, label)
	if err != nil {
		return err
	}
	if found {
		return nil
	}

	_, _, err = client.Issues.AddLabelsToIssue(ctx, owner, repo, issueNum, []string{*setLabel})
	return err
}

// updatePullRequest corresponds to the following git CLI operations:
//
// 1. git add <files>
// 2. git commit --amend
// 3. git push -f
func updatePullRequest(ctx context.Context, client *github.Client, owner, repo, branch string, files []string, issueNum int, label string) error {
	dir, err := ioutil.TempDir("", "gokr-amend")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	kernel := filepath.Join(dir, "kernel")

	clone := exec.CommandContext(ctx,
		"git",
		"clone",
		"--branch="+branch,
		"--depth=2", // just enough for git commit --amend
		"https://"+githubUser+":"+authToken+"@github.com/"+owner+"/"+repo,
		kernel)
	clone.Stdout = os.Stdout
	clone.Stderr = os.Stderr
	if err := clone.Run(); err != nil {
		return fmt.Errorf("%v: %v", clone.Args, err)
	}

	git := func(args ...string) error {
		log.Printf("git %v", args)
		cmd := exec.CommandContext(ctx,
			"git",
			args...)
		cmd.Dir = kernel
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%v: %v", clone.Args, err)
		}
		return nil
	}

	rsync := exec.CommandContext(ctx,
		"rsync",
		append(append([]string{
			"--delete",
			"-av",
		}, files...),
			kernel)...)
	rsync.Stdout = os.Stdout
	rsync.Stderr = os.Stderr
	if err := rsync.Run(); err != nil {
		return fmt.Errorf("%v: %v", clone.Args, err)
	}

	var stdout bytes.Buffer
	status := exec.CommandContext(ctx,
		"git",
		"status",
		"--short")
	status.Dir = kernel
	status.Stdout = &stdout
	status.Stderr = os.Stderr
	if err := status.Run(); err != nil {
		return fmt.Errorf("%v: %v", clone.Args, err)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		log.Printf("all files equal, nothing to amend")
		if label != "" {
			if err := addLabel(ctx, client, owner, repo, issueNum, label); err != nil {
				return err
			}
		}
		return nil
	}

	if err := git("add", "."); err != nil {
		return err
	}

	if err := git("commit", "-a", "--amend", "--no-edit"); err != nil {
		return err
	}
	if err := git("push", "-f", "origin", branch); err != nil {
		return err
	}

	return nil
}

var (
	githubUser              = cienv.MustGetGithubUser()
	authToken               = cienv.MustGetAuthToken()
	slug                    = cienv.MustGetSlug()
	travisPullRequest       = cienv.MustGetPullRequest()
	travisPullRequestBranch = cienv.MustGetPullRequestBranch()
)

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	parts := strings.Split(slug, "/")
	if got, want := len(parts), 2; got != want {
		log.Fatalf("unexpected number of /-separated parts in %q: got %d, want %d", slug, got, want)
	}

	client := github.NewClient(&http.Client{
		Transport: &github.BasicAuthTransport{
			Username: githubUser,
			Password: authToken,
		},
	})

	issueNum, err := strconv.ParseInt(travisPullRequest, 0, 64)
	if err != nil {
		log.Fatal(err)
	}

	if err := updatePullRequest(context.Background(), client, parts[0], parts[1], travisPullRequestBranch, flag.Args(), int(issueNum), *setLabel); err != nil {
		log.Fatal(err)
	}
}
