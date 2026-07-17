package resourcecensus

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestValidateReviewedHermeticBodiesRequiresExactUniqueUntaggedTest(t *testing.T) {
	t.Parallel()

	census := scanHermeticFixture(t, fstest.MapFS{
		"sample/owned_test.go": &fstest.MapFile{Data: []byte(`package sample
import "testing"
func TestOwned(t *testing.T) {}
`)},
		"sample/tagged_test.go": &fstest.MapFile{Data: []byte(`//go:build integration

package sample
import "testing"
func TestTagged(t *testing.T) {}
`)},
	})
	valid := validReviewedHermeticBody("TestOwned")
	if err := validateReviewedHermeticBodies([]ReviewedHermeticBody{valid}, census); err != nil {
		t.Fatalf("validateReviewedHermeticBodies(valid): %v", err)
	}

	tests := []struct {
		name string
		rows []ReviewedHermeticBody
		want string
	}{
		{name: "missing identity", rows: []ReviewedHermeticBody{{EffectiveSize: "medium", MediumReason: "package TestMain mutates process state"}}, want: "package_dir is required"},
		{name: "stale owner", rows: []ReviewedHermeticBody{withHermeticOwner(valid, "TestRemoved")}, want: "runnable owner does not exist"},
		{name: "duplicate row", rows: []ReviewedHermeticBody{valid, valid}, want: "duplicate reviewed hermetic body"},
		{name: "tagged owner", rows: []ReviewedHermeticBody{withHermeticOwner(valid, "TestTagged")}, want: "must be untagged"},
		{name: "wildcard owner", rows: []ReviewedHermeticBody{withHermeticOwner(valid, "Test*")}, want: "wildcard"},
		{name: "dishonest small effective size", rows: []ReviewedHermeticBody{withHermeticSize(valid, "small")}, want: "effective_size"},
		{name: "missing effective size", rows: []ReviewedHermeticBody{withHermeticSize(valid, "")}, want: "effective_size"},
		{name: "missing medium reason", rows: []ReviewedHermeticBody{withHermeticReason(valid, " \t")}, want: "medium_reason is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			requireErrorContains(t, validateReviewedHermeticBodies(tt.rows, census), tt.want)
		})
	}
}

func TestValidateReviewedHermeticRowsAgainstPolicyRequiresExactRows(t *testing.T) {
	t.Parallel()

	want := validReviewedHermeticBody("TestOwned")
	for _, tt := range []struct {
		name   string
		rows   []ReviewedHermeticBody
		mutate func(*ReviewedHermeticBody)
		match  string
	}{
		{name: "missing", match: "missing required reviewed hermetic body"},
		{name: "unexpected", rows: []ReviewedHermeticBody{validReviewedHermeticBody("TestOther")}, match: "unexpected reviewed hermetic body"},
		{name: "duplicate", rows: []ReviewedHermeticBody{want, want}, match: "duplicate reviewed hermetic body"},
		{name: "package dir drift", rows: []ReviewedHermeticBody{want}, mutate: func(row *ReviewedHermeticBody) { row.PackageDir = "other" }, match: "package_dir"},
		{name: "package name drift", rows: []ReviewedHermeticBody{want}, mutate: func(row *ReviewedHermeticBody) { row.PackageName = "other" }, match: "package_name"},
		{name: "owner drift", rows: []ReviewedHermeticBody{want}, mutate: func(row *ReviewedHermeticBody) { row.Owner = "TestOther" }, match: "owner"},
		{name: "effective size drift", rows: []ReviewedHermeticBody{want}, mutate: func(row *ReviewedHermeticBody) { row.EffectiveSize = "small" }, match: "effective_size"},
		{name: "medium reason drift", rows: []ReviewedHermeticBody{want}, mutate: func(row *ReviewedHermeticBody) { row.MediumReason = "other setup" }, match: "medium_reason"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rows := append([]ReviewedHermeticBody(nil), tt.rows...)
			if tt.mutate != nil {
				tt.mutate(&rows[0])
			}
			requireErrorContains(t, errorsFromProblems(validateReviewedHermeticRowsAgainstPolicy([]ReviewedHermeticBody{want}, rows)), tt.match)
		})
	}
}

