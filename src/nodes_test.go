package balancekeeper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

func TestManualReadUsesVerifiedBackupAndReturnsToPrimary(t *testing.T) {
	var down, wrong atomic.Bool
	var backupReads atomic.Int32
	server := func(backup bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := "primary-secret"
			if backup {
				token = "backup-secret"
			}
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("credential crossed providers")
			}
			raw, _ := io.ReadAll(r.Body)
			req := gjson.ParseBytes(raw)
			var result any
			switch req.Get("method").Str {
			case "eth_chainId":
				result = "0x2105"
				if backup && wrong.Load() {
					result = "0x1"
				}
			case "eth_blockNumber":
				result = "0x10"
			case "eth_getBlockByNumber":
				result = M{"number": "0x10", "hash": "0x" + strings.Repeat("f", 64), "timestamp": "0x10"}
			case "eth_getBalance":
				if !backup && down.Load() {
					http.Error(w, "busy", 429)
					return
				}
				result = "0x1"
				if backup {
					backupReads.Add(1)
					result = "0x2"
				}
			default:
				t.Errorf("unexpected request %s", req.Raw)
			}
			json.NewEncoder(w).Encode(M{"jsonrpc": "2.0", "id": 1, "result": result})
		}))
	}
	primary, backup := server(false), server(true)
	defer primary.Close()
	defer backup.Close()
	cfg, e := configureNode("base", Network{RPC: primary.URL, Bearer: "primary-secret", Backup: &NodeEndpoints{RPC: backup.URL, Bearer: "backup-secret"}})
	if e != nil {
		t.Fatal(e)
	}
	s := dbService(t, cfg)
	registerActive(t, s, walletA)
	batch := []M{{"network": "base", "address": walletA, "tokens": []string{"native"}}}
	refresh := func() gjson.Result { return post(t, s, "/v1/refreshes", batch, 200).Get("results.0") }
	if v := refresh(); v.Get("balances.native").Str != "1" || backupReads.Load() != 0 {
		t.Fatal(v)
	}
	down.Store(true)
	if v := refresh(); v.Get("balances.native").Str != "2" || backupReads.Load() != 1 {
		t.Fatal(v)
	}
	wrong.Store(true)
	if v := refresh(); v.Get("status").Str != "failed" || backupReads.Load() != 1 {
		t.Fatal(v)
	}
	down.Store(false)
	if v := refresh(); v.Get("balances.native").Str != "1" || backupReads.Load() != 1 {
		t.Fatal(v)
	}
	if got := post(t, s, "/v1/snapshots/query", batch, 200).Get("results.0.items"); len(got.Array()) != 3 {
		t.Fatal(got)
	}
}

func TestInventoryRestartsPaginationAtBackup(t *testing.T) {
	assetA, assetB := addressFor(85), addressFor(86)
	var first, second atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		v := gjson.ParseBytes(raw)
		var result any
		switch v.Get("method").Str {
		case "eth_chainId":
			result = "0x2105"
		case "eth_blockNumber":
			result = "0x10"
		case "eth_getBlockByNumber":
			result = M{"number": "0x10", "hash": "0x" + strings.Repeat("f", 64), "timestamp": "0x10"}
		case "alchemy_getTokenBalances":
			page := v.Get("params.2.pageKey").Str
			switch r.URL.Path {
			case "/first-index":
				first.Add(1)
				if page != "" {
					http.Error(w, "upstream unavailable", 503)
					return
				}
				result = M{"address": walletA, "pageKey": "primary-cursor", "tokenBalances": []M{{"contractAddress": assetA, "tokenBalance": "0x1"}}}
			case "/second-index":
				second.Add(1)
				if page != "" || r.Header.Get("Authorization") != "" {
					t.Error("primary cursor or token used on backup")
				}
				result = M{"address": walletA, "tokenBalances": []M{{"contractAddress": assetB, "tokenBalance": "0x2"}}}
			default:
				t.Errorf("inventory requested from ordinary RPC: %s", r.URL.Path)
			}
		default:
			t.Error(v.Raw)
		}
		json.NewEncoder(w).Encode(M{"id": 1, "result": result})
	}))
	defer server.Close()
	cfg, e := configureNode("base", Network{RPC: server.URL + "/main", InventoryRPC: server.URL + "/first-index", InventoryBearer: "secret", Backup: &NodeEndpoints{RPC: server.URL + "/standby", InventoryRPC: server.URL + "/second-index"}})
	if e != nil {
		t.Fatal(e)
	}
	s := dbService(t, cfg)
	registerActive(t, s, walletA)
	if worked, err := s.nextInventory(t.Context(), "base"); !worked || err != nil {
		t.Fatal(worked, err)
	}
	var token string
	if e := s.db.QueryRow("SELECT group_concat(token) FROM jobs").Scan(&token); e != nil || token != assetB || first.Load() != 2 || second.Load() != 1 {
		t.Fatal(token, first.Load(), second.Load(), e)
	}
}

