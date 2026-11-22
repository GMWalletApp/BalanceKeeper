package balancekeeper

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// NodeEndpoints groups read endpoints and credentials belonging to one provider.
type NodeEndpoints struct {
	RPC             string `json:"rpc"`
	Bearer          string `json:"bearer,omitempty"`
	APIKey          string `json:"apiKey,omitempty"`
	Index           string `json:"index,omitempty"`
	IndexBearer     string `json:"indexBearer,omitempty"`
	IndexAPIKey     string `json:"indexApiKey,omitempty"`
	InventoryRPC    string `json:"tokenInventoryRPC,omitempty"`
	InventoryBearer string `json:"tokenInventoryBearer,omitempty"`
}

var defaultBackups = map[string]string{
	"ethereum":  "https://ethereum-rpc.publicnode.com",
	"arbitrum":  "https://arbitrum-one-rpc.publicnode.com",
	"binance":   "https://bsc-rpc.publicnode.com",
	"avalanche": "https://avalanche-c-chain-rpc.publicnode.com",
	"polygon":   "https://polygon-bor-rpc.publicnode.com",
	"optimism":  "https://optimism-rpc.publicnode.com",
	"base":      "https://base-rpc.publicnode.com",
	"hyperevm":  "https://hyperliquid-rpc.publicnode.com",
	"arc":       "https://arc-rpc.publicnode.com",
	"robinhood": "https://robinhood-rpc.publicnode.com",
	"monad":     "https://rpc-mainnet.monadinfra.com",
	"plasma":    "https://rpc.plasma.to",
	"solana":    "https://public.rpc.solanavibestation.com",
	"sui":       "https://sui-grpc.publicnode.com:443",
	"aptos":     "https://aptos-rest.publicnode.com",
	"tron":      "https://tron-rpc.publicnode.com",
	"ton":       "https://toncenter.com",
	"bitcoin":   "https://mempool.space/api",
}

func (n Network) withEndpoints(b NodeEndpoints) Network {
	n.RPC, n.Bearer, n.APIKey = b.RPC, b.Bearer, b.APIKey
	n.Index, n.IndexBearer, n.IndexAPIKey = b.Index, b.IndexBearer, b.IndexAPIKey
	n.InventoryRPC, n.InventoryBearer = b.InventoryRPC, b.InventoryBearer
	return n
}

// Normalize versioned roots once so requests append the API version exactly once.
func endpointRoot(raw, suffix string) string {
	u, e := url.Parse(raw)
	if e != nil {
		return raw
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), suffix)
	return u.String()
}

func normalizeEndpoints(name string, b NodeEndpoints) (NodeEndpoints, error) {
	for _, v := range []*string{&b.RPC, &b.Bearer, &b.APIKey, &b.Index, &b.IndexBearer, &b.IndexAPIKey, &b.InventoryRPC, &b.InventoryBearer} {
		raw := *v
		*v = os.ExpandEnv(raw)
		if raw != "" && *v == "" {
			return b, fmt.Errorf("missing endpoint or credential for %s", name)
		}
	}
	if name == "sui" {
		if strings.HasPrefix(b.RPC, "grpcs://") {
			b.RPC = "https://" + strings.TrimPrefix(b.RPC, "grpcs://")
		} else if strings.HasPrefix(b.RPC, "grpc://") {
			b.RPC = "http://" + strings.TrimPrefix(b.RPC, "grpc://")
		}
	}
	if !validURL(b.RPC) || b.InventoryRPC != "" && (chainIDs[name] == 0 || !validURL(b.InventoryRPC)) || b.Index != "" && (name != "ton" || !validURL(b.Index)) {
		return b, fmt.Errorf("invalid endpoint for %s", name)
	}
	if b.APIKey != "" && name != "ton" && name != "tron" || b.IndexAPIKey != "" && name != "ton" {
		return b, errors.New("apiKey is supported for TON and TRON only")
	}
	if b.InventoryBearer != "" && b.InventoryRPC == "" || (b.IndexBearer != "" || b.IndexAPIKey != "") && b.Index == "" {
		return b, fmt.Errorf("unused index credentials for %s", name)
	}
	if name == "sui" {
		u, _ := url.Parse(b.RPC)
		if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" {
			return b, errors.New("Sui requires a gRPC host and port")
		}
	}
	if name == "aptos" {
		b.RPC = endpointRoot(b.RPC, "/v1")
	}
	if name == "ton" {
		b.RPC = endpointRoot(b.RPC, "/api/v2")
		if b.Index != "" {
			b.Index = endpointRoot(b.Index, "/api/v3")
		}
	}
	return b, nil
}

