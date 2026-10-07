package main

import "testing"

func TestCheckRemoteRefusesCredentials(t *testing.T) {
	for url, ok := range map[string]bool{
		"git@github.com:owner/repo.git":                 true,
		"https://github.com/owner/repo.git":             true,
		"ssh://git@github.com/owner/repo.git":           true,
		"/srv/git/repo.git":                             true,
		"file:///srv/git/repo.git":                      true,
		"https://x-access-token:ghs_abc@github.com/o/r": false,
		"https://ghp_abc@github.com/o/r.git":            false,
		"ssh://git:hunter2@example.com/o/r.git":         false,
	} {
		if err := checkRemote(url); (err == nil) != ok {
			t.Errorf("checkRemote(%q) = %v, want ok=%v", url, err, ok)
		}
	}
}
