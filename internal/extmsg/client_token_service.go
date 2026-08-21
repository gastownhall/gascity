package extmsg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	beadTypeClientToken = "extmsg_client_token"

	// labelClientTokenValuePrefix is the lookup label keyed by SHA-256 of the
	// raw token value so subscribe can resolve by the Authorization header.
	labelClientTokenValuePrefix = "extmsg:client:token:lookup:v1:"
)

// RegisterClientInput is the input for registering or re-issuing a client token.
type RegisterClientInput struct {
	Credential        string
	AllowedSessions   []string
	AllowNoCredential bool
}

// RegisterClientResult is the result of a client registration.
type RegisterClientResult struct {
	ClientID string
	Token    string
	Created  bool
}

// RegisterClient issues or re-issues a client token bead for the given
// credential. Issuance is idempotent: if a bead with the same credential hash
// exists, it is returned with Created=false and an empty Token (the raw value
// is only returned on initial issuance).
//
// If AllowNoCredential is false and Credential is empty, an error is returned.
//
// Anonymous mode (AllowNoCredential=true with an empty Credential) maps every
// anonymous registration to the same credential hash, so a city supports
// exactly one anonymous client: the first call creates it and returns the
// token; subsequent anonymous calls are idempotent re-issuances that return
// the existing client ID with Created=false and an empty Token. Callers that
// need distinct client identities must supply distinct credentials.
func RegisterClient(ctx context.Context, store beads.Store, input RegisterClientInput) (RegisterClientResult, error) {
	if err := checkContext(ctx); err != nil {
		return RegisterClientResult{}, err
	}
	credential := strings.TrimSpace(input.Credential)
	if !input.AllowNoCredential && credential == "" {
		return RegisterClientResult{}, fmt.Errorf("%w: credential required (allow_no_credential=false)", ErrInvalidInput)
	}

	credHash := hashCredential(credential)
	label := clientTokenCredentialLabel(credHash)

	items, err := store.List(beads.ListQuery{Label: label})
	if err != nil {
		return RegisterClientResult{}, fmt.Errorf("querying client token: %w", err)
	}
	if len(items) > 0 {
		// Idempotent re-issuance: return existing bead without revealing the token again.
		return RegisterClientResult{
			ClientID: items[0].ID,
			Token:    "",
			Created:  false,
		}, nil
	}

	// Generate a new 32-byte base64url token.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return RegisterClientResult{}, fmt.Errorf("generating token: %w", err)
	}
	tokenValue := base64.RawURLEncoding.EncodeToString(raw)
	tokenHash := hashTokenValue(tokenValue)

	sessionsJSON, err := json.Marshal(input.AllowedSessions)
	if err != nil {
		return RegisterClientResult{}, fmt.Errorf("encoding allowed_sessions: %w", err)
	}
	now := timeNow()

	b, err := store.Create(beads.Bead{
		Title: "extmsg client token",
		Type:  beadTypeClientToken,
		Labels: []string{
			LabelBaseClientToken,
			label,
			labelClientTokenValuePrefix + tokenHash,
		},
		// The raw token is never persisted: ResolveClientToken looks the bead
		// up by the sha256(token) label below, so storing the plaintext value
		// would put a bearer credential at rest for no functional reason.
		Metadata: map[string]string{
			"credential_hash":  credHash,
			"allowed_sessions": string(sessionsJSON),
			"created_at":       formatTime(now),
		},
	})
	if err != nil {
		return RegisterClientResult{}, fmt.Errorf("creating client token bead: %w", err)
	}

	return RegisterClientResult{
		ClientID: b.ID,
		Token:    tokenValue,
		Created:  true,
	}, nil
}

// ResolveClientToken looks up a client token bead by raw token value.
// Returns (clientID, allowedSessions, nil) on success, or an error when the
// token is unknown.
func ResolveClientToken(ctx context.Context, store beads.Store, tokenValue string) (string, []string, error) {
	if err := checkContext(ctx); err != nil {
		return "", nil, err
	}
	tokenValue = strings.TrimSpace(tokenValue)
	if tokenValue == "" {
		return "", nil, fmt.Errorf("%w: token required", ErrInvalidInput)
	}
	tokenHash := hashTokenValue(tokenValue)
	label := labelClientTokenValuePrefix + tokenHash

	items, err := store.List(beads.ListQuery{Label: label})
	if err != nil {
		return "", nil, fmt.Errorf("resolving client token: %w", err)
	}
	if len(items) == 0 {
		return "", nil, ErrClientTokenNotFound
	}

	b := items[0]
	var allowedSessions []string
	if raw := b.Metadata["allowed_sessions"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &allowedSessions); err != nil {
			return "", nil, fmt.Errorf("decoding allowed_sessions: %w", err)
		}
	}
	return b.ID, allowedSessions, nil
}

// hashTokenValue returns the SHA-256 hex hash of a raw token value.
func hashTokenValue(tokenValue string) string {
	sum := sha256.Sum256([]byte(tokenValue))
	return hex.EncodeToString(sum[:])
}
