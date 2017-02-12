// gokr-merge merges GitHub pull requests with the right labels.
package main

import (
	"flag"
	"log"
	"os"
)

var (
	requireLabel = flag.String("require_label",
		"",
		"name of the required label before the PR will be merged")
)

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if *requireLabel == "" {
		log.Fatal("-require_label is a required flag")
	}

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

	log.Printf("TODO: implement")

	os.Exit(2) // label not present
}
