package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/tidwall/gjson"
)

func (s *Service) evmEvidence(ctx context.Context, n string, ev evidence, scanner bool) (evidence, error) {
	delivered := ev.data
	// Read the entire receipt even when the delivery already contains logs.
	// A successful lookup can legitimately return an empty log list.
	tx, txErr := s.rpc(ctx, n, "eth_getTransactionByHash", ev.hash)
	receipt, receiptErr := s.rpc(ctx, n, "eth_getTransactionReceipt", ev.hash)
	if txErr != nil || receiptErr != nil || !strings.EqualFold(tx.Get("hash").Str, ev.hash) || !strings.EqualFold(receipt.Get("transactionHash").Str, ev.hash) || !receipt.Get("logs").IsArray() || !tx.Get("blockNumber").Exists() {
		full, err := s.lookup(ctx, n, ev.hash)
		if err != nil {
			return ev, err
		}
		tx, receipt = full.Get("transaction"), full.Get("receipt")
	}
	if !tx.IsObject() || !receipt.IsObject() {
		return ev, errors.New("transaction or receipt unavailable")
	}
	h, err := number(tx.Get("blockNumber"), 16)
	rh, errH := number(receipt.Get("blockNumber"), 16)
	block := strings.ToLower(tx.Get("blockHash").Str)
	if err != nil || errH != nil || rh != h || scanner && h != ev.height || !strings.EqualFold(tx.Get("hash").Str, ev.hash) || !strings.EqualFold(receipt.Get("transactionHash").Str, ev.hash) || !strings.EqualFold(receipt.Get("blockHash").Str, block) || !receipt.Get("logs").IsArray() {
		return ev, invalidEvidence
	}
	if _, err = normalizeHash("evm", block); err != nil {
		return ev, err
	}
	if _, err := canonical(n, tx.Get("from").Str); err != nil {
		return ev, invalidEvidence
	}
	if tx.Get("type").Str == "0x4" && !tx.Get("authorizationList").IsArray() {
		return ev, invalidEvidence
	}
	for _, l := range receipt.Get("logs").Array() {
		if _, err := canonical(n, l.Get("address").Str); err != nil || !l.Get("topics").IsArray() || l.Get("removed").Bool() || l.Get("blockHash").Exists() && !strings.EqualFold(l.Get("blockHash").Str, block) || l.Get("transactionHash").Exists() && !strings.EqualFold(l.Get("transactionHash").Str, ev.hash) {
			return ev, invalidEvidence
		}
		data := l.Get("data").Str
		if !strings.HasPrefix(data, "0x") {
			return ev, invalidEvidence
		}
		if _, err := hex.DecodeString(data[2:]); err != nil {
			return ev, invalidEvidence
		}
		for _, topic := range l.Get("topics").Array() {
			if _, err := normalizeHash("evm", topic.Str); err != nil {
				return ev, invalidEvidence
			}
		}
	}
	ev.data = gjson.Parse(compact(M{"transaction": json.RawMessage(tx.Raw), "receipt": json.RawMessage(receipt.Raw)}))
	ev.height, ev.block = h, block
	if receipt.Get("status").Str != "0x0" {
		if strings.EqualFold(delivered.Get("transaction.blockHash").Str, block) && strings.EqualFold(delivered.Get("transaction.hash").Str, ev.hash) {
			ev.trace = delivered.Get("trace")
		}
		tr, err := s.rpc(ctx, n, "debug_traceTransaction", ev.hash, M{"tracer": "callTracer"})
		switch {
		case err == nil && tr.IsObject() && tr.Get("type").Str != "":
			ev.trace = tr
		case err != nil && (tr.Get("error.code").Int() == -32601 || strings.Contains(strings.ToLower(tr.Get("error.message").Str), "not supported")): // The canonical receipt remains available on standard RPC nodes.
		default:
			ev.traceRetry = errors.New("transaction trace unavailable, will retry")
		}
	}
	canon, err := s.evmBlock(ctx, n, h)
	if err != nil || !strings.EqualFold(canon.Block, block) {
		return ev, invalidEvidence
	}
	return ev, nil
}

var (
	userOpTopic     = crypto.Keccak256Hash([]byte("UserOperationEvent(bytes32,address,address,uint256,bool,uint256,uint256)")).Hex()
	depositTopic    = crypto.Keccak256Hash([]byte("Deposit(address,uint256)")).Hex()
	withdrawalTopic = crypto.Keccak256Hash([]byte("Withdrawal(address,uint256)")).Hex()
)

