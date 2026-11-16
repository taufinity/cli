package review

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRepoName(t *testing.T) {
	ctx := context.Background()
	cases := []struct{ name, url, want string }{
		{"github_ssh", "git@github.com:taufinity/cli.git", "taufinity/cli"},
		{"bitbucket_https", "https://user@bitbucket.org/acme/site.git", "acme/site"},
		{"gitlab_subgroup", "ssh://git@gitlab.com/group/sub/proj.git", "group/sub/proj"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newRepo(t)
			gitRun(t, dir, "remote", "add", "origin", c.url)
			if got := RepoName(ctx, dir); got != c.want {
				t.Fatalf("RepoName = %q, want %q", got, c.want)
			}
		})
	}
	t.Run("no_remote_uses_folder", func(t *testing.T) {
		dir := newRepo(t)
		if got, want := RepoName(ctx, dir), filepath.Base(dir); got != want {
			t.Fatalf("RepoName = %q, want %q", got, want)
		}
	})
}
