package balancekeeper

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const btcWallet = "1111111111111111111114oLvT2"

func TestBitcoinWalletLifecycle(t *testing.T) {
	ctx := t.Context()
	block, id := strings.Repeat("b", 64), strings.Repeat("a", 64)
	script := "76a914" + strings.Repeat("00", 20) + "88ac"
	var spent atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/block-height/0":
			fmt.Fprint(w, bitcoinGenesis)
			return
		case "/blocks/tip/hash", "/block-height/100":
			fmt.Fprint(w, block)
			return
		case "/block/" + block:
			data = M{"id": block, "timestamp": 1700000000, "height": 100}
		case "/tx/" + id:
			data = M{"txid": id, "status": M{"confirmed": true, "block_hash": block, "block_height": 100}, "vin": []M{{"prevout": M{"scriptpubkey": script, "value": 200}}}, "vout": []M{{"scriptpubkey": "6a", "value": 0}}}
		case "/address/" + btcWallet:
			debits := 0
			if spent.Load() {
				debits = 200
			}
			data = M{"address": btcWallet, "chain_stats": M{"funded_txo_sum": 200, "spent_txo_sum": debits}, "mempool_stats": M{"funded_txo_sum": 999}}
		case "/v1/subscriptions":
			data = M{"registrations": []M{{"network": "bitcoin", "address": btcWallet, "from_height": 99}}}
		case "/v1/events/next":
			data = M{"id": 1, "activity": M{"network": "bitcoin", "height": 100, "tx_id": id, "addresses": []string{btcWallet}, "data": M{"block_hash": block}}}
		case "/v1/events/1/ack":
			w.WriteHeader(204)
			return
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(data)
	}))
	defer srv.Close()
	s, e := openService(Config{Token: "test", BroadcastSecret: "fixture", Database: filepath.Join(t.TempDir(), "btc.db"), Scanner: srv.URL, Networks: map[string]Network{"bitcoin": {RPC: srv.URL}}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.db.Close()
	batch := []M{{"network": "bitcoin", "address": btcWallet}}
	post(t, s, "/v1/watches", []M{{"network": "bitcoin", "address": btcWallet, "assets": []M{{"name": "BTC", "contract_address": nil}}}}, 202)
	if e = s.syncWatches(ctx); e != nil {
		t.Fatal(e)
	}
	step(t, s.nextJob)
	balance := func() string {
		return post(t, s, "/v1/balances/query", batch, 200).Get("results.0.balances.native").Str
	}
	if balance() != "200" {
		t.Fatal("incorrect confirmed balance")
	}
	spent.Store(true)
	// The broadcaster supplies just the txid. The spending wallet occurs only in a prevout.
	post(t, s, "/v1/notifications/broadcasts", []M{{"schemaVersion": 1, "eventId": "btc", "event": "broadcast.accepted", "family": "bitcoin", "network": "bitcoin", "hash": id}}, 204)
	step(t, s.nextInbox)
	step(t, s.nextJob)
	if balance() != "" {
		t.Fatal("spent balance still present")
	}
	if e = s.poll(ctx, "bitcoin"); e != nil {
		t.Fatal(e)
	}
	step(t, s.nextInbox)
	drain(t, s.nextJob)
	rows := post(t, s, "/v1/snapshots/query", batch, 200).Get("results.0.items")
	if len(rows.Array()) != 2 || rows.Get("0.snapshot.amount_raw").Str != "0" {
		t.Fatal(rows)
	}
	if _, e = tokenID("bitcoin", btcWallet); e == nil {
		t.Fatal("contract accepted")
	}
}

func TestBitcoinHistoricalBalancePages(t *testing.T) {
	tip, target := strings.Repeat("f", 64), strings.Repeat("a", 64)
	script := "76a914" + strings.Repeat("00", 20) + "88ac"
	makeTx := func(i int) M {
		return M{"txid": fmt.Sprintf("%064x", i), "status": M{"confirmed": true, "block_height": 11}, "vin": []M{{"prevout": M{"scriptpubkey": script, "value": 2}}}, "vout": []M{{"scriptpubkey": script, "value": 1}}}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blocks/tip/hash":
			fmt.Fprint(w, tip)
		case "/block-height/10":
			fmt.Fprint(w, target)
		case "/address/" + btcWallet:
			json.NewEncoder(w).Encode(M{"address": btcWallet, "chain_stats": M{"funded_txo_sum": 99, "spent_txo_sum": 99}})
		case "/address/" + btcWallet + "/txs/chain":
			var txs []M
			for i := 1; i <= 25; i++ {
				txs = append(txs, makeTx(i))
			}
			json.NewEncoder(w).Encode(txs)
		case "/address/" + btcWallet + "/txs/chain/" + fmt.Sprintf("%064x", 25):
			json.NewEncoder(w).Encode([]M{makeTx(26)})
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer srv.Close()
	s := Service{http: srv.Client(), cfg: Config{Networks: map[string]Network{"bitcoin": {RPC: srv.URL, Confirmations: 1}}}}
	r, e := s.bitcoinBalance(t.Context(), btcWallet, "native", point{Height: 10, Block: target})
	if e != nil || r.Amount != "26" || *r.Decimals != 8 {
		t.Fatal(r, e)
	}
}
