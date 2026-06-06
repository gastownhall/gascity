package t3bridge

import "testing"

func TestAllowThreadReuse_NamedFreshStillReusesThread(t *testing.T) {
	if !allowThreadReuse(AgentKindNamed, "fresh") {
		t.Fatal("named fresh sessions should still reuse their T3 thread")
	}
}

func TestAllowThreadReuse_PoolDoesNotReuseThread(t *testing.T) {
	if allowThreadReuse(AgentKindPool, "sticky") {
		t.Fatal("pool sessions should not reuse one shared T3 thread")
	}
}
