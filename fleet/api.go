package fleet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The explorer's API under concurrent readers: the node serves it from the
// same process that derives and indexes, so this is also the check of whether
// a busy explorer slows the chain.

type Endpoint struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"-"`
	Heavy  bool   `json:"heavy,omitempty"`
}

type LoadRow struct {
	Endpoint  string         `json:"endpoint"`
	Path      string         `json:"path"`
	Conc      int            `json:"concurrency"`
	Seconds   float64        `json:"seconds"`
	Requests  int            `json:"requests"`
	Errors    int            `json:"errors"`
	RPS       float64        `json:"rps"`
	Latency   Dist           `json:"latencyMs"`
	MeanBytes int64          `json:"meanBytes"`
	Statuses  map[string]int `json:"statuses"`
}

func (e *Env) endpoints(ctx context.Context) []Endpoint {
	head, _ := e.Eth.BlockNumber(ctx)
	busy := e.Dex.Funder.Hex()
	token := e.Dex.ERC20.Hex()
	pair, _ := e.pairOf(ctx, e.Dex.WETH, e.Dex.ERC20)
	coll := e.state.Collection
	if coll.Big().Sign() == 0 && len(e.Apps.Collections) > 0 {
		coll = e.Apps.Collections[0]
	}
	var recent []map[string]any
	e.getJSON(ctx, "/api/txs?limit=1&users=1", &recent)
	txHash := ""
	if len(recent) > 0 {
		txHash, _ = recent[0]["hash"].(string)
	}
	rpc := func(method, params string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}`, method, params)
	}
	get := func(name, path string, heavy bool) Endpoint {
		return Endpoint{Name: name, Method: http.MethodGet, Path: path, Heavy: heavy}
	}
	post := func(name, path, body string) Endpoint {
		return Endpoint{Name: name, Method: http.MethodPost, Path: path, Body: body}
	}
	return []Endpoint{
		get("info", "/api/info", false),
		get("stats", "/api/stats", false),
		get("ops", "/api/ops", false),
		get("blocks", "/api/blocks?limit=25", false),
		get("block", fmt.Sprintf("/api/blocks/%d", head-1), false),
		get("txs", "/api/txs?limit=25", false),
		get("tx", "/api/txs/"+txHash, false),
		get("momentums", "/api/momentums", false),
		get("address", "/api/address/"+busy, true),
		get("address txs", "/api/address/"+busy+"/txs", true),
		get("address transfers", "/api/address/"+busy+"/transfers", true),
		get("tokens", "/api/tokens", true),
		get("token", "/api/tokens/"+token, true),
		get("token holders", "/api/tokens/"+token+"/holders", true),
		get("token transfers", "/api/tokens/"+token+"/transfers", false),
		get("dex pairs", "/api/dex/pairs", true),
		get("candles", "/api/dex/pairs/"+pair.Hex()+"/candles", true),
		get("trades", "/api/dex/pairs/"+pair.Hex()+"/trades", false),
		get("nft collections", "/api/nft/collections", true),
		get("nft items", "/api/nft/collections/"+coll.Hex()+"/items", true),
		get("nft listings", "/api/nft/listings", true),
		get("nft sales", "/api/nft/sales", false),
		get("search", "/api/search?q="+busy, false),
		get("verified marks", "/api/contracts/verified?addresses="+strings.Join([]string{token, pair.Hex(), coll.Hex()}, ","), false),
		get("prices", "/api/prices", false),
		post("rpc eth_blockNumber", "/rpc", rpc("eth_blockNumber", "[]")),
		post("rpc eth_getBalance", "/rpc", rpc("eth_getBalance", `["`+busy+`","latest"]`)),
		post("l1rpc frontier momentum", "/l1rpc", rpc("ledger.getFrontierMomentum", "[]")),
	}
}

func (e *Env) ladder(ctx context.Context, eps []Endpoint, concs []int, each time.Duration) []LoadRow {
	var rows []LoadRow
	for _, ep := range eps {
		for _, c := range concs {
			if ctx.Err() != nil {
				return rows
			}
			rows = append(rows, e.load(ctx, []Endpoint{ep}, c, each))
		}
	}
	return rows
}

// load drives conc workers against eps, round-robin, for d — or until ctx ends
// if d is zero.
func (e *Env) load(ctx context.Context, eps []Endpoint, conc int, d time.Duration) LoadRow {
	names := make([]string, len(eps))
	for i, ep := range eps {
		names[i] = ep.Name
	}
	row := LoadRow{Endpoint: strings.Join(names, ", "), Conc: conc, Statuses: map[string]int{}}
	if len(eps) == 1 {
		row.Path = eps[0].Path
	}
	lctx := ctx
	if d > 0 {
		var cancel context.CancelFunc
		lctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	var (
		mu    sync.Mutex
		lat   []float64
		bytes int64
		wg    sync.WaitGroup
	)
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; lctx.Err() == nil; i++ {
				ep := eps[i%len(eps)]
				var body io.Reader
				if ep.Body != "" {
					body = strings.NewReader(ep.Body)
				}
				req, err := http.NewRequestWithContext(lctx, ep.Method, e.API+ep.Path, body)
				if err != nil {
					return
				}
				if ep.Body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				t0 := time.Now()
				resp, err := e.http.Do(req)
				var n int64
				status := "error"
				if err == nil {
					n, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					status = fmt.Sprint(resp.StatusCode)
				}
				took := time.Since(t0)
				if lctx.Err() != nil {
					return
				}
				mu.Lock()
				row.Requests++
				row.Statuses[status]++
				if err != nil || resp.StatusCode >= 400 {
					row.Errors++
				}
				lat = append(lat, float64(took.Microseconds())/1000)
				bytes += n
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	row.Seconds = time.Since(start).Seconds()
	row.Latency = dist(lat)
	if row.Seconds > 0 {
		row.RPS = float64(row.Requests) / row.Seconds
	}
	if row.Requests > 0 {
		row.MeanBytes = bytes / int64(row.Requests)
	}
	return row
}