func TestNetworkPresetsAndProtocols(t *testing.T) {
	names := []string{"bitcoin", "aptos", "solana", "sui", "ton", "tron"}
	for name := range chainIDs {
		names = append(names, name)
	}
	raw, err := os.ReadFile("../config.mainnets.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var example Config
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatal(err)
	}
	if len(example.Networks) != len(names) || len(defaultBackups) != len(names) {
		t.Fatal("preset count differs from supported network count")
	}
	for _, name := range names {
		entry := example.Networks[name]
		if !strings.HasPrefix(entry.RPC, "${") || entry.Backup == nil || entry.Backup.RPC != defaultBackups[name] {
			t.Fatalf("%s example does not match the primary/backup configuration", name)
		}
		cfg, e := configureNode(name, Network{RPC: "https://operator.example", Bearer: "private"})
		if e != nil || cfg.RPC != "https://operator.example" || cfg.Backup == nil || !validURL(cfg.Backup.RPC) || cfg.Backup.Bearer != "" {
			t.Fatal(name, cfg, e)
		}
	}
	t.Setenv("BK_TEST_KEY", "value")
	cfg, e := configureNode("ton", Network{RPC: "https://n.example/api/v2/", APIKey: "${BK_TEST_KEY}", Index: "https://i.example/api/v3"})
	if e != nil || cfg.RPC != "https://n.example" || cfg.Index != "https://i.example" || cfg.APIKey != "value" || cfg.Backup.APIKey != "" {
		t.Fatal(cfg, e)
	}
	for _, name := range []string{"tron", "ton"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := "X-API-Key"
				if name == "tron" {
					key = "TRON-PRO-API-KEY"
				}
				if r.Header.Get(key) != "test" || r.URL.Query().Get("project") != "x" || r.URL.Query().Get("address") != "y" {
					t.Error("missing route parameters or key", r.URL)
				}
				fmt.Fprint(w, "{}")
			}))
			defer server.Close()
			s := Service{cfg: Config{Networks: map[string]Network{name: {RPC: server.URL + "?project=x", APIKey: "test"}}}, http: server.Client()}
			if _, e := s.rest(context.Background(), name, "/read?address=y", nil); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestTransactionRetriesMissingTraceAtBackup(t *testing.T) {
	hash := "0x" + strings.Repeat("1", 64)
	block := "0x" + strings.Repeat("2", 64)
	var traces, backupReceipts atomic.Int32
	var backupTraceDisabled atomic.Bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		method := gjson.GetBytes(raw, "method").Str
		var res any
		switch method {
		case "eth_chainId":
			res = "0x2105"
		case "eth_blockNumber":
			res = "0xa"
		case "eth_getBlockByNumber":
			res = M{"number": "0xa", "hash": block, "timestamp": "0x1"}
		case "eth_getTransactionByHash":
			res = M{"hash": hash, "from": walletA, "blockNumber": "0xa", "blockHash": block}
		case "eth_getTransactionReceipt":
			if r.URL.Path == "/backup" {
				backupReceipts.Add(1)
			}
			res = M{"transactionHash": hash, "blockNumber": "0xa", "blockHash": block, "logs": []M{}, "status": "0x1"}
		case "debug_traceTransaction":
			traces.Add(1)
			if r.URL.Path != "/backup" || backupTraceDisabled.Load() {
				json.NewEncoder(w).Encode(M{"id": 1, "error": M{"code": -32601, "message": "method not supported"}})
				return
			}
			res = M{"type": "CALL", "from": walletA, "to": walletB, "value": "0x1"}
		default:
			t.Error(method)
		}
		json.NewEncoder(w).Encode(M{"id": 1, "result": res})
	}))
	defer mock.Close()
	s := dbService(t, Network{RPC: mock.URL + "/primary", Backup: &NodeEndpoints{RPC: mock.URL + "/backup"}})
	notification := gjson.Parse(compact(M{"activity": M{"tx_id": hash, "height": 10}}))
	v, err := s.transactionAssets(t.Context(), "base", "chainscanner", notification)
	if err != nil || !v.candidates[walletB]["native"] || traces.Load() != 2 || backupReceipts.Load() != 1 {
		t.Fatal(err, v.candidates, traces.Load(), backupReceipts.Load())
	}
	backupTraceDisabled.Store(true)
	v, err = s.transactionAssets(t.Context(), "base", "chainscanner", notification)
	if err != nil || !v.candidates[walletA]["native"] || v.traceRetry != nil || !v.data.Get("receipt.logs").IsArray() {
		t.Fatal("standard RPC receipt lost when trace unavailable", err, v)
	}
}
