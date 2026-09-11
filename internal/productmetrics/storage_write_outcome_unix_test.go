//go:build (linux && !android) || (darwin && !ios)

package productmetrics

import (
	"errors"
	"testing"
)

func TestStorageAtomicWriteReportsNotAppliedAppliedSyncPendingAndDurable(t *testing.T) {
	tests := map[string]struct {
		hooks storageTestHooks
		want  storageWriteState
		err   bool
	}{
		"durable": {want: storageWriteAppliedDurable},
		"not applied": {
			hooks: storageTestHooks{beforeStep: func(step storageStep) error {
				if step == storageStepRename {
					return errors.New("injected rename failure")
				}
				return nil
			}},
			want: storageWriteNotApplied,
			err:  true,
		},
		"applied sync pending": {
			hooks: func() storageTestHooks {
				var renamed bool
				return storageTestHooks{beforeStep: func(step storageStep) error {
					if step == storageStepRename {
						renamed = true
					}
					if renamed && step == storageStepDirectorySync {
						return errors.New("injected persistent directory sync failure")
					}
					return nil
				}}
			}(),
			want: storageWriteAppliedSyncPending,
			err:  true,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			home := newMetricsTestHome(t)
			writeRawConfigFixture(t, home, []byte("old"))
			root, err := openStorageRootMutableWithHooks(home, test.hooks)
			if err != nil {
				t.Fatalf("open mutable root: %v", err)
			}
			defer func() {
				if err := root.Close(); err != nil {
					t.Fatalf("close root: %v", err)
				}
			}()

			result, err := root.writeFileAtomicOutcome(configFileName, []byte("new"))
			if (err != nil) != test.err {
				t.Fatalf("write error = %v, want error=%v", err, test.err)
			}
			if result.state != test.want {
				t.Fatalf("write state = %v, want %v", result.state, test.want)
			}
			got, readErr := root.readFile(configFileName, maximumConfigBytes)
			if readErr != nil {
				t.Fatalf("read result: %v", readErr)
			}
			wantBytes := "new"
			if test.want == storageWriteNotApplied {
				wantBytes = "old"
			}
			if string(got) != wantBytes {
				t.Fatalf("visible bytes = %q, want %q", got, wantBytes)
			}
		})
	}
}
