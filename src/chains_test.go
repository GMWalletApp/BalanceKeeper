package balancekeeper

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/block-vision/sui-go-sdk/pb/sui/rpc/v2"
	"github.com/mr-tron/base58"
	"github.com/tidwall/gjson"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"google.golang.org/grpc"
)

func TestNonEVMReads(t *testing.T) {
	const exact = "9007199254740993"
	sol := base58.Encode(make([]byte, 32))
	mint := splProgram
	apt, _ := canonical("aptos", "0x42")
	owner := "0:" + strings.Repeat("1", 64)
	master := "0:" + strings.Repeat("2", 64)
	wallet := "0:" + strings.Repeat("3", 64)
	boc := func(a string) any {
		addr, _ := tonAddress(a)
		b := cell.BeginCell().MustStoreAddr(addr).EndCell().ToBOC()
		return []any{"slice", M{"bytes": base64.StdEncoding.EncodeToString(b)}}
	}
	var wrongOwner bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := gjson.ParseBytes(b)
		var reply any
		switch r.URL.Path {
		case "/solana":
			var result any
			switch req.Get("method").Str {
			case "getGenesisHash":
				result = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
			case "getSlot":
				result = 90
			case "getBlockTime":
				result = 1000
			case "getTokenAccountsByOwner":
				if req.Get("params.2.minContextSlot").Int() != 90 {
					t.Error("missing minContextSlot")
				}
				var accounts []M
				for _, amount := range []string{exact, "2"} {
					accounts = append(accounts, M{"account": M{"data": M{"parsed": M{"info": M{"owner": sol, "mint": mint, "tokenAmount": M{"amount": amount, "decimals": 6}}}}}})
				}
				result = M{"context": M{"slot": 91}, "value": accounts}
			default:
				t.Errorf("unexpected method %s", req.Raw)
			}
			reply = M{"id": 1, "jsonrpc": "2.0", "result": result}
		case "/v1":
			reply = M{"chain_id": 1, "ledger_version": "101"}
		case "/v1/accounts/" + apt + "/balance/0x1::aptos_coin::AptosCoin":
			if r.URL.Query().Get("ledger_version") != "101" {
				t.Error("missing ledger version")
			}
			reply = json.Number(exact)
		case "/wallet/getblockbynum":
			reply = M{"blockID": tronGenesis}
		case "/walletsolidity/getnowblock":
			reply = M{"block_header": M{"raw_data": M{"number": 112}}}
		case "/walletsolidity/getaccount":
			reply = M{"balance": json.Number(exact)}
		case "/api/v2/getMasterchainInfo":
			reply = M{"result": M{"init": M{"root_hash": "F6OpKZKqvqeFp6CQmFomXNMfMj2EnaUSOXN+Mh+wVWk="}, "last": M{"seqno": 90}}}
		case "/api/v2/getAddressInformation":
			reply = M{"ok": true, "result": M{"state": "active"}}
		case "/api/v2/runGetMethod":
			stack := []any{boc(wallet)}
			if req.Get("method").Str == "get_wallet_data" {
				a := owner
				if wrongOwner {
					a = wallet
				}
				stack = []any{[]any{"num", "0x9"}, boc(a), boc(master)}
			}
			reply = M{"ok": true, "result": M{"exit_code": 0, "stack": stack}}
		default:
			t.Errorf("unexpected path %s", r.URL)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(reply)
	}))
	defer srv.Close()
	s := Service{cfg: Config{Networks: map[string]Network{"solana": {RPC: srv.URL + "/solana"}, "aptos": {RPC: srv.URL}, "tron": {RPC: srv.URL}, "ton": {RPC: srv.URL}}}, http: srv.Client()}
	for _, tc := range []struct{ n, a, t, want string }{{"solana", sol, mint, "9007199254740995"}, {"aptos", apt, "native", exact}, {"tron", tron58(make([]byte, 20)), "native", exact}, {"ton", owner, master, "9"}} {
		r, err := s.balance(t.Context(), tc.n, tc.a, tc.t, 90)
		if err != nil || r.Amount != tc.want {
			t.Fatalf("%s: %+v %v", tc.n, r, err)
		}
		if tc.n == "ton" && r.Derived != wallet {
			t.Fatal("derived wallet mismatch")
		}
	}
	wrongOwner = true
	if _, e := s.balance(t.Context(), "ton", owner, master, 90); e == nil {
		t.Fatal("accepted another owner's jetton balance")
	}
	if _, e := s.balance(t.Context(), "aptos", apt, "native", 102); e == nil {
		t.Fatal("accepted lagging ledger")
	}
}

type suiTestLedger struct {
	pb.UnimplementedLedgerServiceServer
}

func (suiTestLedger) GetServiceInfo(context.Context, *pb.GetServiceInfoRequest) (*pb.GetServiceInfoResponse, error) {
	return &pb.GetServiceInfoResponse{ChainId: ptr("4btiuiMPvEENsttpZC7CZ53DruC3MAgfznDbASZ7DR6S"), CheckpointHeight: ptr(uint64(100))}, nil
}

type suiTestState struct {
	pb.UnimplementedStateServiceServer
}

func (suiTestState) GetBalance(_ context.Context, r *pb.GetBalanceRequest) (*pb.GetBalanceResponse, error) {
	if r.GetCoinType() != "0x2::sui::SUI" {
		return nil, fmt.Errorf("unexpected coin type")
	}
	return &pb.GetBalanceResponse{Balance: &pb.Balance{Balance: ptr(uint64(9007199254740993))}}, nil
}

func TestSuiRead(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := grpc.NewServer()
	pb.RegisterLedgerServiceServer(server, suiTestLedger{})
	pb.RegisterStateServiceServer(server, suiTestState{})
	go server.Serve(l)
	defer server.Stop()
	s := Service{cfg: Config{Networks: map[string]Network{"sui": {RPC: "http://" + l.Addr().String()}}}}
	a, _ := canonical("sui", "0x2")
	r, e := s.balance(t.Context(), "sui", a, "native", 100)
	if e != nil || r.Amount != "9007199254740993" || r.Height != nil {
		t.Fatal(r, e)
	}
}
func TestMoveTokens(t *testing.T) {
	for input, want := range map[string]string{"0x02::sui::SUI": "native", "0x0a::pool::LP<0x001::mod::T, vector<u128>>": "0xa::pool::LP<0x1::mod::T,vector<u128>>"} {
		got, e := tokenID("sui", input)
		if e != nil || got != want {
			t.Fatal(input, got, e)
		}
	}
	for _, input := range []string{"0x2::pool::LP<>", "0x2::pool::LP<vector<u8,u8>>", "0x2::pool::LP<garbage>"} {
		if _, e := tokenID("sui", input); e == nil {
			t.Fatal(input)
		}
	}
}
