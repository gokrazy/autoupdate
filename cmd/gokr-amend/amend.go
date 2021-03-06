// gokr-amend is a tool to amend GitHub pull requests, to be used in
// continuous integration runs (e.g. on travis) to include build
// results.
package main

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
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
	lastRef, _, err := client.Git.GetRef(ctx, owner, repo, "heads/"+branch)
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

	hashByName := make(map[string]string, len(baseTree.Entries))
	for _, e := range baseTree.Entries {
		hashByName[*e.Path] = *e.SHA
	}

	entries := make([]*github.TreeEntry, 0, len(files))
	for _, fn := range files {
		hash := sha1.New()
		f, err := os.Open(fn)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(hash, "blob %d\x00", st.Size()); err != nil {
			return err
		}
		b, err := ioutil.ReadAll(io.TeeReader(f, hash))
		if err != nil {
			return err
		}
		if local, remote := fmt.Sprintf("%x", hash.Sum(nil)), hashByName[fn]; local != remote {
			log.Printf("%s differs (local %s, remote %s)", fn, local, remote)

			blob, _, err := client.Git.CreateBlob(ctx, owner, repo, &github.Blob{
				Content:  github.String(base64.StdEncoding.EncodeToString(b)),
				Encoding: github.String("base64"),
			})
			if err != nil {
				return err
			}

			if got, want := *blob.SHA, local; got != want {
				return fmt.Errorf("blob creation failed: invalid SHA hash: got %s, want %s", got, want)
			}

			entries = append(entries, &github.TreeEntry{
				Path: github.String(fn),
				Mode: github.String("100644"),
				Type: github.String("blob"),
				SHA:  github.String(*blob.SHA),
			})
		}
	}

	if len(entries) == 0 {
		log.Printf("all files equal, nothing to amend")
		if label != "" {
			if err := addLabel(ctx, client, owner, repo, issueNum, label); err != nil {
				return err
			}
		}
		return nil
	}

	newTree, _, err := client.Git.CreateTree(ctx, owner, repo, *baseTree.SHA, entries)
	if err != nil {
		return err
	}
	log.Printf("newTree = %+v", newTree)

	lastCommit.Tree = newTree

	newCommit, _, err := client.Git.CreateCommit(ctx, owner, repo, lastCommit)
	if err != nil {
		return err
	}
	log.Printf("newCommit = %+v", newCommit)

	if label != "" {
		if err := addLabel(ctx, client, owner, repo, issueNum, label); err != nil {
			return err
		}
	}

	newRef, _, err := client.Git.UpdateRef(ctx, owner, repo, &github.Reference{
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
