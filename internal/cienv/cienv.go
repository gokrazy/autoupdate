package cienv

import (
	"log"
	"os"
)

func MustGetGithubUser() string {
	githubUser := os.Getenv("GITHUB_USER") // Travis CI
	if githubUser == "" {
		githubUser = os.Getenv("GH_USER") // GitHub actions
	}
	if githubUser == "" {
		log.Fatal("required environment variable GITHUB_USER (or GH_USER) empty")
	}
	return githubUser
}

func MustGetAuthToken() string {
	authToken := os.Getenv("GITHUB_AUTH_TOKEN") // Travis CI
	if authToken == "" {
		authToken = os.Getenv("GH_AUTH_TOKEN") // GitHub actions
	}
	if authToken == "" {
		log.Fatal("required environment variable GITHUB_AUTH_TOKEN (or GH_AUTH_TOKEN) empty")
	}
	return authToken
}

func MustGetSlug() string {
	slug := os.Getenv("TRAVIS_REPO_SLUG") // Travis CI
	if slug == "" {
		slug = os.Getenv("GITHUB_REPOSITORY") // GitHub actions
	}
	if slug == "" {
		log.Fatal("required environment variable TRAVIS_REPO_SLUG (or GITHUB_REPOSITORY) empty")
	}
	return slug
}

func MustGetPullRequest() string {
	pullRequest := os.Getenv("TRAVIS_PULL_REQUEST")
	if pullRequest == "" {
		log.Fatal("required environment variable TRAVIS_PULL_REQUEST empty")
	}
	return pullRequest
}

func MustGetPullRequestBranch() string {
	pullRequestBranch := os.Getenv("TRAVIS_PULL_REQUEST_BRANCH")
	if pullRequestBranch == "" {
		log.Fatal("required environment variable TRAVIS_PULL_REQUEST_BRANCH empty")
	}
	return pullRequestBranch
}
