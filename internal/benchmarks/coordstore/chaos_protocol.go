package coordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ChaosRequest is one StoreAdapter method call sent to a chaos child process.
type ChaosRequest struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

// ChaosResponse is one StoreAdapter method result returned by a chaos child
// process.
type ChaosResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Err    string          `json:"err,omitempty"`
}

// AckedWrite records a write acknowledged by the chaos child.
type AckedWrite struct {
	AckedAt time.Time `json:"acked_at"`
	Method  string    `json:"method"`
	ID      string    `json:"id"`
}

// ChaosClientConfig configures a Unix-socket chaos client.
type ChaosClientConfig struct {
	SocketPath      string
	AckedWritesPath string
}

// ChaosClient forwards StoreAdapter calls to a chaos server process.
type ChaosClient struct {
	socketPath      string
	ackedWritesPath string

	mu          sync.Mutex
	lastAckTime time.Time
	ackedIDs    []string
}

// NewChaosClient returns a StoreAdapter client backed by a Unix socket.
func NewChaosClient(cfg ChaosClientConfig) *ChaosClient {
	return &ChaosClient{socketPath: cfg.SocketPath, ackedWritesPath: cfg.AckedWritesPath}
}

// LastAckTime returns the most recent acknowledged Create or Update time.
func (c *ChaosClient) LastAckTime() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastAckTime
}

// AckedIDs returns IDs for writes acknowledged by Create or Update.
func (c *ChaosClient) AckedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ackedIDs...)
}

// ResetAckLedger clears acknowledged write state and switches ledger output.
func (c *ChaosClient) ResetAckLedger(path string) error {
	c.mu.Lock()
	c.ackedWritesPath = path
	c.lastAckTime = time.Time{}
	c.ackedIDs = nil
	c.mu.Unlock()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating acked-writes dir: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing stale acked-writes ledger: %w", err)
	}
	return nil
}

// Open is a no-op; the child process owns adapter initialization.
func (c *ChaosClient) Open(context.Context, Config) error { return nil }

// Close releases client-side resources.
func (c *ChaosClient) Close() error { return nil }

// Reset forwards Reset.
func (c *ChaosClient) Reset(ctx context.Context) error {
	return c.callNoResult(ctx, "Reset", nil)
}

// Create forwards Create and persists the ack ledger before returning success.
func (c *ChaosClient) Create(ctx context.Context, r Record) (Record, error) {
	var created Record
	if err := c.call(ctx, "Create", chaosCreateArgs{Record: r}, &created); err != nil {
		return Record{}, err
	}
	if err := c.recordAck("Create", created.ID); err != nil {
		return Record{}, err
	}
	return created, nil
}

// Get forwards Get.
func (c *ChaosClient) Get(ctx context.Context, id string) (Record, error) {
	var out Record
	if err := c.call(ctx, "Get", chaosIDArgs{ID: id}, &out); err != nil {
		return Record{}, err
	}
	return out, nil
}

// Update forwards Update and persists the ack ledger before returning success.
func (c *ChaosClient) Update(ctx context.Context, id string, u Update) error {
	if err := c.callNoResult(ctx, "Update", chaosUpdateArgs{ID: id, Update: u}); err != nil {
		return err
	}
	return c.recordAck("Update", id)
}

// Delete forwards Delete.
func (c *ChaosClient) Delete(ctx context.Context, id string) error {
	return c.callNoResult(ctx, "Delete", chaosIDArgs{ID: id})
}

