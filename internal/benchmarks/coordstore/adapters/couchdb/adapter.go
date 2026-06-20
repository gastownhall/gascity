// Package couchdb provides a CouchDB-backed StoreAdapter for the benchmark
// sweep. It persists every write to CouchDB and serves benchmark reads through
// the same in-process hot model the production design would need for FR-16.
package couchdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/internal/memstore"
)

// Adapter is a write-through CouchDB adapter with an in-process read model.
type Adapter struct {
	*memstore.Adapter
	baseURL string
	client  *http.Client
}

// New returns a CouchDB adapter. baseURL must include the database name.
func New(baseURL string) *Adapter {
	a := &Adapter{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	a.Adapter = memstore.New("cb", a)
	return a
}

// Open ensures the database exists and loads any existing documents.
func (a *Adapter) Open(ctx context.Context, _ coordstore.Config) error {
	if err := a.ensureDB(ctx); err != nil {
		return err
	}
	if err := a.ResetBacking(ctx); err != nil {
		return err
	}
	a.ReplaceState(nil, nil)
	return nil
}

// SaveRecord writes a record document.
func (a *Adapter) SaveRecord(ctx context.Context, r coordstore.Record) error {
	docID := recordDocID(r.ID)
	rev, err := a.currentRev(ctx, docID)
	if err != nil {
		return err
	}
	doc := recordDoc{ID: docID, Rev: rev, DocType: "record", Record: r}
	return a.putJSON(ctx, docID, doc)
}

// DeleteRecord removes a record document.
func (a *Adapter) DeleteRecord(ctx context.Context, id string, _ bool) error {
	return a.deleteDoc(ctx, recordDocID(id))
}

// SaveDep writes a dependency document.
func (a *Adapter) SaveDep(ctx context.Context, d coordstore.Dep) error {
	docID := depDocID(d.FromID, d.ToID)
	rev, err := a.currentRev(ctx, docID)
	if err != nil {
		return err
	}
	doc := depDoc{ID: docID, Rev: rev, DocType: "dep", Dep: d}
	return a.putJSON(ctx, docID, doc)
}

// DeleteDep removes a dependency document.
func (a *Adapter) DeleteDep(ctx context.Context, fromID, toID string) error {
	return a.deleteDoc(ctx, depDocID(fromID, toID))
}

// ResetBacking recreates the CouchDB database.
func (a *Adapter) ResetBacking(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.baseURL, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("couchdb: delete db: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("couchdb: delete db: %s", resp.Status)
	}
	return a.ensureDB(ctx)
}

func (a *Adapter) ensureDB(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.baseURL, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("couchdb: create db: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusAccepted, http.StatusPreconditionFailed:
		return nil
	default:
		return fmt.Errorf("couchdb: create db: %s", resp.Status)
	}
}

func (a *Adapter) currentRev(ctx context.Context, docID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.docURL(docID), nil)
	if err != nil {
		return "", err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("couchdb: get rev %s: %w", docID, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("couchdb: get rev %s: %s: %s", docID, resp.Status, string(body))
	}
	var header struct {
		Rev string `json:"_rev"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&header); err != nil {
		return "", err
	}
	return header.Rev, nil
}

func (a *Adapter) putJSON(ctx context.Context, docID string, doc any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(doc); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.docURL(docID), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("couchdb: put %s: %w", docID, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("couchdb: put %s: %s", docID, resp.Status)
	}
	return nil
}

func (a *Adapter) deleteDoc(ctx context.Context, docID string) error {
	rev, err := a.currentRev(ctx, docID)
	if err != nil {
		return err
	}
	if rev == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.docURL(docID)+"?rev="+url.QueryEscape(rev), nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("couchdb: delete %s: %w", docID, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("couchdb: delete %s: %s", docID, resp.Status)
	}
	return nil
}

func (a *Adapter) docURL(docID string) string {
	return a.baseURL + "/" + url.PathEscape(docID)
}

type recordDoc struct {
	ID      string            `json:"_id"`
	Rev     string            `json:"_rev,omitempty"`
	DocType string            `json:"doc_type"`
	Record  coordstore.Record `json:"record"`
}

type depDoc struct {
	ID      string         `json:"_id"`
	Rev     string         `json:"_rev,omitempty"`
	DocType string         `json:"doc_type"`
	Dep     coordstore.Dep `json:"dep"`
}

func recordDocID(id string) string { return "record:" + id }

func depDocID(fromID, toID string) string {
	return "dep:" + fromID + "\x00" + toID
}
