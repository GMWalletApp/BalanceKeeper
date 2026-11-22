package balancekeeper

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/tidwall/gjson"
)

func bitcoinAddress(raw string) (string, error) {
	a, err := btcutil.DecodeAddress(raw, &chaincfg.MainNetParams)
	if err == nil && a.IsForNet(&chaincfg.MainNetParams) {
		switch a.(type) {
		case *btcutil.AddressPubKeyHash, *btcutil.AddressScriptHash, *btcutil.AddressWitnessPubKeyHash, *btcutil.AddressWitnessScriptHash, *btcutil.AddressTaproot:
			return a.EncodeAddress(), nil
		}
	}
	return "", errors.New("invalid Bitcoin mainnet address")
}

const bitcoinGenesis = "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f"

func bitcoinHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

func (s *Service) bitcoinText(ctx context.Context, path string) (string, error) {
	cfg := s.network(ctx, "bitcoin")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeURL(cfg.RPC, path), nil)
	if err != nil {
		return "", errors.New("invalid Bitcoin index URL")
	}
	if cfg.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Bearer)
	}
	res, err := s.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("Bitcoin index request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Bitcoin index HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 129))
	hash := strings.TrimSpace(string(b))
	if err != nil || len(b) > 128 || !bitcoinHash(hash) {
		return "", invalidEvidence
	}
	return strings.ToLower(hash), nil
}

func (s *Service) bitcoinHashAt(ctx context.Context, h int64) (string, error) {
	return s.bitcoinText(ctx, fmt.Sprintf("/block-height/%d", h))
}

func (s *Service) bitcoinPoint(ctx context.Context, hash string) (point, error) {
	v, err := s.rest(ctx, "bitcoin", "/block/"+hash, nil)
	if err != nil {
		return point{}, err
	}
	h, he := number(v.Get("height"), 10)
	stamp, se := number(v.Get("timestamp"), 10)
	if v.Get("id").Str != hash || he != nil || se != nil || stamp == 0 {
		return point{}, invalidEvidence
	}
	return point{Height: h, Block: hash, Time: ptr(iso(time.Unix(stamp, 0)))}, nil
}

func (s *Service) bitcoinHead(ctx context.Context) (point, error) {
	g, e := s.bitcoinHashAt(ctx, 0)
	if e != nil {
		return point{}, e
	}
	if g != bitcoinGenesis {
		return point{}, invalidEvidence
	}
	hash, e := s.bitcoinText(ctx, "/blocks/tip/hash")
	if e != nil {
		return point{}, e
	}
	tip, e := s.bitcoinPoint(ctx, hash)
	if e != nil {
		return point{}, e
	}
	depth := s.cfg.Networks["bitcoin"].Confirmations
	if depth == 0 {
		return tip, nil
	}
	h := max(0, tip.Height-depth)
	hash, e = s.bitcoinHashAt(ctx, h)
	if e != nil {
		return point{}, e
	}
	p, e := s.bitcoinPoint(ctx, hash)
	if e != nil || p.Height != h {
		return point{}, invalidEvidence
	}
	return p, nil
}

// The indexer's address is optional. Use the locking script for both incoming
// outputs and spent prevouts, including change and transactions with many inputs.
func bitcoinChanges(tx gjson.Result) (map[string]*big.Int, error) {
	changes := map[string]*big.Int{}
	add := func(o gjson.Result, spent bool) error {
		field := o.Get("scriptpubkey")
		if field.Type != gjson.String {
			return invalidEvidence
		}
		b, e := hex.DecodeString(field.Str)
		if e != nil {
			return invalidEvidence
		}
		amount, e := integer(o.Get("value"), 10)
		if e != nil || !amount.IsInt64() || amount.Int64() > btcutil.MaxSatoshi {
			return invalidEvidence
		}
		class, addrs, _, e := txscript.ExtractPkScriptAddrs(b, &chaincfg.MainNetParams)
		if e != nil {
			return invalidEvidence
		}
		switch class {
		case txscript.PubKeyHashTy, txscript.ScriptHashTy, txscript.WitnessV0PubKeyHashTy, txscript.WitnessV0ScriptHashTy, txscript.WitnessV1TaprootTy:
			if len(addrs) != 1 {
				return invalidEvidence
			}
			addr := addrs[0].EncodeAddress()
			if changes[addr] == nil {
				changes[addr] = new(big.Int)
			}
			if spent {
				changes[addr].Sub(changes[addr], amount)
			} else {
				changes[addr].Add(changes[addr], amount)
			}
		}
		return nil
	}
	ins, outs := tx.Get("vin").Array(), tx.Get("vout").Array()
	if len(ins) == 0 || len(outs) == 0 {
		return nil, invalidEvidence
	}
	for _, i := range ins {
		if i.Get("is_coinbase").Raw == "true" {
			if len(ins) != 1 {
				return nil, invalidEvidence
			}
			continue
		}
		if e := add(i.Get("prevout"), true); e != nil {
			return nil, e
		}
	}
	for _, o := range outs {
		if e := add(o, false); e != nil {
			return nil, e
		}
	}
	return changes, nil
}