func TestValidateAgainstPolicyWiresReviewedHermeticPolicyAndSourceChecks(t *testing.T) {
	t.Parallel()

	row := validReviewedHermeticBody("TestOwned")
	policy := Ledger{Version: 2, ReviewedHermeticBody: []ReviewedHermeticBody{row}}
	clean := scanHermeticFixture(t, fstest.MapFS{
		"sample/owned_test.go": &fstest.MapFile{Data: []byte(`package sample
import "testing"
func TestOwned(t *testing.T) {}
`)},
	})

	t.Run("manifest drift is rejected before source validation", func(t *testing.T) {
		ledger := policy
		ledger.ReviewedHermeticBody = append([]ReviewedHermeticBody(nil), policy.ReviewedHermeticBody...)
		ledger.ReviewedHermeticBody[0].EffectiveSize = "small"
		err := validateAgainstPolicy(policy, ledger, clean, time.Time{})
		requireErrorContains(t, err, `bootstrap policy requires "medium"`)
	})

	t.Run("reachable resource is rejected through production wiring", func(t *testing.T) {
		withResource := scanHermeticFixture(t, fstest.MapFS{
			"sample/owned_test.go": &fstest.MapFile{Data: []byte(`package sample
import (
	"testing"
	"time"
)
func TestOwned(t *testing.T) { helper() }
func helper() { time.Sleep(0) }
`)},
		})
		err := validateAgainstPolicy(policy, policy, withResource, time.Time{})
		requireErrorContains(t, err, string(ResourceFixedSleep))
	})
}

func TestValidateReviewedHermeticBodiesRejectsDirectKnownResources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		imports      string
		declarations string
		body         string
		resource     Resource
	}{
		{name: "subprocess", imports: `"os/exec"`, body: `_ = exec.Command("worker")`, resource: ResourceSubprocess},
		{name: "fixed sleep", imports: `"time"`, body: `time.Sleep(0)`, resource: ResourceFixedSleep},
		{name: "environment", body: `t.Setenv("KEY", "value")`, resource: ResourceEnvironment},
		{name: "cwd", body: `t.Chdir("work")`, resource: ResourceCWD},
		{
			name:         "slow process gate",
			declarations: `func skipSlowCmdGCTest(t *testing.T, reason string) {}`,
			body:         `skipSlowCmdGCTest(t, "process-backed")`,
			resource:     ResourceSlowProcessGate,
		},
		{name: "HTTP test server", imports: `"net/http/httptest"`, body: `_ = httptest.NewServer(nil)`, resource: ResourceHTTPTestServer},
		{name: "net listen", imports: `"net"`, body: `_, _ = net.Listen("tcp", "127.0.0.1:0")`, resource: ResourceNetListen},
		{name: "net listen config", imports: `"net"`, body: `_, _ = (net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")`, resource: ResourceNetListenConfig},
		{name: "net listen unixgram", imports: `"net"`, body: `_, _ = net.ListenUnixgram("unixgram", nil)`, resource: ResourceNetListenUnixgram},
		{name: "syscall listen", imports: `"syscall"`, body: `_ = syscall.Listen(0, 0)`, resource: ResourceSyscallListen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			source := fmt.Sprintf("package sample\nimport (\n\t\"testing\"\n\t%s\n)\n%s\nfunc resourceHelper(t *testing.T) { %s }\nfunc TestHermetic(t *testing.T) { resourceHelper(t) }\n", tt.imports, tt.declarations, tt.body)
			census := scanHermeticFixture(t, fstest.MapFS{
				"sample/resource_test.go": &fstest.MapFile{Data: []byte(source)},
			})
			err := validateReviewedHermeticBodies([]ReviewedHermeticBody{validReviewedHermeticBody("TestHermetic")}, census)
			requireErrorContains(t, err, string(tt.resource))
		})
	}
}

