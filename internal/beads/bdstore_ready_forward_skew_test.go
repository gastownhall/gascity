package beads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const bdForwardSchemaSkewSQLFailure = "exit status 1: Warning: schema skew ignored — database (v59) is ahead of binary (v53); some queries may fail\n[mysql] read tcp 127.0.0.1:55892->127.0.0.1:30029: i/o timeout"

// Use the real BdStore projection and Ready doors with in-memory snapshot rows.
// This owns only the wiring from a forward-schema verdict to canonical Ready.
type forwardSkewCacheBacking struct {
	Store
	source *BdStore
}

func (s *forwardSkewCacheBacking) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	return s.source.enrichReadyProjectionForCache(items)
}

func (s *forwardSkewCacheBacking) Ready(query ...ReadyQuery) ([]Bead, error) {
	return s.source.Ready(query...)
}

func TestReadyProjectionForwardSchemaSkewUsesCanonicalReady(t *testing.T) {
	for _, reply := range []string{"ready", "empty", "empty envelope", "malformed", "null", "null envelope", "object", "null row", "empty object", "id-less", "blank id", "status-less", "blank status", "mixed invalid", "error"} {
		t.Run(reply, func(t *testing.T) {
			scope := t.TempDir()
			writeScopeMetadata(t, scope, map[string]any{"database": "dolt", "backend": "dolt", "dolt_mode": "server"})
			mem := NewMemStore()
			ids := map[string]string{}
			for _, name := range []string{"unattributable", "parent", "child", "ephemeral", "safe"} {
				bead, err := mem.Create(Bead{Type: "task", Status: "open", Title: name, Ephemeral: name == "ephemeral"})
				if err != nil {
					t.Fatal(err)
				}
				ids[name] = bead.ID
			}
			if err := mem.DepAdd(ids["child"], ids["parent"], "parent-child"); err != nil {
				t.Fatal(err)
			}
			notices := &bytes.Buffer{}
			sqlCalls, blockedCalls, readyCalls := 0, 0, 0
			runner := &recordingRunner{}
			runner.reply = func(args []string) ([]byte, error) {
				switch args[0] {
				case "version":
					return []byte("bd version 1.2.2\n"), nil
				case "sql":
					sqlCalls++
					return nil, errors.New(bdForwardSchemaSkewSQLFailure)
				case "blocked":
					blockedCalls++
					return []byte(`[]`), nil // Cannot attribute the hidden blocked work.
				case "ready":
					readyCalls++
					switch reply {
					case "empty":
						return []byte(`[]`), nil
					case "empty envelope":
						return []byte(`{"issues":[]}`), nil
					case "null envelope":
						return []byte(`{"issues":null}`), nil
					case "object":
						return []byte(`{"id":"wrong-shape"}`), nil
					case "blank id":
						return []byte(`[{"id":"   ","status":"open"}]`), nil
					case "blank status":
						return []byte(`[{"id":"blank-status","status":"   "}]`), nil
					case "malformed":
						return []byte(`[{`), nil
					case "null":
						return []byte(`null`), nil
					case "null row":
						return []byte(`[null]`), nil
					case "empty object":
						return []byte(`[{}]`), nil
					case "id-less":
						return []byte(`[{"issue_type":"task","status":"open"}]`), nil
					case "status-less":
						return []byte(`[{"id":"statusless","issue_type":"task"}]`), nil
					case "mixed invalid":
						return []byte(fmt.Sprintf(`[{"id":%q,"issue_type":"task","status":"open"},{"status":"open"}]`, ids["safe"])), nil
					case "error":
						return nil, errors.New("canonical ready unavailable")
					}
					return []byte(fmt.Sprintf(`[{"id":%q,"title":"safe","issue_type":"task","status":"open"}]`, ids["safe"])), nil
				}
				return nil, fmt.Errorf("unexpected command: %v", args)
			}
			for rebuild := 0; rebuild < 2; rebuild++ {
				source := NewBdStore(scope, runner.run, WithBdStoreNoticeSink(notices))
				cache := NewCachingStoreForTest(&forwardSkewCacheBacking{Store: mem, source: source}, nil)
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatal(err)
				}
				if !cache.readyReadsMustGoLive() {
					t.Fatal("forward schema must disable cached readiness")
				}
				if _, ok := cache.CachedReady(); ok {
					t.Fatal("cache offered a readiness verdict without complete is_blocked")
				}
				for _, query := range []ReadyQuery{{}, {TierMode: TierBoth}, {Limit: 1}} {
					rows, err := cache.Ready(query)
					switch reply {
					case "ready":
						if err != nil || len(rows) != 1 || rows[0].ID != ids["safe"] {
							t.Fatalf("canonical ready lost: rows=%+v err=%v", rows, err)
						}
					case "empty", "empty envelope":
						if err != nil || len(rows) != 0 {
							t.Fatalf("valid empty ready: rows=%+v err=%v", rows, err)
						}
					default:
						if err == nil {
							t.Fatalf("%s ready became success: %+v", reply, rows)
						}
					}
					if reply == "mixed invalid" {
						var partial *PartialResultError
						if !errors.As(err, &partial) || len(rows) != 1 || rows[0].ID != ids["safe"] {
							t.Fatalf("cache lost partial error or retained corrupt row: rows=%+v err=%v", rows, err)
						}
					}
				}
			}
			if sqlCalls != 1 || blockedCalls != 0 || readyCalls != 6 {
				t.Fatalf("calls sql=%d blocked=%d ready=%d", sqlCalls, blockedCalls, readyCalls)
			}
			if strings.Count(notices.String(), "ready-projection enrichment disabled") != 1 || !strings.Contains(notices.String(), "ahead of binary") {
				t.Fatalf("missing one causal notice: %s", notices)
			}
		})
	}
}

func TestReadyProjectionTransientSQLFailureDoesNotLatch(t *testing.T) {
	scope := t.TempDir()
	runner := &recordingRunner{}
	sqlCalls := 0
	runner.reply = func(args []string) ([]byte, error) {
		if args[0] == "version" {
			return []byte("bd version 1.2.2\n"), nil
		}
		if args[0] == "sql" {
			sqlCalls++
			return nil, errors.New("temporary transport failure")
		}
		return nil, fmt.Errorf("unexpected command: %v", args)
	}
	for rebuild := 0; rebuild < 2; rebuild++ {
		source := NewBdStore(scope, runner.run, WithBdStoreNoticeSink(&bytes.Buffer{}))
		_, err := source.enrichReadyProjectionForCache(activeWorkBeads())
		if err == nil || errors.Is(err, ErrReadyProjectionUnsupported) || source.latchedReadyProjectionDegrade() != nil {
			t.Fatalf("transient failure latched or disappeared: %v", err)
		}
	}
	if sqlCalls != 2 {
		t.Fatalf("SQL calls=%d, want retry on rebuild", sqlCalls)
	}
}

func TestReadyProjectionForwardSchemaSkewClassifierIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"exact production shape", errors.New(bdForwardSchemaSkewSQLFailure), true},
		{"ordinary timeout", errors.New("read tcp: i/o timeout"), false},
		{"warning without binary verdict", errors.New("schema skew ignored"), false},
		{"ahead without explicit override", errors.New("database is ahead of binary"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBdSQLForwardSchemaSkewFailure(tc.err); got != tc.want {
				t.Fatalf("classifier = %v, want %v for %v", got, tc.want, tc.err)
			}
		})
	}
}
