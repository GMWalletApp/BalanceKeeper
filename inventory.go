package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	pb "github.com/block-vision/sui-go-sdk/pb/sui/rpc/v2"
)

func (s *Service) ownerInventoryEnabled(n string) bool {
	return n == "solana" || n == "sui" || s.cfg.Networks[n].InventoryRPC != ""
}

// The index supplies candidates. Only a separate network balance read can set
// a holding or write a snapshot, including when an asset disappears from a page.
func (s *Service) ownerBalances(ctx context.Context, n, a string, height int64) (map[string]string, error) {
	out := map[string]string{}
	switch n {
	case "solana":
		sums := map[string]*big.Int{}
		for _, program := range []string{splProgram, token2022} {
			v, e := s.rpc(ctx, n, "getTokenAccountsByOwner", a, M{"programId": program}, M{"commitment": "finalized", "minContextSlot": height, "encoding": "jsonParsed"})
			slot, se := number(v.Get("context.slot"), 10)
			if e != nil || se != nil || slot < height || !v.Get("value").IsArray() {
				return nil, invalidEvidence
			}
			for _, r := range v.Get("value").Array() {
				info := r.Get("account.data.parsed.info")
				t, e := canonical(n, info.Get("mint").Str)
				amt, ae := integer(info.Get("tokenAmount.amount"), 10)
				if e != nil || ae != nil || info.Get("owner").Str != a {
					return nil, invalidEvidence
				}
				if sums[t] == nil {
					sums[t] = new(big.Int)
				}
				sums[t].Add(sums[t], amt)
			}
		}
		for t, v := range sums {
			out[t] = v.String()
		}
	case "sui":
		ctx, c, close, e := s.suiConn(ctx)
		if e != nil {
			return nil, e
		}
		defer close()
		req := &pb.ListBalancesRequest{Owner: &a}
		seen := map[string]bool{}
		for {
			page, e := pb.NewStateServiceClient(c).ListBalances(ctx, req)
			if e != nil {
				return nil, e
			}
			for _, coin := range page.GetBalances() {
				t, e := tokenID(n, coin.GetCoinType())
				if e != nil || coin.Balance == nil {
					return nil, invalidEvidence
				}
				if t != "native" {
					out[t] = strconv.FormatUint(coin.GetBalance(), 10)
				}
			}
			req.PageToken = page.GetNextPageToken()
			if len(req.PageToken) == 0 {
				break
			}
			if seen[string(req.PageToken)] {
				return nil, invalidEvidence
			}
			seen[string(req.PageToken)] = true
		}
	default:
		cfg := s.cfg.Networks[n]
		id, e := s.rpcAt(ctx, cfg.InventoryRPC, cfg.InventoryBearer, "eth_chainId")
		num, ne := number(id, 16)
		if e != nil || ne != nil || num != chainIDs[n] {
			return nil, invalidEvidence
		}
		opts := M{"maxCount": 100}
		seen := map[string]bool{}
		for {
			page, e := s.rpcAt(ctx, cfg.InventoryRPC, cfg.InventoryBearer, "alchemy_getTokenBalances", a, "erc20", opts)
			if e != nil {
				return nil, e
			}
			if !strings.EqualFold(page.Get("address").Str, a) || !page.Get("tokenBalances").IsArray() {
				return nil, invalidEvidence
			}
			for _, row := range page.Get("tokenBalances").Array() {
				t, e := canonical(n, row.Get("contractAddress").Str)
				if e != nil {
					return nil, e
				}
				// An individual balance error still identifies a contract. Let the balance RPC verify it.
				if row.Get("error").Str != "" {
					out[t] = "unknown"
					continue
				}
				amt, e := integer(row.Get("tokenBalance"), 16)
				if e != nil {
					return nil, e
				}
				out[t] = amt.String()
			}
			key := page.Get("pageKey")
			if !key.Exists() || key.Raw == "null" || key.Raw == `""` {
				break
			}
			if key.Str == "" || seen[key.Str] {
				return nil, invalidEvidence
			}
			seen[key.Str] = true
			opts["pageKey"] = key.Str
		}
	}
	return out, nil
}

func (s *Service) nextInventory(ctx context.Context, n string) (bool, error) {
	var wid, start int64
	var a string
	now := time.Now()
	lease := now.Add(5 * time.Minute).UnixMilli()
	err := s.db.QueryRowContext(ctx, `UPDATE watches SET discovery_due=? WHERE id=(SELECT id FROM watches WHERE network=? AND start>=0 AND discovery_due<=? ORDER BY discovery_due,id LIMIT 1) RETURNING id,address,start`, lease, n, now.UnixMilli()).Scan(&wid, &a, &start)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	observed, err := s.ownerBalances(ctx, n, a, max(0, start-1))
	if err != nil {
		_, de := s.db.ExecContext(ctx, "UPDATE watches SET discovery_due=? WHERE id=? AND discovery_due=?", now.Add(time.Minute).UnixMilli(), wid, lease)
		return true, errors.Join(err, de)
	}
	return true, s.atomic(ctx, func(tx *sql.Tx) error {
		var live bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM watches WHERE id=?)", wid).Scan(&live); e != nil || !live {
			return e
		}
		rows, e := tx.QueryContext(ctx, "SELECT token,amount FROM assets WHERE watch=? AND token<>'native'", wid)
		if e != nil {
			return e
		}
		for rows.Next() {
			var t string
			var prev *string
			if e := rows.Scan(&t, &prev); e != nil {
				rows.Close()
				return e
			}
			next, present := observed[t]
			if !present {
				next = "0"
			}
			if prev != nil && *prev == next {
				delete(observed, t)
			} else {
				observed[t] = next
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for t, hint := range observed {
			_, e = tx.ExecContext(ctx, `INSERT INTO jobs(watch,token,source,observation,min_height)
                SELECT ?,?,'reconcile',?,? WHERE NOT EXISTS(SELECT 1 FROM jobs WHERE watch=? AND token=?)
                AND (?<>'0' OR EXISTS(SELECT 1 FROM assets WHERE watch=? AND token=?))`, wid, t, fmt.Sprintf("owner-inventory/%d", now.UnixNano()), start-1, wid, t, hint, wid, t)
			if e != nil {
				return e
			}
		}
		return nil
	})
}
