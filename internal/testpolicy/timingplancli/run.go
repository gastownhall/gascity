// Package timingplancli implements the file-backed dry-run timing planner command.
package timingplancli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/testpolicy/timingplan"
	"github.com/gastownhall/gascity/internal/testpolicy/timingsummary"
)

const (
	inventoryDocumentSchema = 1
	configDocumentSchema    = 1
	outputDocumentSchema    = 1
)

type inventoryDocument struct {
	Schema int                        `json:"schema"`
	Units  []timingplan.InventoryUnit `json:"units"`
}

type configDocument struct {
	Schema        int                        `json:"schema"`
	Profile       timingplan.ProfileSelector `json:"profile"`
	Shards        int                        `json:"shards"`
	Defaults      timingplan.StaticTiming    `json:"defaults"`
	P95CapSeconds float64                    `json:"p95_cap_seconds"`
}

type outputDocument struct {
	Schema               int                        `json:"schema"`
	Authority            string                     `json:"authority"`
	HistorySchema        int                        `json:"history_schema"`
	Profile              timingplan.ProfileSelector `json:"profile"`
	HistoryProfileStatus string                     `json:"history_profile_status"`
	Plan                 timingplan.Result          `json:"plan"`
}

type singlePathFlag struct {
	name  string
	value string
	set   bool
}

func (value *singlePathFlag) String() string {
	return value.value
}

func (value *singlePathFlag) Set(path string) error {
	if value.set {
		return fmt.Errorf("%s may only be specified once", value.name)
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s path must not be empty", value.name)
	}
	value.value = path
	value.set = true
	return nil
}

// Run validates timing-plan inputs and writes the canonical dry-run plan.
func Run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("test-timing-plan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	inventoryPath := singlePathFlag{name: "inventory"}
	historyPath := singlePathFlag{name: "history"}
	configPath := singlePathFlag{name: "config"}
	flags.Var(&inventoryPath, "inventory", "schema-v1 runnable inventory JSON")
	flags.Var(&historyPath, "history", "canonical schema-v1 timing snapshot JSON")
	flags.Var(&configPath, "config", "schema-v1 timing-plan configuration JSON")
	if err := flags.Parse(args); err != nil {
		return reportError(stderr, 2, err)
	}
	if flags.NArg() != 0 {
		return reportError(stderr, 2, errors.New("positional arguments are not supported"))
	}
	for _, required := range []struct {
		name  string
		value singlePathFlag
	}{
		{name: "inventory", value: inventoryPath},
		{name: "history", value: historyPath},
		{name: "config", value: configPath},
	} {
		if !required.value.set {
			return reportError(stderr, 2, fmt.Errorf("--%s is required", required.name))
		}
	}

	var inventory inventoryDocument
	if err := decodeVersionedFile(inventoryPath.value, "inventory", inventoryDocumentSchema, &inventory); err != nil {
		return reportError(stderr, 1, err)
	}
	if inventory.Units == nil {
		return reportError(stderr, 1, errors.New("inventory units must be an array"))
	}
	var history timingsummary.Snapshot
	if err := decodeVersionedFile(historyPath.value, "history", timingsummary.SnapshotSchema, &history); err != nil {
		return reportError(stderr, 1, err)
	}
	if history.Profiles == nil {
		return reportError(stderr, 1, errors.New("history profiles must be an array"))
	}
	var config configDocument
	if err := decodeVersionedFile(configPath.value, "config", configDocumentSchema, &config); err != nil {
		return reportError(stderr, 1, err)
	}

	result, err := timingplan.PlanSnapshot(timingplan.SnapshotPlanInput{
		Inventory:     inventory.Units,
		History:       history,
		Profile:       config.Profile,
		Shards:        config.Shards,
		Defaults:      config.Defaults,
		P95CapSeconds: config.P95CapSeconds,
	})
	if err != nil {
		return reportError(stderr, 1, err)
	}
	encoded, err := json.Marshal(outputDocument{
		Schema:               outputDocumentSchema,
		Authority:            "dry-run",
		HistorySchema:        history.Schema,
		Profile:              config.Profile,
		HistoryProfileStatus: result.HistoryProfileStatus,
		Plan:                 result.Plan,
	})
	if err != nil {
		return reportError(stderr, 1, fmt.Errorf("encode output: %w", err))
	}
	encoded = append(encoded, '\n')
	if _, err := stdout.Write(encoded); err != nil {
		return reportError(stderr, 1, fmt.Errorf("write output: %w", err))
	}
	return 0
}

func decodeVersionedFile(path, kind string, wantSchema int, destination any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s %s: %w", kind, path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("decode %s %s: %w", kind, path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("decode %s %s: %w", kind, path, err)
	}
	var envelope struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode %s schema in %s: %w", kind, path, err)
	}
	if envelope.Schema == nil {
		return fmt.Errorf("%s schema is required", kind)
	}
	if *envelope.Schema != wantSchema {
		return fmt.Errorf("unsupported %s schema %d", kind, *envelope.Schema)
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(destination); err != nil {
		return fmt.Errorf("decode schema-v%d %s %s: %w", wantSchema, kind, path, err)
	}
	if err := requireJSONEOF(strict); err != nil {
		return fmt.Errorf("decode schema-v%d %s %s: %w", wantSchema, kind, path, err)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func reportError(stderr io.Writer, code int, err error) int {
	_, _ = fmt.Fprintf(stderr, "timing plan: %v\n", err)
	return code
}
