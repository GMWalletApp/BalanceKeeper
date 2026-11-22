package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mr-tron/base58"
	"github.com/tidwall/gjson"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

var chainIDs = map[string]int64{"ethereum": 1, "arbitrum": 42161, "binance": 56, "avalanche": 43114, "polygon": 137, "optimism": 10, "base": 8453, "hyperevm": 999, "monad": 143, "arc": 5042, "plasma": 9745, "robinhood": 4663}

func supported(n string) bool {
	return chainIDs[n] != 0 || n == "solana" || n == "sui" || n == "ton" || n == "tron" || n == "aptos"
}

var nativeSymbols = map[string]string{"ethereum": "ETH", "arbitrum": "ETH", "optimism": "ETH", "base": "ETH", "robinhood": "ETH", "hyperevm": "HYPE", "monad": "MON", "arc": "USDC", "binance": "BNB", "polygon": "POL", "avalanche": "AVAX", "plasma": "XPL", "tron": "TRX", "aptos": "APT", "solana": "SOL", "sui": "SUI", "ton": "TON"}

func abiSymbol(raw string) string {
	if len(raw) > 2048 {
		return ""
	}
	b, err := hex.DecodeString(strings.TrimPrefix(raw, "0x"))
	if err != nil || len(b) < 32 {
		return ""
	}
	if len(b) == 32 {
		b = bytes.TrimRight(b, "\x00")
	} else {
		if len(b) < 64 || new(big.Int).SetBytes(b[:32]).Cmp(big.NewInt(32)) != 0 {
			return ""
		}
		length := new(big.Int).SetBytes(b[32:64])
		if !length.IsUint64() || length.Uint64() > uint64(len(b)-64) {
			return ""
		}
		b = b[64 : 64+int(length.Uint64())]
	}
	if len(b) > 128 || !utf8.Valid(b) || strings.IndexFunc(string(b), unicode.IsControl) >= 0 {
		return ""
	}
	return strings.TrimSpace(string(b))
}

var invalidEvidence = errors.New("incomplete or inconsistent chain response")

func tron58(raw []byte) string {
	p := append([]byte{65}, raw...)
	a := sha256.Sum256(p)
	b := sha256.Sum256(a[:])
	return base58.Encode(append(p, b[:4]...))
}
func tonAddress(a string) (*address.Address, error) {
	if strings.Contains(a, ":") {
		return address.ParseRawAddr(a)
	}
	return address.ParseAddr(a)
}
func canonical(n, a string) (string, error) {
	if len(a) > 256 || a == "" {
		return "", errors.New("invalid address")
	}
	h := strings.TrimPrefix(a, "0x")
	switch {
	case chainIDs[n] != 0:
		if b, e := hex.DecodeString(h); e == nil && len(b) == 20 {
			return "0x" + strings.ToLower(h), nil
		}
	case n == "aptos" || n == "sui":
		if len(h) > 0 && len(h) <= 64 {
			h = strings.Repeat("0", 64-len(h)) + h
			if _, e := hex.DecodeString(h); e == nil {
				return "0x" + strings.ToLower(h), nil
			}
		}
	case n == "solana":
		if b, e := base58.Decode(a); e == nil && len(b) == 32 {
			return a, nil
		}
	case n == "tron":
		if len(h) == 42 && strings.HasPrefix(h, "41") {
			h = h[2:]
		}
		if b, e := hex.DecodeString(h); e == nil && len(b) == 20 {
			return tron58(b), nil
		}
		if b, e := base58.Decode(a); e == nil && len(b) == 25 && b[0] == 65 && tron58(b[1:21]) == a {
			return a, nil
		}
	case n == "ton":
		if t, e := tonAddress(a); e == nil && t.Type() == address.StdAddress && !t.IsTestnetOnly() && len(t.Data()) == 32 {
			return t.StringRaw(), nil
		}
	}
	return "", errors.New("invalid address")
}

