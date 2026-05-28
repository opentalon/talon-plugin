package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/opentalon/talon-language/pkg/talon"
)

// datalevinClient is a minimal HTTP client for the Datalevin server,
// implementing the three methods talon.FactStore requires. It's an
// interim copy of internal/datalevin/client.go from talon-language —
// the SDK doesn't yet export a public constructor for a long-lived
// FactStore (only WithDatalevinURL inside Run/Seed). Once
// talon-language v0.2.1 ships with talon.NewFactStore, this
// type goes away and `factStoreFromURL` returns that directly.
//
// See: https://github.com/opentalon/talon-language/pull/45
type datalevinClient struct {
	baseURL    string
	httpClient *http.Client
}

func newDatalevinClient(baseURL string) *datalevinClient {
	return &datalevinClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// Compile-time guard: when v0.2.1 lands, this assertion holds against
// talon.NewFactStore's return value as well, so the swap
// at the call site is purely typographical.
var _ talon.FactStore = (*datalevinClient)(nil)

type queryResult struct {
	Results [][]any `json:"results"`
}

func (c *datalevinClient) Query(ctx context.Context, query string) ([][]any, error) {
	var out queryResult
	if err := c.post(ctx, "/q", map[string]any{"query": query}, &out); err != nil {
		return nil, fmt.Errorf("datalevin query: %w", err)
	}
	return out.Results, nil
}

func (c *datalevinClient) Transact(ctx context.Context, txData []map[string]any) error {
	var out map[string]any
	if err := c.post(ctx, "/transact", map[string]any{"tx-data": txData}, &out); err != nil {
		return fmt.Errorf("datalevin transact: %w", err)
	}
	return nil
}

func (c *datalevinClient) Schema(ctx context.Context, attrs map[string]map[string]string) error {
	var out map[string]any
	if err := c.post(ctx, "/schema", map[string]any{"schema": attrs}, &out); err != nil {
		return fmt.Errorf("datalevin schema: %w", err)
	}
	return nil
}

func (c *datalevinClient) post(ctx context.Context, path string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
