//go:build integration

package main

import (
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/google/uuid"
)

// TestCapstoneIntegrationRealMinter is the operator-config leg of the capstone:
// it re-runs scenario A, but the X-GC-City-Write grant is minted by the REAL
// gc-write-mint binary through the REAL operator path — a ~/.gc/contexts.toml
// (isolated GC_HOME) with a grant_command, resolved by resolveWriteTarget() with
// GC_CITY_CONTEXT=prod, built into a client by buildRemoteWriteClient, minting via
// the clientgrant env-exec chain. The stock Context carries the v2 audience and
// CID, the real minter is pinned to the same pair, and the live verifier is
// CID-configured. Only the git-fetch (rigCloneGit) and the SSRF
// resolver stay stubbed (no real network); the grant, the resolver, the TLS
// handshake, and the writeAuthMiddleware are all real.
//
// It is integration-tagged because it runs `go build` of gc-write-mint and execs
// it (via sh -c) once per mutation — the same reason cmd/gc-write-mint/e2e_test.go
// is integration-tagged.
func TestCapstoneIntegrationRealMinter(t *testing.T) {
	const cid = "city_capstone"
	h := newCapstoneHarnessWithCID(t, cid)

	// Build the real minter. The test cwd is cmd/gc, so build by import path.
	bin := filepath.Join(t.TempDir(), "gc-write-mint")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "github.com/gastownhall/gascity/cmd/gc-write-mint").CombinedOutput(); err != nil {
		t.Fatalf("build gc-write-mint: %v\n%s", err, out)
	}

	// The minter signs with the private key whose public half the server trusts
	// (the harness installed h's pubkey via InstallWriteAuth). gc-write-mint reads
	// a hex-encoded 32-byte seed.
	keyFile := filepath.Join(t.TempDir(), "city.ed25519")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(h.priv.Seed())), 0o600); err != nil {
		t.Fatal(err)
	}

	// Reset the remote-selection flag globals so only the env drives resolution.
	origCtx, origURL, origName := contextFlag, cityURLFlag, cityNameFlag
	t.Cleanup(func() { contextFlag, cityURLFlag, cityNameFlag = origCtx, origURL, origName })
	contextFlag, cityURLFlag, cityNameFlag = "", "", ""

	// Write the operator's contexts.toml through the real `gc context add` path
	// (city-pinned grant_command). GC_HOME is already the harness temp dir.
	grantCmd := bin + " --kid k1 --key " + keyFile + " --city " + h.cityName +
		" --aud " + citywriteauth.AudienceCityWriteV2 + " --cid " + cid
	if code := doContextAdd(clientcontext.Context{
		Name:          "prod",
		URL:           h.srv.URL,
		City:          h.cityName,
		GrantCommand:  grantCmd,
		GrantAudience: citywriteauth.AudienceCityWriteV2,
		GrantCID:      cid,
		CAFile:        h.caPath,
	}, io.Discard, os.Stderr); code != 0 {
		t.Fatal("gc context add prod failed")
	}

	// Select the context exactly as `gc --context prod` / GC_CITY_CONTEXT=prod does.
	t.Setenv("GC_CITY_CONTEXT", "prod")

	client, isRemote, target, err := resolveWriteTarget()
	if err != nil {
		t.Fatalf("resolveWriteTarget: %v", err)
	}
	if !isRemote || client == nil {
		t.Fatalf("expected a remote write client (isRemote=%v client=%v)", isRemote, client)
	}
	if target == nil || target.Ctx == nil || target.Ctx.Name != "prod" || target.Ctx.GrantCommand == "" {
		t.Fatalf("resolved target did not carry the prod context's grant_command: %+v", target)
	}

	// --- Scenario A via the real minter ---
	reqID := uuid.NewString()
	var addOut, addErr bytes.Buffer
	if code := cmdRigAddRemote(client, target, nil, capstonePublicGitURL(), reqID, "web", "gc", "main", nil, false, false, false, &addOut, &addErr); code != 0 {
		t.Fatalf("rig add exit=%d\nstdout=%s\nstderr=%s", code, addOut.String(), addErr.String())
	}
	if !strings.Contains(addOut.String(), "provisioned → web") {
		t.Errorf("rig add did not provision:\n%s", addOut.String())
	}
	if !strings.Contains(addErr.String(), "context: prod") {
		t.Errorf("target echo should name the prod context:\n%s", addErr.String())
	}

	webStore := h.cs.BeadStore("web")
	if webStore == nil {
		t.Fatalf("provisioned store missing")
	}
	seeded, err := webStore.Create(beads.Bead{Title: "integration work", Type: "task"})
	if err != nil {
		t.Fatalf("seed bead: %v", err)
	}
	if code := capstoneSling(h, client, "worker", seeded.ID, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("sling via real minter failed (exit=%d)", code)
	}
	routed, err := webStore.Get(seeded.ID)
	if err != nil {
		t.Fatalf("re-read routed bead: %v", err)
	}
	if routed.Metadata[capstoneRoutedToKey] == "" {
		t.Fatalf("gc.routed_to not set through the real-minter path")
	}

	// The in-process signer must NOT have been used: every grant came from the
	// built gc-write-mint binary via the contexts.toml grant_command.
	if got := h.grantCount.Load(); got != 0 {
		t.Errorf("the in-process signer minted %d grants; the real gc-write-mint binary should have minted every grant", got)
	}

	// A stock context/minter pair pinned to a different CID can mint a
	// cryptographically valid v2 token, but this city's CID-configured verifier
	// must reject it before the handler runs.
	wrongCID := "city_other"
	wrongCIDCmd := bin + " --kid k1 --key " + keyFile + " --city " + h.cityName +
		" --aud " + citywriteauth.AudienceCityWriteV2 + " --cid " + wrongCID
	wrongCIDClient, err := buildRemoteWriteClient(&remoteTarget{
		BaseURL:  h.srv.URL,
		CityName: h.cityName,
		Ctx: &clientcontext.Context{
			Name:          "wrong-cid",
			URL:           h.srv.URL,
			City:          h.cityName,
			GrantCommand:  wrongCIDCmd,
			GrantAudience: citywriteauth.AudienceCityWriteV2,
			GrantCID:      wrongCID,
			CAFile:        h.caPath,
		},
	})
	if err != nil {
		t.Fatalf("build wrong-CID stock client: %v", err)
	}
	_, err = wrongCIDClient.RigCreate(api.RigCreateRequest{
		Name: "wrong-cid", Prefix: "wc", DefaultBranch: "main",
		GitURL: capstonePublicGitURL(), RequestID: uuid.NewString(),
	}, nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "grant") {
		t.Fatalf("wrong-CID grant error = %v, want verifier refusal", err)
	}

	// If the context says v2 but its external signer is pinned to the legacy
	// audience, gc-write-mint itself refuses the mismatched GrantInfo and no HTTP
	// mutation is attempted.
	wrongAudCmd := bin + " --kid k1 --key " + keyFile + " --city " + h.cityName +
		" --aud " + citywriteauth.AudienceCityWrite
	wrongAudClient, err := buildRemoteWriteClient(&remoteTarget{
		BaseURL:  h.srv.URL,
		CityName: h.cityName,
		Ctx: &clientcontext.Context{
			Name:          "wrong-audience",
			URL:           h.srv.URL,
			City:          h.cityName,
			GrantCommand:  wrongAudCmd,
			GrantAudience: citywriteauth.AudienceCityWriteV2,
			GrantCID:      cid,
			CAFile:        h.caPath,
		},
	})
	if err != nil {
		t.Fatalf("build wrong-audience stock client: %v", err)
	}
	_, err = wrongAudClient.RigCreate(api.RigCreateRequest{
		Name: "wrong-audience", Prefix: "wa", DefaultBranch: "main",
		GitURL: capstonePublicGitURL(), RequestID: uuid.NewString(),
	}, nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "audience") {
		t.Fatalf("wrong-audience minter error = %v, want local audience refusal", err)
	}
}
