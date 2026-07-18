package supervisor

import "testing"

func TestParseOwnerRepo(t *testing.T) {
	cases := []struct {
		url, owner, repo string
	}{
		{"git@codeberg.org:Elysium_Labs/eos.git", "Elysium_Labs", "eos"},
		{"git@codeberg.org:Elysium_Labs/eos", "Elysium_Labs", "eos"},
		{"https://codeberg.org/Elysium_Labs/eos.git", "Elysium_Labs", "eos"},
		{"https://codeberg.org/Elysium_Labs/eos", "Elysium_Labs", "eos"},
		{"ssh://git@codeberg.org/Elysium_Labs/eos.git", "Elysium_Labs", "eos"},
	}
	for _, tc := range cases {
		owner, repo, err := parseOwnerRepo(tc.url)
		if err != nil {
			t.Errorf("%s: %v", tc.url, err)
			continue
		}
		if owner != tc.owner || repo != tc.repo {
			t.Errorf("%s: got %s/%s want %s/%s", tc.url, owner, repo, tc.owner, tc.repo)
		}
	}
}

func TestParseOwnerRepoRejectsGarbage(t *testing.T) {
	if _, _, err := parseOwnerRepo("not-a-url"); err == nil {
		t.Fatal("want error for unparseable remote")
	}
}