func TestValidateReviewedHermeticBodiesFollowsHelpersWithoutShadowFalseMatches(t *testing.T) {
	t.Parallel()

	t.Run("helper chain cycle reports resource once", func(t *testing.T) {
		census := scanHermeticFixture(t, fstest.MapFS{
			"sample/resource_test.go": &fstest.MapFile{Data: []byte(`package sample
import (
	"testing"
	"time"
)
func TestHermetic(t *testing.T) { helperA() }
func helperA() { helperB() }
func helperB() { time.Sleep(0); helperA() }
`)},
		})
		err := validateReviewedHermeticBodies([]ReviewedHermeticBody{validReviewedHermeticBody("TestHermetic")}, census)
		requireErrorContains(t, err, string(ResourceFixedSleep))
		if got := strings.Count(err.Error(), string(ResourceFixedSleep)); got != 1 {
			t.Fatalf("fixed_sleep reports = %d, want 1; err=%v", got, err)
		}
	})

	t.Run("local shadow does not reach package helper", func(t *testing.T) {
		census := scanHermeticFixture(t, fstest.MapFS{
			"sample/helper_test.go": &fstest.MapFile{Data: []byte(`package sample
import "time"
func helper() { time.Sleep(0) }
`)},
			"sample/owned_test.go": &fstest.MapFile{Data: []byte(`package sample
import "testing"
func TestHermetic(t *testing.T) {
	helper := func() {}
	helper()
}
`)},
		})
		if err := validateReviewedHermeticBodies([]ReviewedHermeticBody{validReviewedHermeticBody("TestHermetic")}, census); err != nil {
			t.Fatalf("validateReviewedHermeticBodies(local shadow): %v", err)
		}
	})

	t.Run("clean cross-file helper passes", func(t *testing.T) {
		census := scanHermeticFixture(t, fstest.MapFS{
			"sample/helper_test.go": &fstest.MapFile{Data: []byte(`package sample
func helper() { nested() }
func nested() {}
`)},
			"sample/owned_test.go": &fstest.MapFile{Data: []byte(`package sample
import "testing"
func TestHermetic(t *testing.T) { helper() }
`)},
		})
		if err := validateReviewedHermeticBodies([]ReviewedHermeticBody{validReviewedHermeticBody("TestHermetic")}, census); err != nil {
			t.Fatalf("validateReviewedHermeticBodies(clean helper): %v", err)
		}
	})

	t.Run("cross-file helper reports deterministic call chain", func(t *testing.T) {
		fixture := func() fstest.MapFS {
			return fstest.MapFS{
				"sample/helper.go": &fstest.MapFile{Data: []byte(`package sample
import (
	"os/exec"
	"time"
)
func helper() {
	nestedProcess()
	nestedSleep()
}
func nestedProcess() { _ = exec.Command("worker") }
func nestedSleep() { time.Sleep(0) }
`)},
				"sample/owned_test.go": &fstest.MapFile{Data: []byte(`package sample
import "testing"
func TestHermetic(t *testing.T) { helper() }
`)},
			}
		}
		const want = "reviewed hermetic body package_dir=sample package_name=sample owner=TestHermetic: fixed_sleep is reachable through TestHermetic -> helper -> nestedSleep (sample/helper.go:11)\n" +
			"reviewed hermetic body package_dir=sample package_name=sample owner=TestHermetic: subprocess is reachable through TestHermetic -> helper -> nestedProcess (sample/helper.go:10)"
		for iteration := 0; iteration < 2; iteration++ {
			census := scanHermeticFixture(t, fixture())
			err := validateReviewedHermeticBodies([]ReviewedHermeticBody{validReviewedHermeticBody("TestHermetic")}, census)
			if err == nil || err.Error() != want {
				t.Fatalf("iteration %d error = %v, want exact:\n%s", iteration, err, want)
			}
		}
	})

	t.Run("cross-file helper shadows predeclared identifier", func(t *testing.T) {
		census := scanHermeticFixture(t, fstest.MapFS{
			"sample/helper.go": &fstest.MapFile{Data: []byte(`package sample
import "time"
func clear() { time.Sleep(0) }
`)},
			"sample/owned_test.go": &fstest.MapFile{Data: []byte(`package sample
import "testing"
func TestHermetic(t *testing.T) { clear() }
`)},
		})
		err := validateReviewedHermeticBodies([]ReviewedHermeticBody{validReviewedHermeticBody("TestHermetic")}, census)
		requireErrorContains(t, err, "TestHermetic -> clear")
	})

	t.Run("function alias still reaches helper", func(t *testing.T) {
		census := scanHermeticFixture(t, fstest.MapFS{
			"sample/resource_test.go": &fstest.MapFile{Data: []byte(`package sample
import (
	"testing"
	"time"
)
func TestHermetic(t *testing.T) {
	alias := helper
	_ = func() { alias() }
}
func helper() { time.Sleep(0) }
`)},
		})
		err := validateReviewedHermeticBodies([]ReviewedHermeticBody{validReviewedHermeticBody("TestHermetic")}, census)
		requireErrorContains(t, err, "TestHermetic -> helper")
	})

	t.Run("duplicate runnable declaration fails closed", func(t *testing.T) {
		census := scanHermeticFixture(t, fstest.MapFS{
			"sample/first_test.go": &fstest.MapFile{Data: []byte(`package sample
import "testing"
func TestHermetic(t *testing.T) {}
`)},
			"sample/second_test.go": &fstest.MapFile{Data: []byte(`package sample
import "testing"
func TestHermetic(t *testing.T) {}
`)},
		})
		err := validateReviewedHermeticBodies([]ReviewedHermeticBody{validReviewedHermeticBody("TestHermetic")}, census)
		requireErrorContains(t, err, "runnable owner is not unique")
	})
}