// Esplora exposes current chain_stats only. Undo confirmed address changes
// above the requested height, traversing confirmed history in reverse order.
func (s *Service) bitcoinRewind(ctx context.Context, addr string, h int64, balance *big.Int) error {
	path := "/address/" + addr + "/txs/chain"
	cursor := ""
	seen := map[string]bool{}
	for {
		v, e := s.rest(ctx, "bitcoin", path+cursor, nil)
		if e != nil {
			return e
		}
		if !v.IsArray() {
			return invalidEvidence
		}
		rows := v.Array()
		for _, tx := range rows {
			id := tx.Get("txid").Str
			height, e := number(tx.Get("status.block_height"), 10)
			if e != nil || tx.Get("status.confirmed").Raw != "true" || !bitcoinHash(id) || seen[id] {
				return invalidEvidence
			}
			if height <= h {
				return nil
			}
			seen[id] = true
			changes, e := bitcoinChanges(tx)
			if e != nil {
				return e
			}
			delta := changes[addr]
			if delta == nil {
				return invalidEvidence
			}
			balance.Sub(balance, delta)
			cursor = "/" + id
		}
		if len(rows) < 25 {
			return nil
		}
	}
}

func (s *Service) bitcoinBalance(ctx context.Context, a, t string, h point) (reading, error) {
	if t != "native" {
		return reading{}, invalidEvidence
	}
	tip, e := s.bitcoinText(ctx, "/blocks/tip/hash")
	if e != nil {
		return reading{}, e
	}
	v, e := s.rest(ctx, "bitcoin", "/address/"+a, nil)
	if e != nil {
		return reading{}, e
	}
	funded, fe := integer(v.Get("chain_stats.funded_txo_sum"), 10)
	spent, se := integer(v.Get("chain_stats.spent_txo_sum"), 10)
	if v.Get("address").Str != a || fe != nil || se != nil || funded.Cmp(spent) < 0 {
		return reading{}, invalidEvidence
	}
	value := funded.Sub(funded, spent)
	if tip != h.Block {
		if s.cfg.Networks["bitcoin"].Confirmations == 0 {
			return reading{}, invalidEvidence
		}
		if e := s.bitcoinRewind(ctx, a, h.Height, value); e != nil {
			return reading{}, e
		}
	}
	end, e := s.bitcoinText(ctx, "/blocks/tip/hash")
	if e != nil {
		return reading{}, e
	}
	block, e := s.bitcoinHashAt(ctx, h.Height)
	if e != nil || end != tip || block != h.Block || value.Sign() < 0 || !value.IsInt64() || value.Int64() > btcutil.MaxSatoshi {
		return reading{}, invalidEvidence
	}
	return reading{Name: "BTC", Decimals: ptr(8), Amount: value.String(), Height: &h.Height, Time: h.Time}, nil
}

func (s *Service) bitcoinEvidence(ctx context.Context, ev evidence, scanner bool) (evidence, error) {
	tx, e := s.rest(ctx, "bitcoin", "/tx/"+ev.hash, nil)
	if e != nil {
		return ev, e
	}
	h, e := number(tx.Get("status.block_height"), 10)
	block := tx.Get("status.block_hash").Str
	if e != nil || tx.Get("txid").Str != ev.hash || tx.Get("status.confirmed").Raw != "true" || !bitcoinHash(block) || (scanner && h != ev.height) {
		return ev, invalidEvidence
	}
	canon, e := s.bitcoinHashAt(ctx, h)
	if e != nil || canon != block {
		return ev, invalidEvidence
	}
	ev.height, ev.block, ev.data = h, block, tx
	return ev, nil
}
