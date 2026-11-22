package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/tidwall/gjson"
)

func wordAddress(a string) string { return "0x" + strings.Repeat("0", 24) + a[2:] }
func addressFor(i int) string     { return fmt.Sprintf("0x%040x", i) }
func dbService(t *testing.T, n Network) *Service {
	t.Helper()
	s, e := openService(Config{Database: filepath.Join(t.TempDir(), "balances.db"), Token: "test", Networks: map[string]Network{"base": n}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}
func registerActive(t *testing.T, s *Service, a string) int64 {
	t.Helper()
	var id int64
	err := s.db.QueryRow("INSERT INTO watches(network,address,initial,start,status) VALUES('base',?,'{}',9,'active') RETURNING id", a).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func drain(t *testing.T, fn func(context.Context) (bool, error)) {
	t.Helper()
	for range 300 {
		work, e := fn(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		if !work {
			return
		}
	}
	t.Fatal("queue did not drain")
}

func TestPartialDeliveryBundledSwapAndDelegatedExecution(t *testing.T) {
	// A bundler pays gas, two accounts execute, an output token has never
	// been registered, and a freshly created contract changes a balance silently.
	key, e := crypto.GenerateKey()
	if e != nil {
		t.Fatal(e)
	}
	authority := strings.ToLower(crypto.PubkeyToAddress(key.PublicKey).Hex())
	delegate, router, paymaster := addressFor(31), addressFor(32), addressFor(33)
	input, output, silent, nft, known := addressFor(41), addressFor(42), addressFor(43), addressFor(44), addressFor(45)
	failed := addressFor(46)
	secondAccount, mintRecipient := addressFor(47), addressFor(48)
	auth, e := types.SignSetCode(key, types.SetCodeAuthorization{Address: common.HexToAddress(delegate)})
	if e != nil {
		t.Fatal(e)
	}
	hash, block := "0x"+strings.Repeat("a", 64), "0x"+strings.Repeat("b", 64)
	transfer := func(contract, from, to string) M {
		return M{"address": contract, "topics": []string{"0x" + transferTopic, wordAddress(from), wordAddress(to)}, "data": fmt.Sprintf("0x%064x", 7)}
	}
	logs := []M{{"address": router, "topics": []string{userOpTopic, block, wordAddress(secondAccount), wordAddress(paymaster)}, "data": "0x" + strings.Repeat("0", 256)}, transfer(input, walletA, router), transfer(output, router, walletA), transfer(output, router, walletA),
		{"address": router, "topics": []string{userOpTopic, hash, wordAddress(walletB), wordAddress(paymaster)}, "data": "0x" + strings.Repeat("0", 256)}}
	tx := M{"hash": hash, "from": addressFor(30), "to": router, "value": "0x0", "blockNumber": "0xa", "blockHash": block, "type": "0x4", "authorizationList": []types.SetCodeAuthorization{auth}}
	receipt := M{"transactionHash": hash, "blockHash": block, "blockNumber": "0xa", "logs": logs, "status": "0x1"}
	trace := M{"type": "CALL", "from": addressFor(30), "to": router, "calls": []M{
		{"type": "CALL", "from": router, "to": authority, "calls": []M{
			{"type": "DELEGATECALL", "from": authority, "to": delegate, "calls": []M{{"type": "CREATE2", "from": authority, "to": silent}, {"type": "CALL", "from": authority, "to": silent, "input": "0x40c10f19" + wordAddress(mintRecipient)[2:] + fmt.Sprintf("%064x", 17)}}}}},
		{"type": "CALL", "from": router, "to": nft, "input": "0x23b872dd" + wordAddress(walletA)[2:] + wordAddress(walletB)[2:] + fmt.Sprintf("%064x", 1)},
		{"type": "CALL", "from": router, "to": failed, "error": "execution reverted"}}}
	var ready atomic.Bool
	amounts := map[string]int{output: 25, input: 0, silent: 17, known: 18, nft: 1}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := gjson.ParseBytes(b)
		var v any
		switch req.Get("method").Str {
		case "eth_getTransactionByHash":
			v = tx
		case "eth_getTransactionReceipt":
			v = receipt
		case "debug_traceTransaction":
			if !ready.Load() {
				http.Error(w, "busy", 503)
				return
			}
			v = trace
		case "eth_chainId":
			v = "0x2105"
		case "eth_blockNumber":
			v = "0xb"
		case "eth_getBlockByNumber":
			v = M{"number": req.Get("params.0").Str, "hash": block, "timestamp": "0x10"}
		case "eth_call":
			to, data := req.Get("params.0.to").Str, req.Get("params.0.data").Str
			switch data[:10] {
			case "0x70a08231":
				amt := amounts[to]
				if to == silent && !strings.HasSuffix(data, walletA[2:]) && !strings.HasSuffix(data, mintRecipient[2:]) {
					amt = 0
				}
				if to == failed {
					t.Error("probed reverted frame")
				}
				v = fmt.Sprintf("0x%064x", amt)
			case "0x01ffc9a7":
				if to == nft {
					v = fmt.Sprintf("0x%064x", 1)
				} else {
					v = "0x"
				}
			case "0xdd62ed3e", "0x18160ddd":
				v = fmt.Sprintf("0x%064x", 0)
			case "0x313ce567":
				v = "0x12"
			case "0x95d89b41":
				v = "0x"
			default:
				t.Errorf("unknown eth_call %s", data)
			}
		default:
			t.Errorf("unexpected method %s", req.Raw)
		}
		json.NewEncoder(w).Encode(M{"id": 1, "jsonrpc": "2.0", "result": v})
	}))
	defer mock.Close()
	s := dbService(t, Network{RPC: mock.URL})
	// The delegated signer is present in authorizationList independently of
	// whether the optional trace endpoint is available.
	for _, a := range []string{walletA, walletB, authority, paymaster, secondAccount} {
		id := registerActive(t, s, a)
		if _, e := s.db.Exec("INSERT INTO assets(watch,token,name,amount) VALUES(?,?,?,'0')", id, known, "known"); e != nil {
			t.Fatal(e)
		}
	}
	registerActive(t, s, mintRecipient)
	event := compact(M{"id": 10, "activity": M{"network": "base", "tx_id": hash, "height": 10, "addresses": []string{walletA}, "data": M{"transaction": tx, "receipt": M{"logs": logs[:1]}}}})
	if _, e := s.db.Exec("INSERT INTO inbox(source,identity,digest,network,payload,ceiling) VALUES('chainscanner','fixture','fixture','base',?,6)", event); e != nil {
		t.Fatal(e)
	}
	if _, e := s.nextInbox(t.Context()); e == nil {
		t.Fatal("temporary trace failure was lost")
	}
	drain(t, s.nextJob)
	balances := func(a string) gjson.Result {
		return post(t, s, "/v1/balances/query", []M{{"network": "base", "address": a}}, 200).Get("results.0.balances")
	}
	if balances(walletA).Get(output).Str != "25" {
		t.Fatal("full receipt not applied during trace outage")
	}
	for _, a := range []string{walletA, walletB, authority, paymaster, secondAccount} {
		if balances(a).Get(known).Str != "18" {
			t.Fatalf("missed prior zero token for %s", a)
		}
	}
	ready.Store(true)
	if _, e := s.db.Exec("UPDATE inbox SET due=0"); e != nil {
		t.Fatal(e)
	}
	step(t, s.nextInbox)
	drain(t, s.nextProbe)
	drain(t, s.nextJob)
	if balances(walletA).Get(silent).Str != "17" || balances(walletA).Get(nft).Exists() || balances(mintRecipient).Get(silent).Str != "17" {
		t.Fatal(balances(walletA))
	}
	var snapshots, done int
	if e := s.db.QueryRow("SELECT count(*) FROM snapshots WHERE token=? AND watch=1", output).Scan(&snapshots); e != nil || snapshots != 1 {
		t.Fatal("transfer duplicated", snapshots, e)
	}
	if e := s.db.QueryRow("SELECT done FROM inbox").Scan(&done); e != nil || done != 1 {
		t.Fatal(done, e)
	}
}

func TestOwnerInventoryPaginationAndRetry(t *testing.T) {
	var second atomic.Bool
	token := addressFor(88)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := gjson.ParseBytes(b)
		var res any
		switch req.Get("method").Str {
		case "eth_chainId":
			res = "0x2105"
		case "alchemy_getTokenBalances":
			if req.Get("params.2.pageKey").Str == "" {
				res = M{"address": walletA, "tokenBalances": []M{}, "pageKey": "p2"}
			} else {
				if !second.Load() {
					http.Error(w, "retry", 503)
					return
				}
				res = M{"address": walletA, "tokenBalances": []M{{"contractAddress": token, "tokenBalance": "0x99"}}}
			}
		default:
			t.Error(req)
		}
		json.NewEncoder(w).Encode(M{"id": 1, "result": res})
	}))
	defer server.Close()
	s := dbService(t, Network{InventoryRPC: server.URL})
	registerActive(t, s, walletA)
	if _, err := s.nextInventory(t.Context(), "base"); err == nil {
		t.Fatal("partial inventory accepted")
	}
	second.Store(true)
	if _, err := s.db.Exec("UPDATE watches SET discovery_due=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.nextInventory(t.Context(), "base"); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.db.QueryRow("SELECT token FROM jobs WHERE source='reconcile'").Scan(&got); err != nil || got != token {
		t.Fatal(got, err)
	}
	if len(post(t, s, "/v1/balances/query", []M{{"network": "base", "address": walletA}}, 200).Get("results.0.balances").Map()) != 0 {
		t.Fatal("unverified index balance exposed")
	}
}

