package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorotrail/internal/rpc"
)

func newTestChain(lps float64) *chain {
	return newChain(200_000, lps, 1_000, 2)
}

func newTestClient(t *testing.T, c *chain) *rpc.HTTPClient {
	t.Helper()
	srv := httptest.NewServer(c.serve())
	t.Cleanup(srv.Close)
	return rpc.NewHTTPClient(srv.URL)
}

func newRawClient(t *testing.T, c *chain) func(method string, params any) ([]byte, error) {
	t.Helper()
	srv := httptest.NewServer(c.serve())
	t.Cleanup(srv.Close)
	return func(method string, params any) ([]byte, error) {
		body, err := json.Marshal(request{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`1`),
			Method:  method,
			Params:  paramsJSON(params),
		})
		if err != nil {
			return nil, err
		}
		resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, err = buf.ReadFrom(resp.Body)
		return buf.Bytes(), err
	}
}

func paramsJSON(p any) json.RawMessage {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	return b
}

func TestHealthAndLatestLedger(t *testing.T) {
	c := newTestChain(1)
	client := newTestClient(t, c)

	health, err := client.GetHealth(context.Background())
	require.NoError(t, err)
	require.Equal(t, "healthy", health.Status)
	require.GreaterOrEqual(t, health.LatestLedger, uint32(200_000))
	require.LessOrEqual(t, health.OldestLedger, health.LatestLedger)

	ll, err := client.GetLatestLedger(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, ll.Sequence, uint32(200_000))
	require.NotEmpty(t, ll.ID)
}

func TestGetEventsPagination(t *testing.T) {
	c := newTestChain(10000) // head advances during the test so paging never runs dry
	client := newTestClient(t, c)

	first, err := client.GetEvents(context.Background(), rpc.GetEventsRequest{
		StartLedger: 200_000,
		Pagination:  &rpc.Pagination{Limit: 3},
	})
	require.NoError(t, err)
	require.Len(t, first.Events, 3, "page 1 returns exactly the requested limit")
	require.NotEmpty(t, first.Cursor, "a full page must carry a continuation cursor")
	require.GreaterOrEqual(t, first.LatestLedger, first.Events[0].Ledger)

	seen := map[string]bool{}
	for _, ev := range first.Events {
		require.False(t, seen[ev.ID], "synthetic events are distinct within a ledger: %s", ev.ID)
		seen[ev.ID] = true
		require.Equal(t, "contract", ev.Type)
		require.True(t, json.Valid(ev.ValueJSON))
		require.Len(t, ev.TopicJSON, 1)
	}

	second, err := client.GetEvents(context.Background(), rpc.GetEventsRequest{
		Pagination: &rpc.Pagination{Cursor: first.Cursor, Limit: 3},
	})
	require.NoError(t, err)
	require.Len(t, second.Events, 3, "cursor resumes past the first page")
	for _, ev := range second.Events {
		require.False(t, seen[ev.ID], "cursor page must not repeat delivered events: %s", ev.ID)
		seen[ev.ID] = true
	}
}

func TestGetEventsBeyondHeadReturnsEmpty(t *testing.T) {
	c := newTestChain(100)
	client := newTestClient(t, c)

	resp, err := client.GetEvents(context.Background(), rpc.GetEventsRequest{
		StartLedger: 1_000_000, // far past the head
		Pagination:  &rpc.Pagination{Limit: 10},
	})
	require.NoError(t, err)
	require.Empty(t, resp.Events, "no events exist beyond the chain head")
	require.Empty(t, resp.Cursor, "empty page must carry no cursor")
}

func TestGetLedgerEntriesAndSimulate(t *testing.T) {
	c := newTestChain(100)
	client := newTestClient(t, c)

	entries, err := client.GetLedgerEntries(context.Background(), rpc.GetLedgerEntriesRequest{
		Keys: []string{"AAAAAAfake-key"},
	})
	require.NoError(t, err)
	require.Empty(t, entries.Entries)

	sim, err := client.SimulateTransaction(context.Background(), rpc.SimulateTransactionRequest{
		Transaction: "fake-envelope",
	})
	require.NoError(t, err)
	require.Empty(t, sim.Results)
	require.Equal(t, rpc.SimulationCost{}, sim.Cost)
}

func TestUnknownMethod(t *testing.T) {
	c := newTestChain(1)
	call := newRawClient(t, c)

	respBytes, err := call("bogusMethod", nil)
	require.NoError(t, err)
	var parsed struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(respBytes, &parsed))
	require.Equal(t, codeMethod, parsed.Error.Code)
	require.Contains(t, parsed.Error.Message, "bogusMethod")
}

func TestDeterministicEvents(t *testing.T) {
	c := newTestChain(100)
	call := newRawClient(t, c)

	params := map[string]any{
		"startLedger": 200_000,
		"pagination":  map[string]any{"cursor": "", "limit": 4},
	}
	b1, err := call("getEvents", params)
	require.NoError(t, err)
	b2, err := call("getEvents", params)
	require.NoError(t, err)

	var r1, r2 struct {
		Result rpc.GetEventsResponse `json:"result"`
	}
	require.NoError(t, json.Unmarshal(b1, &r1))
	require.NoError(t, json.Unmarshal(b2, &r2))
	require.Equal(t, r1.Result.Events, r2.Result.Events, "the same request must return the same events")
}