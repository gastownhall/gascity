package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// keyFileMode is the permission of a generated private key file: readable by
// its owner only.
const keyFileMode = 0o600

// keyDirMode is the permission keygen gives a key directory it creates.
const keyDirMode = 0o700

// keygenParams configure one keygen run.
type keygenParams struct {
	Kid   string    // key id paired with the public key in the printed line
	Out   string    // path the PEM PKCS#8 private key is written to
	Force bool      // replace an existing file at Out
	Rand  io.Reader // key-generation randomness
}

// runKeygen implements `gc-write-mint keygen --kid <id> --out <path>
// [--force]`. It prints the server's verify-key entry to stdout and returns
// the process exit code: 0 on success, 1 when keygen fails, 2 on a usage
// error.
func runKeygen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	kid := fs.String("kid", "", "key id paired with the public key; must not contain ':', ',' or whitespace")
	out := fs.String("out", "", "path to write the ed25519 private key to (PEM PKCS#8, mode 0600)")
	force := fs.Bool("force", false, "replace an existing file at --out")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gc-write-mint keygen: unexpected arguments %q\n", fs.Args()) //nolint:errcheck // best-effort stderr
		return 2
	}
	if strings.TrimSpace(*out) == "" {
		fmt.Fprintln(stderr, "gc-write-mint keygen: --out is required") //nolint:errcheck // best-effort stderr
		return 2
	}
	line, err := keygen(keygenParams{Kid: *kid, Out: *out, Force: *force, Rand: rand.Reader})
	if err != nil {
		fmt.Fprintln(stderr, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintln(stdout, line) //nolint:errcheck // best-effort stdout
	return 0
}

// keygen generates an ed25519 keypair, writes the private key to p.Out as PEM
// PKCS#8 with mode 0600, and returns the "<kid>:<base64 public key>" entry a
// server lists in its write-auth verify key. It refuses a kid the verify-key
// list cannot carry, and an existing p.Out unless p.Force is set.
func keygen(p keygenParams) (string, error) {
	if err := validateKid(p.Kid); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Out) == "" {
		return "", errors.New("gc-write-mint keygen: --out is required")
	}
	if !p.Force {
		if _, err := os.Lstat(p.Out); err == nil {
			return "", fmt.Errorf("gc-write-mint keygen: %s already exists; pass --force to replace it", p.Out)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("gc-write-mint keygen: checking %s: %w", p.Out, err)
		}
	}
	_, priv, err := ed25519.GenerateKey(p.Rand)
	if err != nil {
		return "", fmt.Errorf("gc-write-mint keygen: generating key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", fmt.Errorf("gc-write-mint keygen: encoding key: %w", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := writeKeyFile(p.Out, data, p.Force); err != nil {
		return "", err
	}
	return verifyKeyLine(p.Kid, priv), nil
}

// validateKid refuses a kid the "kid:base64,kid2:base64" verify-key list
// cannot carry unambiguously: empty, or containing ':', ',' or whitespace.
func validateKid(kid string) error {
	if kid == "" {
		return errors.New("gc-write-mint keygen: --kid is required")
	}
	if i := strings.IndexFunc(kid, func(r rune) bool {
		return r == ':' || r == ',' || unicode.IsSpace(r)
	}); i >= 0 {
		return fmt.Errorf("gc-write-mint keygen: --kid %q must not contain ':', ',' or whitespace", kid)
	}
	return nil
}

// verifyKeyLine returns the "<kid>:<base64 raw 32-byte public key>" entry for
// priv's public key.
func verifyKeyLine(kid string, priv ed25519.PrivateKey) string {
	pub := priv.Public().(ed25519.PublicKey)
	return kid + ":" + base64.StdEncoding.EncodeToString(pub)
}

// writeKeyFile writes data to path with mode 0600 through a temp file in the
// same directory, creating that directory (0700) when missing. Without force
// it links the temp file into place, which fails rather than replace a file
// created since keygen checked; with force it renames over any existing file.
func writeKeyFile(path string, data []byte, force bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, keyDirMode); err != nil {
		return fmt.Errorf("gc-write-mint keygen: creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("gc-write-mint keygen: creating temp key file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup; gone after a rename
	if err := tmp.Chmod(keyFileMode); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("gc-write-mint keygen: setting key file mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("gc-write-mint keygen: writing key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("gc-write-mint keygen: syncing key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("gc-write-mint keygen: closing key: %w", err)
	}
	if force {
		if err := os.Rename(tmpPath, path); err != nil {
			return fmt.Errorf("gc-write-mint keygen: replacing %s: %w", path, err)
		}
		return nil
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("gc-write-mint keygen: %s already exists; pass --force to replace it", path)
		}
		return fmt.Errorf("gc-write-mint keygen: writing %s: %w", path, err)
	}
	return nil
}
