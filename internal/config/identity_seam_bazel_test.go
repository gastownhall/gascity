package config

import "testing"

func TestIdentitySeamBindingQualifiedIdentity(t *testing.T) {
	tests := []struct {
		name, binding, agent, want string
	}{
		{name: "bare", agent: "worker", want: "worker"},
		{name: "binding", binding: "pack", agent: "worker", want: "pack.worker"},
		{name: "empty agent", binding: "pack", want: "pack."},
		{name: "odd binding", binding: "pack.with.dot", agent: "worker/name", want: "pack.with.dot.worker/name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bindingQualifiedIdentity(tt.binding, tt.agent); got != tt.want {
				t.Fatalf("bindingQualifiedIdentity(%q, %q) = %q, want %q", tt.binding, tt.agent, got, tt.want)
			}
		})
	}
}

func TestIdentitySeamQualifiedIdentity(t *testing.T) {
	tests := []struct {
		name, dir, binding, agent, want string
	}{
		{name: "bare", agent: "worker", want: "worker"},
		{name: "dir", dir: "repo", agent: "worker", want: "repo/worker"},
		{name: "binding", binding: "pack", agent: "worker", want: "pack.worker"},
		{name: "dir and binding", dir: "repo", binding: "pack", agent: "worker", want: "repo/pack.worker"},
		{name: "deep dir", dir: "one/two", binding: "pack", agent: "worker", want: "one/two/pack.worker"},
		{name: "empty values", want: ""},
		{name: "odd values", dir: "/", binding: ".", agent: "/", want: "//../"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qualifiedIdentity(tt.dir, tt.binding, tt.agent); got != tt.want {
				t.Fatalf("qualifiedIdentity(%q, %q, %q) = %q, want %q", tt.dir, tt.binding, tt.agent, got, tt.want)
			}
		})
	}
}

func TestIdentitySeamParseQualifiedIdentity(t *testing.T) {
	tests := []struct {
		identity, wantDir, wantName string
	}{
		{identity: "worker", wantName: "worker"},
		{identity: "repo/worker", wantDir: "repo", wantName: "worker"},
		{identity: "repo/pack.worker", wantDir: "repo", wantName: "pack.worker"},
		{identity: "one/two/worker", wantDir: "one/two", wantName: "worker"},
		{identity: "", wantName: ""},
		{identity: "/worker", wantDir: "", wantName: "worker"},
		{identity: "worker/", wantDir: "worker", wantName: ""},
		{identity: "one//worker", wantDir: "one/", wantName: "worker"},
	}
	for _, tt := range tests {
		t.Run(tt.identity, func(t *testing.T) {
			gotDir, gotName := parseQualifiedIdentity(tt.identity)
			if gotDir != tt.wantDir || gotName != tt.wantName {
				t.Fatalf("parseQualifiedIdentity(%q) = (%q, %q), want (%q, %q)", tt.identity, gotDir, gotName, tt.wantDir, tt.wantName)
			}
		})
	}
}

func TestIdentitySeamAgentIdentityMatches(t *testing.T) {
	tests := []struct {
		name, dir, agent, binding, identity string
		want                                bool
	}{
		{name: "bare qualified", agent: "worker", identity: "worker", want: true},
		{name: "dir qualified", dir: "repo", agent: "worker", identity: "repo/worker", want: true},
		{name: "binding qualified", agent: "worker", binding: "pack", identity: "pack.worker", want: true},
		{name: "dir and binding qualified", dir: "repo", agent: "worker", binding: "pack", identity: "repo/pack.worker", want: true},
		{name: "V1 fallback", dir: "repo", agent: "worker", identity: "repo/worker", want: true},
		{name: "V1 bare fallback", agent: "worker", identity: "worker", want: true},
		{name: "V2 bare rejection", agent: "worker", binding: "pack", identity: "worker", want: false},
		{name: "wrong identity", dir: "repo", agent: "worker", identity: "other/worker", want: false},
		{name: "empty identity", identity: "", want: true},
		{name: "empty V2 identity", binding: "pack", identity: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentIdentityMatches(tt.dir, tt.agent, tt.binding, tt.identity); got != tt.want {
				t.Fatalf("agentIdentityMatches(%q, %q, %q, %q) = %v, want %v", tt.dir, tt.agent, tt.binding, tt.identity, got, tt.want)
			}
		})
	}
}

func TestIdentitySeamQualifiedParseConsistency(t *testing.T) {
	for _, tt := range []struct {
		dir, binding, agent string
	}{
		{dir: "", binding: "", agent: "worker"},
		{dir: "repo", binding: "", agent: "worker"},
		{dir: "", binding: "pack", agent: "worker"},
		{dir: "repo/sub", binding: "pack", agent: "worker"},
		{dir: "", binding: "", agent: ""},
	} {
		identity := qualifiedIdentity(tt.dir, tt.binding, tt.agent)
		gotDir, gotName := parseQualifiedIdentity(identity)
		wantName := bindingQualifiedIdentity(tt.binding, tt.agent)
		if gotDir != tt.dir || gotName != wantName {
			t.Errorf("qualifiedIdentity(%q, %q, %q) = %q; parse = (%q, %q), want (%q, %q)", tt.dir, tt.binding, tt.agent, identity, gotDir, gotName, tt.dir, wantName)
		}
	}
}
