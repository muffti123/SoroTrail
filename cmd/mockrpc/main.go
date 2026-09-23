// Command mockrpc is a deterministic Stellar (Soroban) JSON-RPC 2.0 mock for
// local development and the docker compose `test` profile.
//
// It serves the JSON-RPC methods SoroTrail's indexer consumes — getHealth,
// getLatestLedger, getEvents, getLedgerEntries, simulateTransaction — against
// a fake chain whose head advances at a configurable rate, so an ingester
// pointed at it sees a healthy, slowly-moving network without any real RPC,
// network egress, or external credentials.
//
// Events are synthesized deterministically: the same (ledger, sequence)
// always yields the same event, so catch-up, cursor paging, and reorg
// windows behave repeatably across runs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sorotrail/sorotrail/internal/rpc"
)

// contractIDs is the pool of strkey-format contract addresses synthetic
// events cycle through, so a single mock chain exercises several contracts.
var contractIDs = []string{
	"CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	"CCW67TSBWVENNVMTQPEXNGXYL6G5CZWKW563CYCPBQR27XMTC2AFAXXT",
	"CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
}

const protocolVersion = 22

// maxBodyBytes bounds a single JSON-RPC request body.
const maxBodyBytes = 1 << 20

// request is a JSON-RPC 2.0 request as sent by internal/rpc's client.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is a JSON-RPC 2.0 response.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("mockrpc: %s (%d)", e.Message, e.Code)
}

const (
	codeParse    = -32700
	codeInvalid  = -32600
	codeMethod   = -32601
	codeParams   = -32602
	codeInternal = -32603
)

// getEventsParams is the subset of the getEvents params the mock reads. It
// duplicates the field names of internal/rpc.GetEventsRequest so the mock
// tolerates any JSON the client sends without depending on client internals.
type getEventsParams struct {
	StartLedger uint32        `json:"startLedger"`
	EndLedger   uint32        `json:"endLedger"`
	Filters     []interface{} `json:"filters"`
	Pagination  *pagination   `json:"pagination"`
}

type pagination struct {
	Cursor string `json:"cursor"`
	Limit  uint   `json:"limit"`
}

// chain models a live Stellar network. The ledger head advances from
// startLedger as wall-clock time passes, so health-probing clients see a
// moving chain with no background goroutine or external clock.
type chain struct {
	mu sync.Mutex

	startLedger uint32
	head        uint32
	boot        time.Time
	baseTime    time.Time

	ledgersPerSec    float64
	retention        uint32
	eventsPerLedger  uint32
}

func newChain(startLedger uint32, ledgersPerSec float64, retention, eventsPerLedger uint32) *chain {
	return &chain{
		startLedger:      startLedger,
		head:             startLedger,
		boot:             time.Now(),
		baseTime:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ledgersPerSec:    ledgersPerSec,
		retention:        retention,
		eventsPerLedger:  eventsPerLedger,
	}
}

// current returns the chain head, advancing it by the wall-clock time elapsed
// since boot. Guarded by mu so concurrent getHealth/getEvents requests agree
// on the head.
func (c *chain) current() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	elapsed := time.Since(c.boot).Seconds()
	c.head = c.startLedger + uint32(elapsed*c.ledgersPerSec)
	return c.head
}

// oldest returns the oldest retained ledger, clamped so young chains don't
// underflow.
func (c *chain) oldest(head uint32) uint32 {
	if head <= c.retention {
		return 1
	}
	return head - c.retention
}

// eventAt synthesizes the deterministic event at (ledger, seq).
func (c *chain) eventAt(ledger uint32, seq uint32) rpc.Event {
	contract := contractIDs[uint64(seq)%uint64(len(contractIDs))]
	closed := c.baseTime.Add(time.Duration(ledger) * 5 * time.Second).UTC()
	return rpc.Event{
		ID:                       fmt.Sprintf("%010d-%04d", ledger, seq),
		PagingToken:              fmt.Sprintf("%010d-%04d", ledger, seq),
		Type:                     "contract",
		Ledger:                   ledger,
		LedgerClosedAt:           closed.Format(time.RFC3339),
		ContractID:               contract,
		TxHash:                   fmt.Sprintf("%064x", uint64(ledger)*10000+uint64(seq)),
		TransactionIndex:         int32(seq % 8),
		OperationIndex:           int32(seq % 2),
		InSuccessfulContractCall: true,
		TopicJSON:                []json.RawMessage{json.RawMessage(`{"symbol":"transfer"}`)},
		ValueJSON:                json.RawMessage(fmt.Sprintf(`{"u64":%d}`, seq*2+1)),
	}
}

// handleGetEvents pages synthetic events across [begin, end), starting after
// the continuation cursor when one is supplied, and returns a top-level
// cursor pointing at the last delivered event whenever more data remains.
func (c *chain) handleGetEvents(p getEventsParams) (rpc.GetEventsResponse, error) {
	head := c.current()
	oldest := c.oldest(head)

	begin := p.StartLedger
	if begin < oldest {
		begin = oldest
	}
	end := head + 1 // EndLedger is exclusive
	if p.EndLedger > 0 && p.EndLedger < end {
		end = p.EndLedger
	}

	limit := uint(100)
	if p.Pagination != nil && p.Pagination.Limit > 0 && p.Pagination.Limit <= rpc.MaxEventsPerRequest {
		limit = p.Pagination.Limit
	}

	posLedger, posSeq := begin, uint32(0)
	if p.Pagination != nil && p.Pagination.Cursor != "" {
		cursorLedger, cursorSeq, err := parseCursor(p.Pagination.Cursor)
		if err != nil {
			return rpc.GetEventsResponse{}, &rpcError{Code: codeParams, Message: err.Error()}
		}
		posLedger, posSeq = cursorLedger, cursorSeq
		// Normalize an out-of-band seq (e.g. an old cursor with a higher
		// events-per-ledger) to the next valid start position.
		posLedger, posSeq = advancePos(posLedger, posSeq, c.eventsPerLedger)
	}

	var (
		events []rpc.Event
		cursor string
	)
	for posLedger < end && uint(len(events)) < limit {
		ev := c.eventAt(posLedger, posSeq)
		events = append(events, ev)
		cursor = fmt.Sprintf("%d:%d", posLedger, posSeq)
		posLedger, posSeq = advancePos(posLedger, posSeq, c.eventsPerLedger)
	}

	// Nothing left behind the cursor: drop it so the ingester treats the
	// page as terminal instead of looping forever on a stale cursor.
	if posLedger >= end {
		cursor = ""
	}

	return rpc.GetEventsResponse{
		Events:       events,
		LatestLedger: head,
		OldestLedger: oldest,
		Cursor:       cursor,
	}, nil
}