var moveStruct = regexp.MustCompile(`^0x([0-9a-fA-F]{1,64})(::[A-Za-z_][A-Za-z_0-9]*::[A-Za-z_][A-Za-z_0-9]*)`)
var movePrimitive = regexp.MustCompile(`^(bool|address|signer|u256|u128|u64|u32|u16|u8)`)

func moveType(input string, depth int) (value, rest string, ok bool) {
	if depth > 32 || len(input) > 4096 {
		return
	}
	input = strings.TrimSpace(input)
	if m := moveStruct.FindStringSubmatch(input); m != nil {
		h := strings.TrimLeft(strings.ToLower(m[1]), "0")
		if h == "" {
			h = "0"
		}
		value = "0x" + h + m[2]
		rest = strings.TrimSpace(input[len(m[0]):])
		if !strings.HasPrefix(rest, "<") {
			return value, rest, true
		}
	} else if depth > 0 && strings.HasPrefix(input, "vector<") {
		value = "vector"
		rest = input[6:]
	} else if depth > 0 {
		v := movePrimitive.FindString(input)
		return v, strings.TrimSpace(input[len(v):]), v != ""
	} else {
		return
	}
	var args []string
	rest = rest[1:]
	for {
		var arg string
		arg, rest, ok = moveType(rest, depth+1)
		if !ok || len(rest) == 0 {
			return "", "", false
		}
		args = append(args, arg)
		switch rest[0] {
		case '>':
			return value + "<" + strings.Join(args, ",") + ">", strings.TrimSpace(rest[1:]), true
		case ',':
			if value == "vector" {
				return "", "", false
			}
			rest = rest[1:]
		default:
			return "", "", false
		}
	}
}

func tokenID(n, t string) (string, error) {
	if t == "native" {
		return t, nil
	}
	if (n == "aptos" || n == "sui") && strings.Contains(t, "::") {
		typ, rest, valid := moveType(t, 0)
		if !valid || rest != "" {
			return "", errors.New("invalid Move type")
		}
		t = typ
		if n == "sui" && t == "0x2::sui::SUI" || n == "aptos" && t == "0x1::aptos_coin::AptosCoin" {
			return "native", nil
		}
		return t, nil
	}
	if n == "sui" {
		return "", errors.New("coin type required")
	}
	a, e := canonical(n, t)
	if n == "aptos" && strings.TrimLeft(strings.TrimPrefix(a, "0x"), "0") == "a" {
		return "native", nil
	}
	return a, e
}

type point struct {
	Height int64
	Block  string
	Time   *string
}
type reading struct {
	Name     string
	Amount   string
	Decimals *int
	Height   *int64
	Time     *string
	Derived  string
}

