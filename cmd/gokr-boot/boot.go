package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/gokrazy/internal/config"
	"github.com/google/go-github/v35/github"
	"github.com/google/renameio/v2"
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

	updateRootFlag = flag.Bool("update_root",
		false,
		"update bakery root file system, too? required for gokrazy/kernel with loadable kernel modules")
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

func writeImages(hostname string) (boot string, root string, _ error) {
	log.Printf("writeImages(%s)", hostname)
	bootf, err := ioutil.TempFile("", "gokr-boot")
	if err != nil {
		return "", "", err
	}
	bootf.Close()
	rootf, err := ioutil.TempFile("", "gokr-root")
	if err != nil {
		return "", "", err
	}
	rootf.Close()
	// Inject the hostname into the instance config.
	cfg, err := config.ReadFromFile()
	if err != nil {
		return "", "", err
	}
	cfg.Hostname = hostname
	b, err := cfg.FormatForFile()
	if err != nil {
		return "", "", err
	}
	if err := renameio.WriteFile(config.InstanceConfigPath(), b, 0644); err != nil {
		return "", "", err
	}
	cmd := exec.Command("gok",
		"overwrite",
		"--boot="+bootf.Name(),
		"--root="+rootf.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return bootf.Name(), rootf.Name(), cmd.Run()
}

func useBakeries(booteryURL, slug string) ([]string, error) {
	u, err := url.Parse(booteryURL)
	if err != nil {
		return nil, err
	}
	v := u.Query()
	v.Set("slug", slug)
	u.RawQuery = v.Encode()
	req, err := http.NewRequest(http.MethodPut, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		b, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected HTTP status code: got %d (%s), want %d", got, strings.TrimSpace(string(b)), want)
	}
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var useReply struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal(b, &useReply); err != nil {
		return nil, err
	}
	return useReply.Hosts, nil
}

func releaseBakeries(booteryURL string) error {
	req, err := http.NewRequest(http.MethodPut, booteryURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		b, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("unexpected HTTP status code: got %d (%s), want %d", got, strings.TrimSpace(string(b)), want)
	}
	return nil
}

func streamTo(img, booteryURL, hostname, newer string) (string, error) {
	f, err := os.Open(img)
	if err != nil {
		return "", err
	}
	defer f.Close()
	u, err := url.Parse(booteryURL)
	if err != nil {
		return "", err
	}
	v := u.Query()
	v.Set("hostname", hostname)
	if newer != "" {
		v.Set("boot-newer", newer)
	}
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

func testBoot(bootImg, booteryURL, hostname, newer string) (string, error) {
	return streamTo(bootImg, booteryURL, hostname, newer)
}

func updateRoot(rootImg, booteryURL, hostname string) (string, error) {
	return streamTo(rootImg, strings.TrimSuffix(booteryURL, "/testboot")+"/updateroot", hostname, "")
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

func testBoot1(hostname, newer string) (string, error) {
	bootImg, rootImg, err := writeImages(hostname)
	if err != nil {
		return "", err
	}
	defer os.Remove(bootImg)
	defer os.Remove(rootImg)

	if *updateRootFlag {
		log.Printf("updating root file system")
		if _, err := updateRoot(rootImg, *booteryURL, hostname); err != nil {
			return "", errors.New(strings.Replace(err.Error(), *booteryURL, "<bootery_url>", -1))
		}
	}

	log.Printf("testing boot file system")
	bootlog, err := testBoot(bootImg, strings.TrimSuffix(*booteryURL, "/testboot")+"/testboot1"+fmt.Sprintf("?update_root=%v", *updateRootFlag), hostname, newer)
	if err != nil {
		return "", errors.New(strings.Replace(err.Error(), *booteryURL, "<bootery_url>", -1))
	}
	return bootlog, nil
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

	// Subtract a second to ensure the gokrazy build timestamp is different
	// (UNIX timestamps use seconds as their granularity).
	newer := strconv.FormatInt(time.Now().Unix()-1, 10)

	// Power on bakeries and expand slug into hostnames
	booteryBase := strings.TrimSuffix(*booteryURL, "/testboot")
	hosts, err := useBakeries(booteryBase+"/usebakeries", slug)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := releaseBakeries(booteryBase + "/releasebakeries"); err != nil {
			log.Fatal(err)
		}
	}()

	log.Printf("updating hosts %q", hosts)
	for _, host := range hosts {
		bootlog, err := testBoot1(host, newer)
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
	}

	if err := addLabel(ctx, client, parts[0], parts[1], issueNum, *setLabel); err != nil {
		log.Fatal(err)
	}

	if err := removeLabel(ctx, client, parts[0], parts[1], issueNum, *requireLabel); err != nil {
		log.Fatal(err)
	}
}