func TestRejectPartialOrWrongCanonicalReceipt(t *testing.T) {
	block, hash := "0x"+strings.Repeat("a", 64), "0x"+strings.Repeat("b", 64)
	for _, wrong := range []string{"hash", "height", "logs", "malformedLog", "fork"} {
		t.Run(wrong, func(t *testing.T) {
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				method := gjson.GetBytes(raw, "method").Str
				var res any
				switch method {
				case "eth_getTransactionByHash":
					res = M{"hash": hash, "blockNumber": "0xa", "blockHash": block, "from": walletB}
				case "eth_getTransactionReceipt":
					receipt := M{"transactionHash": hash, "blockNumber": "0xa", "blockHash": block, "logs": []M{}, "status": "0x0"}
					switch wrong {
					case "hash":
						receipt["transactionHash"] = block
					case "height":
						receipt["blockNumber"] = "0xb"
					case "logs":
						receipt["logs"] = nil
					case "malformedLog":
						receipt["logs"] = []M{{"address": walletA, "topics": []string{"0x01"}, "data": "0x"}}
					}
					res = receipt
				case "eth_getBlockByNumber":
					res = M{"number": "0xa", "hash": hash, "timestamp": "0xf"}
				default:
					t.Error(method)
				}
				json.NewEncoder(w).Encode(M{"id": 1, "result": res})
			}))
			defer mock.Close()
			s := dbService(t, Network{RPC: mock.URL})
			if _, e := s.evmEvidence(t.Context(), "base", evidence{hash: hash, height: 10}, true); e == nil {
				t.Fatal("accepted incomplete or mismatched receipt")
			}
		})
	}
}
func TestSolanaPartialNotificationFetchesClosedAndNewMints(t *testing.T) {
	// A swap closes one token account and creates another. Both mints must
	// survive even when the notification omits all token balances.
	a := splProgram
	signature := strings.Repeat("1", 64)
	tx := M{"slot": 12, "transaction": M{"signatures": []string{signature}, "message": M{"accountKeys": []string{a}}}, "meta": M{
		"preBalances": []int{10}, "postBalances": []int{9},
		"preTokenBalances":  []M{{"accountIndex": 2, "owner": a, "mint": splProgram, "uiTokenAmount": M{"amount": "15"}}},
		"postTokenBalances": []M{{"accountIndex": 3, "owner": a, "mint": token2022, "uiTokenAmount": M{"amount": "8"}}},
	}}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := gjson.ParseBytes(b)
		if req.Get("method").Str != "getTransaction" || req.Get("params.1.commitment").Str != "finalized" {
			t.Error(req)
		}
		json.NewEncoder(w).Encode(M{"id": 1, "result": tx})
	}))
	defer mock.Close()
	s := Service{cfg: Config{Networks: map[string]Network{"solana": {RPC: mock.URL}}}, http: mock.Client()}
	p := gjson.Parse(compact(M{"activity": M{"tx_id": signature, "height": 12, "data": M{"meta": M{}}}}))
	ev, e := s.evidence(t.Context(), "solana", "chainscanner", p)
	if e != nil {
		t.Fatal(e)
	}
	seen := parseSolana("solana", ev.data)[a]
	for _, tkn := range []string{"native", splProgram, token2022} {
		if !seen[tkn] {
			t.Fatal(tkn, seen)
		}
	}
}
