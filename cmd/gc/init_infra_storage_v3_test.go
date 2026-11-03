package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestNativeInfrastructureRecoversBdInitAfterMetadataBeforeIdentity(t *testing.T) {
	city, _, plan := nativeInfrastructureFixture(t)
	first := nativeInfrastructureFakeOps(t, plan)
	run := first.Run
	first.Run = func(ctx context.Context, root string, env []string, args ...string) error {
		if err := run(ctx, root, env, args...); err != nil {
			return err
		}
		if args[0] == "bd" && args[1] == "init" {
			return errors.New("process died after metadata before DB identity")
		}
		return nil
	}
	if err := resumeInitInfrastructureWithOps(context.Background(), city, first); err == nil {
		t.Fatal("interrupted init reported success")
	}
	recovered, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
	if err != nil || recovered == nil || recovered.Ready {
		t.Fatalf("pending=%+v err=%v", recovered, err)
	}
	resumed := nativeInfrastructureFakeOps(t, *recovered)
	baseRun := resumed.Run
	confirmed := false
	resumed.EnsureIdentity = func(_ context.Context, root string, pending initInfrastructurePlan) error {
		if pending.ProjectID != "owned-project" || pending.Database != plan.Database || root != filepath.Join(city, ".gc", "storage", "infra") {
			t.Fatalf("recovery changed recorded identity or scope: %+v %s", pending, root)
		}
		persisted, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
		if err != nil || persisted.ProjectID != pending.ProjectID {
			t.Fatalf("project identity not durable before recovery: %+v %v", persisted, err)
		}
		confirmed = true
		return nil
	}
	resumed.Run = func(ctx context.Context, root string, env []string, args ...string) error {
		if args[0] == "bd" && args[1] == "init" {
			t.Fatal("metadata recovery reinitialized the database")
		}
		if !confirmed {
			t.Fatalf("provider command ran before recorded identity was confirmed: %s", strings.Join(args, " "))
		}
		return baseRun(ctx, root, env, args...)
	}
	resumed.Verify = func(context.Context, string, initInfrastructurePlan) error {
		if !confirmed {
			t.Fatal("native readiness ran before identity recovery")
		}
		return nil
	}
	if err := resumeInitInfrastructureWithOps(context.Background(), city, resumed); err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Fatal("restart skipped identity recovery because metadata existed")
	}
}

func TestNativeInfrastructureIdentityRefusalPreservesConfig(t *testing.T) {
	city, raw, plan := nativeInfrastructureFixture(t)
	ops := nativeInfrastructureFakeOps(t, plan)
	ops.EnsureIdentity = func(context.Context, string, initInfrastructurePlan) error {
		return errors.New("foreign database identity")
	}
	if err := resumeInitInfrastructureWithOps(context.Background(), city, ops); err == nil {
		t.Fatal("identity failure reported ready")
	}
	got, err := fsys.OSFS{}.ReadFile(filepath.Join(city, "city.toml"))
	if err != nil || string(got) != string(raw) {
		t.Fatalf("identity failure changed config: %s %v", got, err)
	}
	stored, err := readInitInfrastructurePlan(fsys.OSFS{}, city)
	if err != nil || stored.Ready {
		t.Fatalf("identity failure marked ready: %+v %v", stored, err)
	}
}