func ptr[T any](v T) *T { return &v }
func integer(v gjson.Result, base int) (*big.Int, error) {
	if v.Type != gjson.String && v.Type != gjson.Number {
		return nil, invalidEvidence
	}
	raw := v.String()
	if v.Type == gjson.Number {
		raw = v.Raw
	}
	if base == 16 {
		raw = strings.TrimPrefix(raw, "0x")
	}
	if len(raw) > 100 || len(raw) == 0 || strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "-") {
		return nil, invalidEvidence
	}
	num, ok := new(big.Int).SetString(raw, base)
	if !ok || num.Sign() < 0 {
		return nil, invalidEvidence
	}
	return num, nil
}
func number(v gjson.Result, base int) (int64, error) {
	x, e := integer(v, base)
	if e != nil || !x.IsInt64() {
		return 0, invalidEvidence
	}
	return x.Int64(), nil
}
func (s *Service) rpc(ctx context.Context, n, method string, params ...any) (gjson.Result, error) {
	cfg := s.cfg.Networks[n]
	return s.rpcAt(ctx, cfg.RPC, cfg.Bearer, method, params...)
}
func (s *Service) rpcAt(ctx context.Context, endpoint, bearer, method string, params ...any) (gjson.Result, error) {
	if params == nil {
		params = []any{}
	}
	r, _, e := s.request(ctx, "POST", endpoint, bearer, M{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if e != nil {
		return r, e
	}
	if r.Get("error").Exists() || r.Get("id").Int() != 1 || r.Get("result").Raw == "" {
		return r, fmt.Errorf("%s RPC failed", method)
	}
	return r.Get("result"), nil
}
func (s *Service) rest(ctx context.Context, n, path string, body any) (gjson.Result, error) {
	cfg := s.cfg.Networks[n]
	method := "GET"
	if body != nil {
		method = "POST"
	}
	r, _, e := s.request(ctx, method, strings.TrimRight(cfg.RPC, "/")+path, cfg.Bearer, body)
	return r, e
}
func evmHeight(raw int64) string { return fmt.Sprintf("0x%x", raw) }
func (s *Service) evmBlock(ctx context.Context, n string, h int64) (point, error) {
	r, e := s.rpc(ctx, n, "eth_getBlockByNumber", evmHeight(h), false)
	height, he := number(r.Get("number"), 16)
	ts, te := number(r.Get("timestamp"), 16)
	if e != nil || he != nil || te != nil || height != h || len(r.Get("hash").String()) != 66 {
		return point{}, invalidEvidence
	}
	return point{h, r.Get("hash").String(), ptr(iso(time.Unix(ts, 0)))}, nil
}
func (s *Service) head(ctx context.Context, n string) (point, error) {
	var r gjson.Result
	var e error
	var h int64
	switch {
	case chainIDs[n] != 0:
		r, e = s.rpc(ctx, n, "eth_chainId")
		v, ve := number(r, 16)
		if e != nil || ve != nil || v != chainIDs[n] {
			return point{}, invalidEvidence
		}
		r, e = s.rpc(ctx, n, "eth_blockNumber")
		if e == nil {
			h, e = number(r, 16)
		}
		if e != nil {
			return point{}, e
		}
		return s.evmBlock(ctx, n, max(0, h-s.cfg.Networks[n].Confirmations))
	case n == "solana":
		r, e = s.rpc(ctx, n, "getGenesisHash")
		if e != nil || r.String() != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
			return point{}, invalidEvidence
		}
		r, e = s.rpc(ctx, n, "getSlot", M{"commitment": "finalized"})
	case n == "aptos":
		r, e = s.rest(ctx, n, "/v1", nil)
		if r.Get("chain_id").Int() != 1 {
			return point{}, invalidEvidence
		}
		r = r.Get("ledger_version")
	case n == "tron":
		g, err := s.rest(ctx, n, "/wallet/getblockbynum", M{"num": 0})
		if err != nil || g.Get("blockID").String() != tronGenesis {
			return point{}, invalidEvidence
		}
		r, e = s.rest(ctx, n, "/walletsolidity/getnowblock", M{})
		r = r.Get("block_header.raw_data.number")
	case n == "sui":
		return s.suiHead(ctx)
	case n == "ton":
		r, e = s.rest(ctx, n, "/api/v2/getMasterchainInfo", nil)
		if r.Get("result.init.root_hash").String() != "F6OpKZKqvqeFp6CQmFomXNMfMj2EnaUSOXN+Mh+wVWk=" {
			return point{}, invalidEvidence
		}
		r = r.Get("result.last.seqno")
	}
	if e != nil {
		return point{}, e
	}
	h, e = number(r, 10)
	return point{Height: h}, e
}
func (s *Service) balance(ctx context.Context, n, a, t string, minHeight int64) (reading, error) {
	h, e := s.head(ctx, n)
	if e != nil {
		return reading{}, e
	}
	if h.Height < minHeight {
		return reading{}, errors.New("RPC behind required height")
	}
	switch {
	case chainIDs[n] != 0:
		at := evmHeight(h.Height)
		var v gjson.Result
		var decimals *int
		var name string
		if t == "native" {
			v, e = s.rpc(ctx, n, "eth_getBalance", a, at)
			decimals = ptr(18)
		} else {
			v, e = s.rpc(ctx, n, "eth_call", M{"to": t, "data": "0x70a08231" + strings.Repeat("0", 24) + a[2:]}, at)
			if len(v.String()) != 66 {
				return reading{}, invalidEvidence
			}
			sym, err := s.rpc(ctx, n, "eth_call", M{"to": t, "data": "0x95d89b41"}, at)
			if err == nil {
				name = abiSymbol(sym.Str)
			}
			d, err := s.rpc(ctx, n, "eth_call", M{"to": t, "data": "0x313ce567"}, at)
			if num, ne := number(d, 16); err == nil && ne == nil && num <= 255 {
				decimals = ptr(int(num))
			}
		}
		x, ne := integer(v, 16)
		if e != nil || ne != nil {
			return reading{}, invalidEvidence
		}
		end, e := s.evmBlock(ctx, n, h.Height)
		if e != nil || end.Block != h.Block {
			return reading{}, invalidEvidence
		}
		return reading{Name: name, Amount: x.String(), Height: &h.Height, Time: h.Time, Decimals: decimals}, nil
	case n == "solana":
		opt := M{"commitment": "finalized", "minContextSlot": h.Height}
		var v gjson.Result
		sum := new(big.Int)
		var dec *int
		if t == "native" {
			v, e = s.rpc(ctx, n, "getBalance", a, opt)
			if e == nil {
				sum, e = integer(v.Get("value"), 10)
			}
			dec = ptr(9)
		} else {
			opt["encoding"] = "jsonParsed"
			v, e = s.rpc(ctx, n, "getTokenAccountsByOwner", a, M{"mint": t}, opt)
			if !v.Get("value").IsArray() {
				return reading{}, invalidEvidence
			}
			for _, row := range v.Get("value").Array() {
				info := row.Get("account.data.parsed.info")
				if info.Get("owner").String() != a || info.Get("mint").String() != t {
					return reading{}, invalidEvidence
				}
				x, err := integer(info.Get("tokenAmount.amount"), 10)
				if err != nil {
					return reading{}, err
				}
				sum.Add(sum, x)
				d, err := number(info.Get("tokenAmount.decimals"), 10)
				if err == nil && d <= 255 {
					dec = ptr(int(d))
				}
			}
		}
		slot, se := number(v.Get("context.slot"), 10)
		if e != nil || se != nil || slot < h.Height {
			return reading{}, invalidEvidence
		}
		var stamp *string
		if b, err := s.rpc(ctx, n, "getBlockTime", slot); err == nil {
			if sec, err := number(b, 10); err == nil {
				stamp = ptr(iso(time.Unix(sec, 0)))
			}
		}
		return reading{Amount: sum.String(), Decimals: dec, Height: &slot, Time: stamp}, nil
	case n == "tron":
		var x *big.Int
		var dec *int
		if t == "native" {
			v, err := s.rest(ctx, n, "/walletsolidity/getaccount", M{"address": a, "visible": true})
			if err != nil || !v.IsObject() || v.Get("Error").Exists() || v.Get("code").Exists() {
				return reading{}, invalidEvidence
			}
			x = new(big.Int)
			if v.Get("balance").Exists() {
				x, e = integer(v.Get("balance"), 10)
			}
			dec = ptr(6)
		} else {
			raw, _ := base58.Decode(a)
			v, err := s.rest(ctx, n, "/walletsolidity/triggerconstantcontract", M{"owner_address": a, "contract_address": t, "function_selector": "balanceOf(address)", "parameter": strings.Repeat("0", 24) + hex.EncodeToString(raw[1:21]), "visible": true})
			if err != nil || !v.Get("result.result").Bool() || len(v.Get("constant_result.0").String()) != 64 {
				return reading{}, invalidEvidence
			}
			x, e = integer(v.Get("constant_result.0"), 16)
		}
		if e != nil {
			return reading{}, e
		}
		return reading{Amount: x.String(), Decimals: dec}, nil
	case n == "aptos":
		typ := t
		var dec *int
		if t == "native" {
			typ = "0x1::aptos_coin::AptosCoin"
			dec = ptr(8)
		}
		v, e := s.rest(ctx, n, "/v1/accounts/"+a+"/balance/"+url.PathEscape(typ)+"?ledger_version="+strconv.FormatInt(h.Height, 10), nil)
		if e != nil {
			return reading{}, e
		}
		x, e := integer(v, 10)
		if e != nil {
			return reading{}, e
		}
		return reading{Amount: x.String(), Decimals: dec, Height: &h.Height}, nil
	case n == "sui":
		return s.suiBalance(ctx, a, t)
	case n == "ton":
		return s.tonBalance(ctx, a, t)
	}
	return reading{}, invalidEvidence
}
func stackAddr(v gjson.Result) (string, error) {
	b, e := base64.StdEncoding.DecodeString(v.Get("1.bytes").String())
	if e != nil {
		return "", invalidEvidence
	}
	c, e := cell.FromBOC(b)
	if e != nil {
		return "", e
	}
	sl, e := c.BeginParse()
	if e != nil {
		return "", e
	}
	a, e := sl.LoadAddr()
	if e != nil || a == nil {
		return "", invalidEvidence
	}
	return a.StringRaw(), nil
}
func (s *Service) tonGetter(ctx context.Context, a, method string, stack []any) (gjson.Result, error) {
	v, e := s.rest(ctx, "ton", "/api/v2/runGetMethod", M{"address": a, "method": method, "stack": stack})
	if e != nil || !v.Get("ok").Bool() || !v.Get("result.exit_code").Exists() || v.Get("result.exit_code").Int() != 0 || !v.Get("result.stack").IsArray() {
		return v, invalidEvidence
	}
	return v.Get("result.stack"), nil
}
func (s *Service) tonBalance(ctx context.Context, a, t string) (reading, error) {
	if t == "native" {
		v, e := s.rest(ctx, "ton", "/api/v2/getAddressBalance?address="+url.QueryEscape(a), nil)
		if e != nil || !v.Get("ok").Bool() {
			return reading{}, invalidEvidence
		}
		x, e := integer(v.Get("result"), 10)
		if e != nil {
			return reading{}, e
		}
		return reading{Amount: x.String(), Decimals: ptr(9)}, nil
	}
	owner, e := tonAddress(a)
	if e != nil {
		return reading{}, e
	}
	boc := cell.BeginCell().MustStoreAddr(owner).EndCell().ToBOC()
	v, e := s.tonGetter(ctx, t, "get_wallet_address", []any{[]any{"tvm.Slice", base64.StdEncoding.EncodeToString(boc)}})
	if e != nil {
		return reading{}, e
	}
	wallet, e := stackAddr(v.Get("0"))
	if e != nil {
		return reading{}, e
	}
	info, e := s.rest(ctx, "ton", "/api/v2/getAddressInformation?address="+url.QueryEscape(wallet), nil)
	if e != nil || !info.Get("ok").Bool() {
		return reading{}, invalidEvidence
	}
	if info.Get("result.state").String() == "uninitialized" {
		return reading{Amount: "0", Derived: wallet}, nil
	}
	v, e = s.tonGetter(ctx, wallet, "get_wallet_data", []any{})
	if e != nil {
		return reading{}, e
	}
	who, e := stackAddr(v.Get("1"))
	master, me := stackAddr(v.Get("2"))
	if e != nil || me != nil || who != a || master != t {
		return reading{}, invalidEvidence
	}
	x, e := integer(v.Get("0.1"), 16)
	if e != nil {
		return reading{}, e
	}
	return reading{Amount: x.String(), Derived: wallet}, nil
}

const tronGenesis = "00000000000000001ebf88508a03865c71d452e25f4d51194196a1d22b6653dc"
