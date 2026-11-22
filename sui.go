package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"strconv"
	"time"

	pb "github.com/block-vision/sui-go-sdk/pb/sui/rpc/v2"
	"github.com/tidwall/gjson"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func (s *Service) suiConn(ctx context.Context) (context.Context, *grpc.ClientConn, func(), error) {
	cfg := s.cfg.Networks["sui"]
	u, e := url.Parse(cfg.RPC)
	if e != nil || u.Hostname() == "" || u.Path != "" && u.Path != "/" {
		return ctx, nil, nil, invalidEvidence
	}
	var cred credentials.TransportCredentials = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	port := u.Port()
	if u.Scheme == "http" {
		cred = insecure.NewCredentials()
		if port == "" {
			port = "80"
		}
	}
	if port == "" {
		port = "443"
	}
	c, e := grpc.NewClient("dns:///"+net.JoinHostPort(u.Hostname(), port), grpc.WithTransportCredentials(cred))
	if e != nil {
		return ctx, nil, nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if cfg.Bearer != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+cfg.Bearer)
	}
	return ctx, c, func() { cancel(); c.Close() }, nil
}
func (s *Service) suiHead(ctx context.Context) (point, error) {
	ctx, c, close, e := s.suiConn(ctx)
	if e != nil {
		return point{}, e
	}
	defer close()
	r, e := pb.NewLedgerServiceClient(c).GetServiceInfo(ctx, &pb.GetServiceInfoRequest{})
	if e != nil || r.GetChainId() != "4btiuiMPvEENsttpZC7CZ53DruC3MAgfznDbASZ7DR6S" || r.CheckpointHeight == nil {
		return point{}, invalidEvidence
	}
	return point{Height: int64(r.GetCheckpointHeight())}, nil
}
func (s *Service) suiBalance(ctx context.Context, a, t string) (reading, error) {
	ctx, c, close, e := s.suiConn(ctx)
	if e != nil {
		return reading{}, e
	}
	defer close()
	var dec *int
	if t == "native" {
		t = "0x2::sui::SUI"
		dec = ptr(9)
	}
	r, e := pb.NewStateServiceClient(c).GetBalance(ctx, &pb.GetBalanceRequest{Owner: &a, CoinType: &t})
	if e != nil || r.GetBalance() == nil || r.GetBalance().Balance == nil {
		return reading{}, invalidEvidence
	}
	var name string
	if dec == nil {
		info, err := pb.NewStateServiceClient(c).GetCoinInfo(ctx, &pb.GetCoinInfoRequest{CoinType: &t})
		if err == nil && info.GetMetadata() != nil {
			md := info.GetMetadata()
			name = md.GetSymbol()
			if md.Decimals != nil && md.GetDecimals() <= 255 {
				dec = ptr(int(md.GetDecimals()))
			}
		}
	}
	return reading{Name: name, Amount: strconv.FormatUint(r.GetBalance().GetBalance(), 10), Decimals: dec}, nil
}

func (s *Service) suiTransaction(ctx context.Context, hash string) (gjson.Result, error) {
	ctx, c, close, e := s.suiConn(ctx)
	if e != nil {
		return gjson.Result{}, e
	}
	defer close()
	r, e := pb.NewLedgerServiceClient(c).GetTransaction(ctx, &pb.GetTransactionRequest{Digest: &hash, ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"digest", "checkpoint", "transaction.sender", "transaction.gas_payment.owner", "effects.changed_objects", "balance_changes"}}})
	if e != nil {
		return gjson.Result{}, e
	}
	if r.GetTransaction() == nil {
		return gjson.Result{}, invalidEvidence
	}
	b, e := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(r.GetTransaction())
	return gjson.ParseBytes(b), e
}
