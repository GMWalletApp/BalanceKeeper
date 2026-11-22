package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

const walletA = "0x1111111111111111111111111111111111111111"
const walletB = "0x2222222222222222222222222222222222222222"
const testToken = "0x3333333333333333333333333333333333333333"

func post(t *testing.T, s *Service, path string, body any, want int) gjson.Result {
	t.Helper()
	raw := compact(body)
	r := httptest.NewRequest("POST", path, strings.NewReader(raw))
	if path == "/v1/notifications/broadcasts" {
		mac := hmac.New(sha256.New, []byte(s.cfg.BroadcastSecret))
		mac.Write([]byte(raw))
		r.Header.Set("X-Broadcaster-Signature", hex.EncodeToString(mac.Sum(nil)))
	} else {
		r.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s status %d: %s", path, w.Code, w.Body.String())
	}
	return gjson.Parse(w.Body.String())
}
func step(t *testing.T, fn func(context.Context) (bool, error)) {
	t.Helper()
	worked, e := fn(t.Context())
	if e != nil || !worked {
		t.Fatalf("worker step: %t %v", worked, e)
	}
}
func TestWalletLifecycle(t *testing.T) {
	ctx := t.Context()
	block := "0x" + strings.Repeat("b", 64)
	hash := "0x" + strings.Repeat("a", 64)
	transfer := M{"address": testToken, "topics": []string{"0x" + transferTopic, "0x" + strings.Repeat("0", 24) + walletB[2:], "0x" + strings.Repeat("0", 24) + walletA[2:]}, "data": "0x" + strings.Repeat("0", 63) + "1"}
	tx := M{"hash": hash, "from": walletB, "to": testToken, "value": "0x0", "blockNumber": "0x64", "blockHash": block}
	receipt := M{"transactionHash": hash, "blockHash": block, "blockNumber": "0x64", "logs": []any{transfer}, "status": "0x1"}
	data := M{"transaction": tx, "receipt": receipt}
	var amount, acks atomic.Int64
	amount.Store(100)
	var confirmed atomic.Bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		in := gjson.ParseBytes(b)
		var reply any
		switch r.URL.Path {
		case "/v1/subscriptions":
			var regs []M
			for _, target := range in.Array() {
				regs = append(regs, M{"network": target.Get("network").Str, "address": target.Get("address").Str, "from_height": 99})
			}
			reply = M{"registrations": regs}
		case "/v1/transactions/base/" + hash:
			v := data
			if !confirmed.Load() {
				v = M{"transaction": tx, "receipt": nil}
			}
			reply = M{"network": "base", "hash": hash, "result": v}
		case "/v1/events/next":
			reply = M{"id": 17, "activity": M{"network": "base", "height": 100, "addresses": []string{walletA}, "tx_id": hash, "data": data}}
		case "/v1/events/17/ack":
			acks.Add(1)
			w.WriteHeader(204)
			return
		case "/rpc":
			var result any
			switch in.Get("method").Str {
			case "eth_chainId":
				result = "0x2105"
			case "eth_blockNumber":
				result = "0x68"
			case "eth_getBlockByNumber":
				result = M{"hash": block, "number": in.Get("params.0").Str, "timestamp": "0x67300000"}
			case "eth_getBalance":
				result = "0x0"
			case "eth_call":
				num := amount.Load()
				if in.Get("params.0.data").Str == "0x313ce567" {
					num = 6
				}
				result = fmt.Sprintf("0x%064x", num)
			default:
				t.Errorf("unexpected RPC %s", in.Raw)
			}
			reply = M{"id": 1, "jsonrpc": "2.0", "result": result}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(reply)
	}))
	defer mock.Close()
	cfg := Config{Token: "test", BroadcastSecret: "private fixture key", Database: filepath.Join(t.TempDir(), "bk.db"), Scanner: mock.URL, Networks: map[string]Network{"base": {RPC: mock.URL + "/rpc"}}}
	s, e := openService(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.db.Close() }()
	target := M{"network": "base", "address": walletA}
	batch := []M{target}
	init := []M{{"network": "base", "address": walletA, "assets": []M{{"name": "ETH", "contract_address": nil}}}}
	post(t, s, "/v1/watches", init, 202)
	post(t, s, "/v1/watches", init, 200)
	if worked, e := s.nextJob(ctx); e != nil || worked {
		t.Fatal("premature initialization", e)
	}
	if e = s.syncWatches(ctx); e != nil {
		t.Fatal(e)
	}
	step(t, s.nextJob)
	initial := post(t, s, "/v1/snapshots/query", batch, 200).Get("results.0.items.0.snapshot")
	if initial.Get("amount_raw").Str != "0" || initial.Get("source").Str != "initial" || initial.Get("block_time").Str == "" {
		t.Fatal(initial)
	}
	event := M{"schemaVersion": 1, "eventId": "accepted", "family": "evm", "network": "8453", "event": "broadcast.accepted", "hash": hash}
	path := "/v1/notifications/broadcasts"
	unsigned := httptest.NewRequest("POST", path, strings.NewReader(compact([]M{event})))
	denied := httptest.NewRecorder()
	s.Handler().ServeHTTP(denied, unsigned)
	if denied.Code != 401 {
		t.Fatal("unsigned accepted")
	}
	post(t, s, path, []M{event}, 204)
	post(t, s, path, []M{event}, 204)
	if _, e := s.nextInbox(ctx); e == nil {
		t.Fatal("pending tx should retry")
	}
	if v := post(t, s, "/v1/balances/query", batch, 200).Get("results.0.balances"); len(v.Map()) != 0 {
		t.Fatal("pending created balance", v)
	}
	confirmed.Store(true)
	included := M{"schemaVersion": 1, "eventId": "included", "family": "evm", "network": "8453", "event": "broadcast.included", "hash": hash, "txHash": hash}
	post(t, s, path, []M{included}, 204)
	step(t, s.nextInbox)
	step(t, s.nextJob)
	if e = s.poll(ctx, "base"); e != nil {
		t.Fatal(e)
	}
	if acks.Load() != 1 {
		t.Fatal("missing ACK")
	}
	// Resume a durable delivery after ACK without losing its work.
	s.db.Close()
	s, e = openService(cfg)
	if e != nil {
		t.Fatal(e)
	}
	step(t, s.nextInbox)
	step(t, s.nextJob)
	snapshots := func() gjson.Result { return post(t, s, "/v1/snapshots/query", batch, 200).Get("results.0.items") }
	if rows := snapshots(); len(rows.Array()) != 2 || rows.Get("0.snapshot.source").Str != "broadcaster" {
		t.Fatal(rows)
	}
	if v := post(t, s, "/v1/balances/query", batch, 200).Get("results.0.balances." + testToken).Str; v != "100" {
		t.Fatal(v)
	}
	amount.Store(0)
	sel := M{"network": "base", "address": walletA, "tokens": []string{testToken}}
	post(t, s, "/v1/refreshes", []M{target, sel}, 200)
	if len(snapshots().Array()) != 3 {
		t.Fatal("overlapping refresh duplicated observation")
	}
	if v := post(t, s, "/v1/balances/query", []M{sel}, 200).Get("results.0.balances." + testToken).Str; v != "0" {
		t.Fatal(v)
	}
	if v := post(t, s, "/v1/balances/query", batch, 200).Get("results.0.balances"); len(v.Map()) != 0 {
		t.Fatal("zero holding", v)
	}
	amount.Store(5)
	result := post(t, s, "/v1/refreshes", []M{sel, target}, 200)
	if len(result.Get("results.1.balances").Map()) != 0 {
		t.Fatal("whole-address refresh expanded after admission", result)
	}
	hist := post(t, s, "/v1/snapshots/query", []M{{"network": "base", "address": walletA, "limit": 1, "tokens": []string{testToken}}}, 200).Get("results.0")
	if hist.Get("items.0.snapshot.amount_raw").Str != "5" || hist.Get("next_before").Str == "" {
		t.Fatal(hist)
	}
	from := hist.Get("items.0.snapshot.observed_at").Str
	for _, pair := range []struct {
		k    string
		want int
	}{{"from", 1}, {"to", 3}} {
		rows := post(t, s, "/v1/snapshots/query", []M{{"network": "base", "address": walletA, pair.k: from}}, 200).Get("results.0.items").Array()
		if len(rows) != pair.want {
			t.Fatal(pair, len(rows))
		}
	}
	invalid := []M{{"network": "base", "address": walletB, "assets": []M{}}, {"network": "base", "address": walletA, "assets": []M{}}}
	if idx := post(t, s, "/v1/watches", invalid, 409).Get("error.index").Int(); idx != 1 {
		t.Fatal(idx)
	}
	if v := post(t, s, "/v1/watches/status", []M{{"network": "base", "address": walletB}}, 200).Get("results.0.error.code").Str; v != "watch_not_found" {
		t.Fatal("partial commit", v)
	}
	post(t, s, "/v1/watches/delete", batch, 200)
	post(t, s, "/v1/watches", init, 202)
	if rows := snapshots(); len(rows.Array()) != 0 {
		t.Fatal("old history recovered", rows)
	}
	if _, e = s.db.Exec("UPDATE inbox SET due=0"); e != nil {
		t.Fatal(e)
	}
	step(t, s.nextInbox)
	if err := s.syncWatches(ctx); err != nil {
		t.Fatal(err)
	}
	if state := post(t, s, "/v1/watches/status", batch, 200).Get("results.0.subscription").Str; state != "pending" {
		t.Fatal("old interval reused", state)
	}
	if err := s.syncWatches(ctx); err != nil {
		t.Fatal(err)
	}
	if state := post(t, s, "/v1/watches/status", batch, 200).Get("results.0.subscription").Str; state != "active" {
		t.Fatal(state)
	}
	var jobs int
	if e = s.db.QueryRow("SELECT count(*) FROM jobs").Scan(&jobs); e != nil || jobs != 1 {
		t.Fatal(jobs, e)
	}
}