func configureNode(name string, n Network) (Network, error) {
	if !supported(name) || n.Confirmations < 0 {
		return n, fmt.Errorf("invalid network %s", name)
	}
	primary, err := normalizeEndpoints(name, NodeEndpoints{RPC: n.RPC, Bearer: n.Bearer, APIKey: n.APIKey, Index: n.Index, IndexBearer: n.IndexBearer, IndexAPIKey: n.IndexAPIKey, InventoryRPC: n.InventoryRPC, InventoryBearer: n.InventoryBearer})
	if err != nil {
		return n, err
	}
	b := NodeEndpoints{RPC: defaultBackups[name]}
	if n.Backup != nil {
		b = *n.Backup
		if b.RPC == "" {
			b.RPC = defaultBackups[name]
		}
	}
	b, err = normalizeEndpoints(name, b)
	if err != nil {
		return n, err
	}
	if strings.TrimRight(primary.RPC, "/") == strings.TrimRight(b.RPC, "/") {
		return n, fmt.Errorf("%s needs a distinct backup.rpc", name)
	}
	if b.InventoryRPC != "" && primary.InventoryRPC == "" {
		return n, fmt.Errorf("%s backup inventory requires a primary inventory", name)
	}
	configured := n.withEndpoints(primary)
	configured.Backup = &b
	return configured, nil
}

type nodeContextKey struct{}
type selectedNode struct {
	name          string
	config        Network
	head          *point
	indexVerified bool
}

func (s *Service) network(ctx context.Context, name string) Network {
	if node, ok := ctx.Value(nodeContextKey{}).(*selectedNode); ok && node.name == name {
		return node.config
	}
	return s.cfg.Networks[name]
}

// Restart an entire read on the other node. This pins pagination, block checks
// and balance reads to the same provider. Callbacks must not commit DB writes.
func readNodes[T any](ctx context.Context, s *Service, name string, read func(context.Context) (T, error)) (T, error) {
	cfg := s.cfg.Networks[name]
	if node, ok := ctx.Value(nodeContextKey{}).(*selectedNode); ok && node.name == name {
		return read(ctx)
	}
	if cfg.Backup == nil {
		return read(ctx)
	}
	var value T
	var errs []error
	for _, route := range []Network{cfg, cfg.withEndpoints(*cfg.Backup)} {
		if err := ctx.Err(); err != nil {
			return value, err
		}
		node := &selectedNode{name: name, config: route}
		call := context.WithValue(ctx, nodeContextKey{}, node)
		h, err := s.head(call, name)
		if err == nil {
			node.head = &h
			value, err = read(call)
			if err == nil {
				return value, nil
			}
		}
		errs = append(errs, err)
	}
	return value, fmt.Errorf("%s primary and backup failed: %w", name, errors.Join(errs...))
}

// Keep routing parameters (including provider tokens) on the configured URL.
func nodeURL(base, path string) string {
	root, query, _ := strings.Cut(base, "?")
	suffix, args, _ := strings.Cut(path, "?")
	if query != "" && args != "" {
		query += "&"
	}
	query += args
	result := strings.TrimRight(root, "/") + suffix
	if query != "" {
		result += "?" + query
	}
	return result
}