func TestValidateReviewedHermeticBodiesRequiresRetainedRealOwner(t *testing.T) {
	t.Parallel()

	row := seedReviewedHermeticBody()
	missing := scanHermeticFixture(t, fstest.MapFS{
		"cmd/gc/owned_test.go": &fstest.MapFile{Data: []byte(`package main
import "testing"
func TestPrepareWaitWakeState_ResolvesRigDependencyBeads(t *testing.T) {}
`)},
	})
	requireErrorContains(t, validateReviewedHermeticBodies([]ReviewedHermeticBody{row}, missing), "retained real composition owner")

	complete := scanHermeticFixture(t, fstest.MapFS{
		"cmd/gc/owned_test.go": &fstest.MapFile{Data: []byte(`package main
import "testing"
func TestPrepareWaitWakeState_ResolvesRigDependencyBeads(t *testing.T) {}
func TestCmdSessionWait_AllowsRigDependencyBeads(t *testing.T) {}
`)},
	})
	if err := validateReviewedHermeticBodies([]ReviewedHermeticBody{row}, complete); err != nil {
		t.Fatalf("validateReviewedHermeticBodies(retained owner): %v", err)
	}
}

func errorsFromProblems(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "\n"))
}

func scanHermeticFixture(t *testing.T, files fstest.MapFS) Census {
	t.Helper()
	census, err := ScanFS(files)
	if err != nil {
		t.Fatalf("ScanFS: %v", err)
	}
	return census
}

func validReviewedHermeticBody(owner string) ReviewedHermeticBody {
	return ReviewedHermeticBody{
		PackageDir:    "sample",
		PackageName:   "sample",
		Owner:         owner,
		EffectiveSize: "medium",
		MediumReason:  "package TestMain mutates process state",
	}
}

func seedReviewedHermeticBody() ReviewedHermeticBody {
	return ReviewedHermeticBody{
		PackageDir:    "cmd/gc",
		PackageName:   "main",
		Owner:         "TestPrepareWaitWakeState_ResolvesRigDependencyBeads",
		EffectiveSize: "medium",
		MediumReason:  "package TestMain mutates process state",
	}
}

func withHermeticOwner(row ReviewedHermeticBody, owner string) ReviewedHermeticBody {
	row.Owner = owner
	return row
}

func withHermeticSize(row ReviewedHermeticBody, size string) ReviewedHermeticBody {
	row.EffectiveSize = size
	return row
}

func withHermeticReason(row ReviewedHermeticBody, reason string) ReviewedHermeticBody {
	row.MediumReason = reason
	return row
}