func parseEVM(n string, ev evidence, contracts map[string]bool) candidates {
	tx, r := ev.data.Get("transaction"), ev.data.Get("receipt")
	c := candidates{}
	c.add(n, tx.Get("from").Str, "native")
	c.touch(n, tx.Get("to").Str)
	for _, raw := range tx.Get("authorizationList").Array() {
		var auth types.SetCodeAuthorization
		if json.Unmarshal([]byte(raw.Raw), &auth) != nil || !auth.ChainID.IsZero() && (!auth.ChainID.IsUint64() || auth.ChainID.Uint64() != uint64(chainIDs[n])) {
			continue
		}
		if addr, err := auth.Authority(); err == nil {
			c.touch(n, addr.Hex())
		}
	}
	if r.Get("status").Str == "0x0" {
		return c
	}
	if value, e := integer(tx.Get("value"), 16); e == nil && value.Sign() > 0 {
		c.add(n, tx.Get("to").Str, "native")
		c.add(n, r.Get("contractAddress").Str, "native")
	}
	remember := func(raw string) {
		if t, e := canonical(n, raw); e == nil {
			contracts[t] = true
		}
	}
	remember(tx.Get("to").Str)
	remember(r.Get("contractAddress").Str)
	logs := r.Get("logs")
	addLogs(c, n, logs)
	for _, l := range logs.Array() {
		remember(l.Get("address").Str)
		topics := l.Get("topics").Array()
		addressWords(c, n, strings.TrimPrefix(l.Get("data").Str, "0x"))
		if len(topics) == 0 {
			continue
		}
		for _, topic := range topics[1:] {
			c.touch(n, topicAddr(topic))
		}
		switch strings.ToLower(topics[0].Str) {
		case userOpTopic:
			if len(topics) == 4 {
				c.touch(n, topicAddr(topics[2]))
				c.touch(n, topicAddr(topics[3]))
			}
		case depositTopic, withdrawalTopic:
			if len(topics) == 2 && len(l.Get("data").Str) == 66 {
				c.touch(n, topicAddr(topics[1]))
			}
		}
	}
	if tx.Get("to").Str != "" {
		tokenParticipants(c, n, tx.Get("from").Str, tx.Get("input").Str)
	}
	var walk func(gjson.Result, string, string)
	walk = func(frame gjson.Result, parent, caller string) {
		if !frame.IsObject() || frame.Get("error").Str != "" || frame.Get("revertReason").Str != "" {
			return
		}
		from, to := frame.Get("from").Str, frame.Get("to").Str
		c.touch(n, from)
		c.touch(n, to)
		kind := strings.ToUpper(frame.Get("type").Str)
		storage, msgSender := to, from
		switch kind {
		case "DELEGATECALL":
			storage, msgSender = parent, caller
		case "CALLCODE":
			storage = parent
		case "CALL", "CREATE", "CREATE2", "SELFDESTRUCT":
			if value, e := integer(frame.Get("value"), 16); e == nil && value.Sign() > 0 {
				c.add(n, from, "native")
				c.add(n, to, "native")
			}
		}
		remember(storage)
		if (kind == "CALL" || kind == "DELEGATECALL" || kind == "CALLCODE") && frame.Get("output").Str != "0x"+strings.Repeat("0", 64) {
			tokenParticipants(c, n, msgSender, frame.Get("input").Str)
		}
		for _, child := range frame.Get("calls").Array() {
			walk(child, storage, msgSender)
		}
	}
	walk(ev.trace, "", "")
	return c
}

// Contract-specific mints, airdrops and batches often name recipients only
// in ABI words. These are participant hints; matching a numeric argument can
// at most cause an additional confirmed balance read for a registered wallet.
func addressWords(c candidates, n, words string) {
	for len(words) >= 64 {
		word := words[:64]
		words = words[64:]
		if strings.HasPrefix(word, "000000000000000000000000") && strings.Trim(word[24:], "0") != "" {
			c.touch(n, "0x"+word[24:])
		}
	}
}
func tokenParticipants(c candidates, n, from, input string) {
	if len(input) < 10 || !strings.HasPrefix(input, "0x") {
		return
	}
	c.touch(n, from)
	addressWords(c, n, input[10:])
}
