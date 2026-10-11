package storebinding

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationGuardRetriesOuterReleaseWithoutReopeningClaims(t *testing.T) {
	identity, err := newMigrationGuardIdentity(testMigrationGuardDirectory(t), 1, 1, Generation(1))
	if err != nil {
		t.Fatalf("newMigrationGuardIdentity(): %v", err)
	}
	firstCloseErr := errors.New("first outer guard close")
	closeCalls := 0
	guard, err := newMigrationGuard(identity, func() error {
		closeCalls++
		if closeCalls == 1 {
			return firstCloseErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("newMigrationGuard(): %v", err)
	}
	if err := guard.Release(); !errors.Is(err, firstCloseErr) {
		t.Fatalf("first guard.Release() error = %v, want close failure", err)
	}
	if _, err := guard.claim(context.Background()); !errors.Is(err, ErrMigrationGuardCleanupPending) {
		t.Fatalf("claim during outer cleanup error = %v, want ErrMigrationGuardCleanupPending", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatalf("second guard.Release(): %v", err)
	}
	if closeCalls != 2 {
		t.Fatalf("outer close calls = %d, want 2", closeCalls)
	}
	if _, err := guard.claim(context.Background()); !errors.Is(err, ErrMigrationGuardReleased) {
		t.Fatalf("claim after successful release error = %v, want ErrMigrationGuardReleased", err)
	}
}

func TestMigrationGuardConstructionIsAcquisitionOnly(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "migration_guard.go", nil, 0)
	if err != nil {
		t.Fatalf("parse migration guard API: %v", err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if function.Recv != nil {
			if function.Name.IsExported() && receiverIsMigrationGuard(function) && function.Name.Name == "Claim" {
				t.Fatal("exported MigrationGuard.Claim bypasses AcquireWriterFence")
			}
			continue
		}
		if !function.Name.IsExported() || !returnsMigrationGuard(function) {
			continue
		}
		if function.Name.Name != "AcquireMigrationGuard" {
			t.Fatalf("exported MigrationGuard constructor %s bypasses stable directory acquisition", function.Name.Name)
		}
	}
}

func receiverIsMigrationGuard(function *ast.FuncDecl) bool {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return false
	}
	switch receiver := function.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return receiver.Name == "MigrationGuard"
	case *ast.StarExpr:
		identity, ok := receiver.X.(*ast.Ident)
		return ok && identity.Name == "MigrationGuard"
	default:
		return false
	}
}

func returnsMigrationGuard(function *ast.FuncDecl) bool {
	if function.Type.Results == nil {
		return false
	}
	for _, result := range function.Type.Results.List {
		identity, ok := result.Type.(*ast.Ident)
		if ok && identity.Name == "MigrationGuard" {
			return true
		}
	}
	return false
}

func testMigrationGuardDirectory(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "city", ".gc")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("create city .gc directory: %v", err)
	}
	return directory
}

func TestAcquireMigrationGuardRejectsNoncanonicalOrSymlinkedCityDirectory(t *testing.T) {
	directory := testMigrationGuardDirectory(t)
	noncanonical := filepath.Dir(directory) + "/../" + filepath.Base(filepath.Dir(directory)) + "/.gc"
	if _, err := AcquireMigrationGuard(context.Background(), noncanonical, Generation(1)); err == nil {
		t.Fatal("AcquireMigrationGuard() succeeded for a noncanonical directory")
	}

	aliasCity := filepath.Join(t.TempDir(), "alias-city")
	if err := os.Symlink(filepath.Dir(directory), aliasCity); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := AcquireMigrationGuard(context.Background(), filepath.Join(aliasCity, ".gc"), Generation(1)); err == nil {
		t.Fatal("AcquireMigrationGuard() succeeded for a symlinked city directory")
	}
}