// FilterScan forwards FilterScan.
func (c *ChaosClient) FilterScan(ctx context.Context, q Query) ([]Record, error) {
	var out []Record
	if err := c.call(ctx, "FilterScan", chaosQueryArgs{Query: q}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// BatchGet forwards BatchGet.
func (c *ChaosClient) BatchGet(ctx context.Context, ids []string) ([]Record, error) {
	var out []Record
	if err := c.call(ctx, "BatchGet", chaosIDsArgs{IDs: ids}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetMetadataBatch forwards SetMetadataBatch.
func (c *ChaosClient) SetMetadataBatch(ctx context.Context, id string, kvs map[string]string) error {
	return c.callNoResult(ctx, "SetMetadataBatch", chaosMetadataArgs{ID: id, Metadata: kvs})
}

// Ready forwards Ready.
func (c *ChaosClient) Ready(ctx context.Context, q ReadyQuery) ([]Record, error) {
	var out []Record
	if err := c.call(ctx, "Ready", chaosReadyArgs{Ready: q}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DepAdd forwards DepAdd.
func (c *ChaosClient) DepAdd(ctx context.Context, fromID, toID, depType string) error {
	return c.callNoResult(ctx, "DepAdd", chaosDepArgs{FromID: fromID, ToID: toID, DepType: depType})
}

// DepRemove forwards DepRemove.
func (c *ChaosClient) DepRemove(ctx context.Context, fromID, toID string) error {
	return c.callNoResult(ctx, "DepRemove", chaosDepArgs{FromID: fromID, ToID: toID})
}

// DepList forwards DepList.
func (c *ChaosClient) DepList(ctx context.Context, id, direction string) ([]Dep, error) {
	var out []Dep
	if err := c.call(ctx, "DepList", chaosDepListArgs{ID: id, Direction: direction}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PurgeExpired forwards PurgeExpired.
func (c *ChaosClient) PurgeExpired(ctx context.Context) (int, error) {
	var out int
	if err := c.call(ctx, "PurgeExpired", nil, &out); err != nil {
		return 0, err
	}
	return out, nil
}

// PurgeTerminal forwards PurgeTerminal.
func (c *ChaosClient) PurgeTerminal(ctx context.Context, olderThan time.Duration) (int, error) {
	var out int
	if err := c.call(ctx, "PurgeTerminal", chaosPurgeTerminalArgs{OlderThan: olderThan}, &out); err != nil {
		return 0, err
	}
	return out, nil
}

// PrimeScan forwards PrimeScan.
func (c *ChaosClient) PrimeScan(ctx context.Context) (int, error) {
	var out int
	if err := c.call(ctx, "PrimeScan", nil, &out); err != nil {
		return 0, err
	}
	return out, nil
}

// RecentScan forwards RecentScan.
func (c *ChaosClient) RecentScan(ctx context.Context, limit int) ([]Record, error) {
	var out []Record
	if err := c.call(ctx, "RecentScan", chaosLimitArgs{Limit: limit}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Stats forwards Stats.
func (c *ChaosClient) Stats(ctx context.Context) map[string]int64 {
	var out map[string]int64
	if err := c.call(ctx, "Stats", nil, &out); err != nil {
		return map[string]int64{"chaos_stats_error": 1}
	}
	return out
}

func (c *ChaosClient) callNoResult(ctx context.Context, method string, args any) error {
	return c.call(ctx, method, args, nil)
}

func (c *ChaosClient) call(ctx context.Context, method string, args any, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var raw json.RawMessage
	if args != nil {
		data, err := json.Marshal(args)
		if err != nil {
			return fmt.Errorf("chaos marshal %s args: %w", method, err)
		}
		raw = data
	}
	conn, err := net.Dial("unix", c.socketPath)
	if err != nil {
		return fmt.Errorf("chaos dial %s: %w", c.socketPath, err)
	}
	defer conn.Close() //nolint:errcheck
	if err := json.NewEncoder(conn).Encode(ChaosRequest{Method: method, Args: raw}); err != nil {
		return fmt.Errorf("chaos encode %s: %w", method, err)
	}
	var resp ChaosResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("chaos decode %s: %w", method, err)
	}
	if resp.Err != "" {
		if resp.Err == ErrNotFound.Error() {
			return ErrNotFound
		}
		return errors.New(resp.Err)
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("chaos unmarshal %s result: %w", method, err)
		}
	}
	return nil
}

func (c *ChaosClient) recordAck(method, id string) error {
	if id == "" {
		return fmt.Errorf("chaos ack %s missing id", method)
	}
	ackedAt := time.Now().UTC()
	entry := AckedWrite{AckedAt: ackedAt, Method: method, ID: id}
	if c.ackedWritesPath != "" {
		if err := os.MkdirAll(filepath.Dir(c.ackedWritesPath), 0o755); err != nil {
			return fmt.Errorf("creating acked-writes dir: %w", err)
		}
		file, err := os.OpenFile(c.ackedWritesPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("opening acked-writes ledger: %w", err)
		}
		data, err := json.Marshal(entry)
		if err != nil {
			file.Close() //nolint:errcheck
			return fmt.Errorf("marshaling acked write: %w", err)
		}
		data = append(data, '\n')
		if _, err := file.Write(data); err != nil {
			file.Close() //nolint:errcheck
			return fmt.Errorf("writing acked write: %w", err)
		}
		if err := file.Sync(); err != nil {
			file.Close() //nolint:errcheck
			return fmt.Errorf("fsync acked write: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("closing acked write ledger: %w", err)
		}
	}
	c.mu.Lock()
	c.lastAckTime = ackedAt
	c.ackedIDs = append(c.ackedIDs, id)
	c.mu.Unlock()
	return nil
}

type chaosCreateArgs struct {
	Record Record `json:"record"`
}

type chaosIDArgs struct {
	ID string `json:"id"`
}

type chaosIDsArgs struct {
	IDs []string `json:"ids"`
}

type chaosUpdateArgs struct {
	ID     string `json:"id"`
	Update Update `json:"update"`
}

type chaosQueryArgs struct {
	Query Query `json:"query"`
}

type chaosMetadataArgs struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
}

type chaosReadyArgs struct {
	Ready ReadyQuery `json:"ready"`
}

type chaosDepArgs struct {
	FromID  string `json:"from_id"`
	ToID    string `json:"to_id"`
	DepType string `json:"dep_type"`
}

type chaosDepListArgs struct {
	ID        string `json:"id"`
	Direction string `json:"direction"`
}

type chaosLimitArgs struct {
	Limit int `json:"limit"`
}

type chaosPurgeTerminalArgs struct {
	OlderThan time.Duration `json:"older_than"`
}
