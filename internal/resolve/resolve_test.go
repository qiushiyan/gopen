package resolve

import "testing"

func TestBaseURL(t *testing.T) {
	for remote, want := range map[string]string{
		"git@github.com:acme/widget.git":          "https://github.com/acme/widget",
		"git@github.com:acme/widget":              "https://github.com/acme/widget",
		"git@github.com-personal:acme/widget.git": "https://github.com/acme/widget",
		"git@github.com-work_2:acme/widget.git":   "https://github.com/acme/widget",
		"ssh://git@github.com/acme/widget.git":    "https://github.com/acme/widget",
		"https://github.com/acme/widget.git":      "https://github.com/acme/widget",
		"https://github.com/acme/widget/":         "https://github.com/acme/widget",
		"https://github.com/acme/widget.git/\n":   "https://github.com/acme/widget",
		"git@gitlab.com:acme/widget.git":          "",
		"https://github.example.com/acme/widget":  "",
		"https://github.com/acme":                 "",
		"https://github.com/acme/widget/tree/x":   "",
		"/srv/git/widget.git":                     "",
	} {
		got, ok := BaseURL(remote)
		if got != want || ok != (want != "") {
			t.Errorf("BaseURL(%q) = %q, %v; want %q", remote, got, ok, want)
		}
	}
}

func TestSplitLines(t *testing.T) {
	type want struct {
		path       string
		start, end int
		err        bool
	}
	for arg, w := range map[string]want{
		"main.go":        {"main.go", 0, 0, false},
		"main.go:42":     {"main.go", 42, 42, false},
		"main.go:10-20":  {"main.go", 10, 20, false},
		"a:b/main.go:7":  {"a:b/main.go", 7, 7, false},
		"notes:draft":    {"notes:draft", 0, 0, false},
		"main.go:":       {"main.go:", 0, 0, false},
		"main.go:0":      {"", 0, 0, true},
		"main.go:20-10":  {"", 0, 0, true},
		":12":            {"", 0, 0, true},
		"main.go:10-":    {"main.go:10-", 0, 0, false},
		"/abs/x.go:3-3":  {"/abs/x.go", 3, 3, false},
		"dir/sub/:12-13": {"dir/sub/", 12, 13, false},
	} {
		p, s, e, err := SplitLines(arg)
		if p != w.path || s != w.start || e != w.end || (err != nil) != w.err {
			t.Errorf("SplitLines(%q) = %q %d %d %v; want %+v", arg, p, s, e, err, w)
		}
	}
}

func TestCacheHit(t *testing.T) {
	const sha = "abc123"
	now := int64(1_000_000)
	cases := []struct {
		entry string
		url   string
		hit   bool
	}{
		{"abc123 999999 https://github.com/acme/widget/pull/7", "https://github.com/acme/widget/pull/7", true},
		{"abc123 999999 -", "", true},
		{"abc123 999400 -", "", false},               // exactly the TTL old
		{"def456 999999 -", "", false},               // tip moved
		{"abc123 https://example/pull/1", "", false}, // the old two-field form
		{"abc123 soon -", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		url, hit := CacheHit(c.entry, sha, now)
		if url != c.url || hit != c.hit {
			t.Errorf("CacheHit(%q) = %q %v; want %q %v", c.entry, url, hit, c.url, c.hit)
		}
	}
}

func TestRefURL(t *testing.T) {
	r := &Repo{Base: "https://github.com/acme/widget"}
	for _, c := range []struct {
		ref  string
		f    *File
		want string
	}{
		{"feat/x", nil, "https://github.com/acme/widget/tree/feat/x"},
		{"feat#1", nil, "https://github.com/acme/widget/tree/feat%231"},
		{"main", &File{Path: ""}, "https://github.com/acme/widget/tree/main"},
		{"main", &File{Path: "cmd", Dir: true}, "https://github.com/acme/widget/tree/main/cmd"},
		{"main", &File{Path: "a b/c.go", Start: 4, End: 4}, "https://github.com/acme/widget/blob/main/a%20b/c.go#L4"},
		{"main", &File{Path: "c.go", Start: 4, End: 9}, "https://github.com/acme/widget/blob/main/c.go#L4-L9"},
		{"main", &File{Path: "docs/README.md", Start: 2, End: 2}, "https://github.com/acme/widget/blob/main/docs/README.md?plain=1#L2"},
		{"main", &File{Path: "docs/README.md"}, "https://github.com/acme/widget/blob/main/docs/README.md"},
	} {
		if got := r.RefURL(c.ref, c.f); got != c.want {
			t.Errorf("RefURL(%q, %+v) = %q; want %q", c.ref, c.f, got, c.want)
		}
	}
	if got := r.CompareURL("feat/x"); got != "https://github.com/acme/widget/compare/feat/x?expand=1" {
		t.Errorf("CompareURL = %q", got)
	}
}
