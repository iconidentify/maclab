package server

import "testing"

func TestParseSource(t *testing.T) {
	cases := []struct{ in, repo, ref string }{
		{"https://github.com/iconidentify/aurora-linux", "https://github.com/iconidentify/aurora-linux", ""},
		{"https://github.com/iconidentify/aurora-linux/tree/custom/sep", "https://github.com/iconidentify/aurora-linux", "custom/sep"},
		{"github.com/AsahiLinux/linux/tree/asahi-wip", "https://github.com/AsahiLinux/linux", "asahi-wip"},
		{"https://github.com/o/r/commit/17cba00e43b94ba6b5c64be7cbe3db9234a41f84", "https://github.com/o/r", "17cba00e43b94ba6b5c64be7cbe3db9234a41f84"},
		{"https://github.com/o/r/pull/42", "https://github.com/o/r", "refs/pull/42/head"},
		{"https://github.com/o/r.git#dev", "https://github.com/o/r", "dev"},
		{"https://git.example.org/linux.git@v7.1", "https://git.example.org/linux.git", "v7.1"},
		{"https://git.example.org/linux.git#topic/x", "https://git.example.org/linux.git", "topic/x"},
	}
	for _, c := range cases {
		s, err := parseSource(c.in)
		if err != nil || s.Repo != c.repo || s.Ref != c.ref {
			t.Errorf("%s: got %q %q %v, want %q %q", c.in, s.Repo, s.Ref, err, c.repo, c.ref)
		}
	}
}
