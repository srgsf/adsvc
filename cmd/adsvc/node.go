package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/config"
	"github.com/srgsf/adsvc/internal/library"
	"github.com/srgsf/adsvc/internal/proxy"
	"github.com/srgsf/adsvc/internal/record"
	"github.com/srgsf/adsvc/internal/replica"
)

// node is the federation part of the config file: trust, tracking, sync.
type node struct {
	policy  catalog.Policy
	origins []catalog.OriginRule
	share   catalog.Share
	peers   []replica.Peer
	name    string
	serve   bool
	public  bool
}

func nodeSettings(file *config.Proxy) (node, error) {
	t := file.Trust
	pol := catalog.DefaultPolicy
	set := func(dst *float64, v *float64) {
		if v != nil {
			*dst = *v
		}
	}
	set(&pol.SelfWeight, t.Self)
	set(&pol.UnknownWeight, t.Unknown)
	set(&pol.MinTrust, t.MinTrust)
	set(&pol.FileMapMin, t.FileMapMin)
	pol.QuotaPerDay = cmp.Or(t.QuotaPerDay, pol.QuotaPerDay)
	pol.MaxAds = min(cmp.Or(file.Tracking.MaxAds, library.MaxAds), library.MaxAds)
	n := node{policy: pol, name: file.Sync.Name, serve: file.Sync.Serve == nil || *file.Sync.Serve, public: file.Sync.Public}
	for _, id := range slices.Sorted(maps.Keys(t.Origins)) { // SetOrigins gets the same order every time
		o := t.Origins[id]
		origin, err := record.ParseOrigin(id)
		if err != nil {
			return node{}, err
		}
		n.origins = append(n.origins, catalog.OriginRule{Origin: origin, Name: o.Name, Weight: o.Weight, Blocked: o.Blocked})
	}
	on := func(v *bool) bool { return v == nil || *v }
	s := file.Sync.Share
	n.share = catalog.Share{Ads: on(s.Ads), Labels: on(s.Labels), Votes: on(s.Votes), FileMaps: on(s.FileMaps)}
	for _, p := range file.Sync.Peers {
		pull, push := p.PullPush()
		n.peers = append(n.peers, replica.Peer{Name: p.Name, URL: p.URL, Token: p.Token, Pull: pull, Push: push, Interval: p.Interval})
	}
	return n, nil
}

// apply makes n the node's settings: trust policy and origins, what is shared, the peers;
// the ad index follows the new tracking set. The origins go first: when writing them
// fails, nothing has changed.
func (n node) apply(ctx context.Context, px *proxy.Proxy, sy *replica.Syncer, front *frontHandler) error {
	if err := px.DB.SetOrigins(ctx, n.origins); err != nil {
		return fmt.Errorf("trust.origins: %w", err)
	}
	px.DB.SetPolicy(n.policy)
	if err := px.DB.SetShare(ctx, n.share); err != nil {
		return fmt.Errorf("sync.share: %w", err)
	}
	if err := px.Lib.Refresh(ctx); err != nil {
		return err
	}
	sy.SetPeers(n.peers)
	front.serve.Store(n.serve)
	front.public.Store(n.public)
	return nil
}