// advancePos steps the (ledger, seq) scan position to the next synthetic
// event, wrapping seq at the per-ledger event count.
func advancePos(ledger, seq, eventsPerLedger uint32) (uint32, uint32) {
	seq++
	if seq >= eventsPerLedger {
		seq = 0
		ledger++
	}
	return ledger, seq
}

func parseCursor(s string) (uint32, uint32, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed mockrpc cursor %q", s)
	}
	var ledger, seq uint32
	if _, err := fmt.Sscanf(parts[0], "%d", &ledger); err != nil {
		return 0, 0, fmt.Errorf("malformed mockrpc cursor %q", s)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &seq); err != nil {
		return 0, 0, fmt.Errorf("malformed mockrpc cursor %q", s)
	}
	return ledger, seq, nil
}

// handle returns the result for one JSON-RPC method. Unknown methods yield a
// *rpcError with code codeMethod.
func (c *chain) handle(method string, params json.RawMessage) (any, error) {
	switch method {
	case "getHealth":
		head := c.current()
		return rpc.Health{
			Status:                "healthy",
			LatestLedger:          head,
			OldestLedger:          c.oldest(head),
			LedgerRetentionWindow: c.retention,
		}, nil

	case "getLatestLedger":
		head := c.current()
		return rpc.LatestLedger{
			ID:              fmt.Sprintf("Ledger-%010d", head),
			ProtocolVersion: protocolVersion,
			Sequence:        head,
		}, nil

	case "getEvents":
		var p getEventsParams
		if len(params) > 0 && string(params) != "null" {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &rpcError{Code: codeParams, Message: fmt.Sprintf("invalid getEvents params: %v", err)}
			}
		}
		return c.handleGetEvents(p)

	case "getLedgerEntries":
		return rpc.GetLedgerEntriesResponse{Entries: []rpc.LedgerEntryResult{}}, nil

	case "simulateTransaction":
		return rpc.SimulateTransactionResponse{
			Results: []json.RawMessage{},
			Cost:    rpc.SimulationCost{CPUInstructions: 0, MemoryBytes: 0},
		}, nil

	default:
		return nil, &rpcError{Code: codeMethod, Message: fmt.Sprintf("method %q not found", method)}
	}
}

func (c *chain) serve() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err := dec.Decode(&req); err != nil {
			writeRPCError(w, codeParse, "cannot parse request body: "+err.Error())
			return
		}
		if req.Method == "" {
			writeRPCError(w, codeInvalid, "missing method")
			return
		}

		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}

		result, err := c.handle(req.Method, req.Params)
		if err != nil {
			var rpcE *rpcError
			if !errors.As(err, &rpcE) {
				rpcE = &rpcError{Code: codeInternal, Message: err.Error()}
			}
			writeJSON(w, response{JSONRPC: "2.0", ID: id, Error: rpcE})
			return
		}
		writeJSON(w, response{JSONRPC: "2.0", ID: id, Result: result})
	})
}

func writeRPCError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

type options struct {
	ledgersPerSec   float64
	startLedger     uint32
	retention       uint32
	eventsPerLedger uint32
}

func main() {
	addr := flag.String("addr", ":8081", "HTTP listen address")
	lps := flag.Float64("ledgers-per-second", 2, "how fast the mock chain head advances")
	startLedger := flag.Uint("start-ledger", 200_000, "ledger sequence the mock chain starts from")
	retention := flag.Uint("retention-ledgers", 17_280, "retained ledger window reported by getHealth/getEvents")
	eventsPerLedger := flag.Uint("events-per-ledger", 2, "synthetic events emitted per ledger")
	flag.Parse()

	opts := &options{
		ledgersPerSec:   *lps,
		startLedger:     uint32(*startLedger),
		retention:       uint32(*retention),
		eventsPerLedger: uint32(*eventsPerLedger),
	}
	if err := run(*addr, opts); err != nil {
		log.Fatalf("mockrpc: %v", err)
	}
}

// run boots the HTTP server and blocks until SIGINT/SIGTERM or a fatal
// server error. Kept separate from main so tests can exercise it.
func run(addr string, opts *options) error {
	if opts.ledgersPerSec <= 0 {
		return fmt.Errorf("ledgers-per-second must be positive, got %v", opts.ledgersPerSec)
	}
	if opts.eventsPerLedger == 0 {
		return fmt.Errorf("events-per-ledger must be positive, got %d", opts.eventsPerLedger)
	}

	c := newChain(opts.startLedger, opts.ledgersPerSec, opts.retention, opts.eventsPerLedger)
	srv := &http.Server{
		Addr:              addr,
		Handler:           c.serve(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("mockrpc listening on %s (chain %d, %g ledgers/s)", addr, opts.startLedger, opts.ledgersPerSec)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}