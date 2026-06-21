package dolttest

import "testing"

func TestDoltConfigPath(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"space form", "/usr/bin/dolt sql-server --config /tmp/x/cfg.yaml", "/tmp/x/cfg.yaml"},
		{"equals form", "dolt sql-server --config=/a/b/cfg.yaml", "/a/b/cfg.yaml"},
		{"missing value", "dolt sql-server --config", ""},
		{"no config flag", "dolt sql-server --port 3306", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doltConfigPath(tc.cmd); got != tc.want {
				t.Fatalf("doltConfigPath(%q) = %q, want %q", tc.cmd, got, tc.want)
			}
		})
	}
}

func TestLooksLikeDoltSQLServer(t *testing.T) {
	cases := []struct {
		fields []string
		want   bool
	}{
		{[]string{"dolt", "sql-server"}, true},
		{[]string{"/usr/local/bin/dolt", "sql-server", "--config", "x"}, true},
		{[]string{"dolt", "status"}, false},
		{[]string{"sql-server"}, false},
		{[]string{"notdolt", "sql-server"}, false},
	}
	for _, tc := range cases {
		if got := looksLikeDoltSQLServer(tc.fields); got != tc.want {
			t.Fatalf("looksLikeDoltSQLServer(%v) = %v, want %v", tc.fields, got, tc.want)
		}
	}
}

func TestPathWithin(t *testing.T) {
	cases := []struct {
		root, p string
		want    bool
	}{
		{"/tmp/gcac/run", "/tmp/gcac/run/gc-home/cfg", true},
		{"/tmp/gcac/run", "/tmp/gcac/run", true},
		{"/tmp/gcac/run", "/tmp/gcac/other/cfg", false},
		{"/tmp/gcac/run", "/tmp/gcac/run-sibling/cfg", false}, // prefix-but-not-child
		{"/", "/tmp/x", false},
		{"", "/tmp/x", false},
		{"/tmp/x", "", false},
	}
	for _, tc := range cases {
		if got := pathWithin(tc.root, tc.p); got != tc.want {
			t.Fatalf("pathWithin(%q,%q) = %v, want %v", tc.root, tc.p, got, tc.want)
		}
	}
}

func TestRunDirUnder(t *testing.T) {
	dir, ok := runDirUnder("/tmp/gcac/gcac-123-abc/gc-home/.dolt/cfg", "/tmp/gcac", "gcac-")
	if !ok || dir != "/tmp/gcac/gcac-123-abc" {
		t.Fatalf("runDirUnder match = (%q,%v), want (/tmp/gcac/gcac-123-abc,true)", dir, ok)
	}
	if _, ok := runDirUnder("/tmp/gcac/other-123/x", "/tmp/gcac", "gcac-"); ok {
		t.Fatalf("runDirUnder wrong-prefix should not match")
	}
	if _, ok := runDirUnder("/tmp/elsewhere/gcac-1/x", "/tmp/gcac", "gcac-"); ok {
		t.Fatalf("runDirUnder outside-parent should not match")
	}
	if _, ok := runDirUnder("", "/tmp/gcac", "gcac-"); ok {
		t.Fatalf("runDirUnder empty config should not match")
	}
}

func TestOwnerPIDFromRunDir(t *testing.T) {
	if pid, ok := ownerPIDFromRunDir("gcac-12345-678901", "gcac-"); !ok || pid != 12345 {
		t.Fatalf("ownerPIDFromRunDir = (%d,%v), want (12345,true)", pid, ok)
	}
	if _, ok := ownerPIDFromRunDir("gcwi-9-x", "gcac-"); ok {
		t.Fatalf("wrong prefix should not parse")
	}
	if _, ok := ownerPIDFromRunDir("gcac-notapid-x", "gcac-"); ok {
		t.Fatalf("non-numeric pid should not parse")
	}
}
