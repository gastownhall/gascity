package coordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
)

// RunChaosServer serves StoreAdapter requests on socketPath until ctx is
// canceled or the process exits.
func RunChaosServer(ctx context.Context, adapter StoreAdapter, socketPath string, ready io.Writer) error {
	if socketPath == "" {
		return fmt.Errorf("chaos server socket path is required")
	}
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("chaos listen: %w", err)
	}
	defer ln.Close() //nolint:errcheck
	if ready != nil {
		fmt.Fprintln(ready, "READY") //nolint:errcheck
	}
	go func() {
		<-ctx.Done()
		ln.Close() //nolint:errcheck
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("chaos accept: %w", err)
		}
		go handleChaosConn(ctx, adapter, conn)
	}
}

func handleChaosConn(ctx context.Context, adapter StoreAdapter, conn net.Conn) {
	defer conn.Close() //nolint:errcheck
	var req ChaosRequest
	encoder := json.NewEncoder(conn)
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = encoder.Encode(ChaosResponse{Err: err.Error()})
		return
	}
	result, err := dispatchChaosRequest(ctx, adapter, req)
	if err != nil {
		_ = encoder.Encode(ChaosResponse{Err: err.Error()})
		return
	}
	_ = encoder.Encode(ChaosResponse{Result: result})
}

func dispatchChaosRequest(ctx context.Context, adapter StoreAdapter, req ChaosRequest) (json.RawMessage, error) {
	marshal := func(v any, err error) (json.RawMessage, error) {
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	}
	switch req.Method {
	case "Reset":
		return nil, adapter.Reset(ctx)
	case "Create":
		var args chaosCreateArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return marshal(adapter.Create(ctx, args.Record))
	case "Get":
		var args chaosIDArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return marshal(adapter.Get(ctx, args.ID))
	case "Update":
		var args chaosUpdateArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return nil, adapter.Update(ctx, args.ID, args.Update)
	case "Delete":
		var args chaosIDArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return nil, adapter.Delete(ctx, args.ID)
	case "FilterScan":
		var args chaosQueryArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return marshal(adapter.FilterScan(ctx, args.Query))
	case "BatchGet":
		var args chaosIDsArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return marshal(adapter.BatchGet(ctx, args.IDs))
	case "SetMetadataBatch":
		var args chaosMetadataArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return nil, adapter.SetMetadataBatch(ctx, args.ID, args.Metadata)
	case "Ready":
		var args chaosReadyArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return marshal(adapter.Ready(ctx, args.Ready))
	case "DepAdd":
		var args chaosDepArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return nil, adapter.DepAdd(ctx, args.FromID, args.ToID, args.DepType)
	case "DepRemove":
		var args chaosDepArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return nil, adapter.DepRemove(ctx, args.FromID, args.ToID)
	case "DepList":
		var args chaosDepListArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return marshal(adapter.DepList(ctx, args.ID, args.Direction))
	case "PurgeExpired":
		return marshal(adapter.PurgeExpired(ctx))
	case "PurgeTerminal":
		var args chaosPurgeTerminalArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return marshal(adapter.PurgeTerminal(ctx, args.OlderThan))
	case "PrimeScan":
		return marshal(adapter.PrimeScan(ctx))
	case "RecentScan":
		var args chaosLimitArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return marshal(adapter.RecentScan(ctx, args.Limit))
	case "Stats":
		return json.Marshal(adapter.Stats(ctx))
	default:
		return nil, fmt.Errorf("unknown chaos method %q", req.Method)
	}
}
