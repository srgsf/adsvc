package testdb

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/srgsf/adsvc/internal/store"
)

// Node is one server of the testbed.
type Node struct {
	Name    string
	Port    int
	Role    string
	Imports []string // persona snapshots imported before the first start
	Unknown float64  // trust.unknown
	MaxAds  int      // tracking.max_ads
	Trust   []Trust  // per persona: trust.origins
	Peers   []Link
}

// Trust is a node's opinion of one persona.
type Trust struct {
	Persona string
	Weight  float64
	Blocked bool
}

// Link is a sync peer of a node.
type Link struct {
	To   string
	Mode string // pull, push or both
}

// Testbed is the layout: five nodes, so that every path of the federation is taken.
//
//	hub      holds the honest crowd and the curator; trusts them; nothing else at first
//	spoke    behind NAT: holds the collider and the trash and only pushes them to the hub
//	mirror   pulls the hub: gets everything the hub has, trash included, as a relay
//	stranger pulls the mirror and blocks the trash origin: a node with a policy
//	naive    imports everything and trusts everyone: what no policy looks like
func Testbed() []Node {
	trusted := []Trust{{"honest-1", 2, false}, {"honest-2", 2, false}, {"honest-3", 2, false}, {"curator", 3, false}}
	return []Node{
		{Name: "hub", Port: 8081, Role: "trusts the honest crowd and the curator; receives the spoke's push",
			Imports: []string{"honest-*", "curator"}, Unknown: 0.2, MaxAds: 10000, Trust: trusted},
		{Name: "spoke", Port: 8083, Role: "behind NAT: holds the collider and the trash, only pushes them to the hub",
			Imports: []string{"collider", "trash"}, Unknown: 0.2, MaxAds: 10000,
			Peers: []Link{{"hub", "push"}}},
		{Name: "mirror", Port: 8082, Role: "starts empty and pulls the hub: initial sync, relay of the spoke's records",
			Unknown: 0.2, MaxAds: 10000, Trust: trusted, Peers: []Link{{"hub", "pull"}}},
		{Name: "stranger", Port: 8084, Role: "starts empty, pulls the mirror, blocks the trash origin",
			Unknown: 0.2, MaxAds: 10000, Trust: append([]Trust{{"trash", 0, true}}, trusted...),
			Peers: []Link{{"mirror", "pull"}}},
		{Name: "naive", Port: 8085, Role: "imports everything, trusts everyone (unknown 1), tracks at most 1000 ads",
			Imports: []string{"honest-*", "curator", "collider", "trash"}, Unknown: 1, MaxAds: 1000},
	}
}

// Secrets are the tokens shared by every node of a testbed.
type Secrets struct{ Admin, Sync string }

func newSecrets() Secrets { return Secrets{Admin: token(), Sync: token()} }

func token() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Layout is what Build wrote.
type Layout struct {
	Dir       string
	Secrets   Secrets
	Nodes     []Node
	Hosts     map[string]string
	Snapshots map[string]string
}

// Build writes the testbed into dir: snapshots/<persona>.db, nodes/<node>/config.yml, and
// with seed, nodes/<node>/data with the node's imports done. hosts maps a node name to the
// host peers and clients reach it at (default localhost).
func Build(ctx context.Context, w *World, dir string, hosts map[string]string, seed bool) (*Layout, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	snaps := filepath.Join(dir, "snapshots")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(snaps, 0o750); err != nil {
		return nil, err
	}
	l := &Layout{Dir: dir, Secrets: newSecrets(), Nodes: Testbed(), Hosts: map[string]string{}}
	for _, n := range l.Nodes {
		l.Hosts[n.Name] = cmpOr(hosts[n.Name], "localhost")
	}
	if l.Snapshots, err = w.WriteSnapshots(ctx, snaps); err != nil {
		return nil, err
	}
	for _, n := range l.Nodes {
		nd := filepath.Join(dir, "nodes", n.Name)
		if err := os.MkdirAll(nd, 0o750); err != nil {
			return nil, err
		}
		cfg, err := l.config(w, n, filepath.Join(nd, "data"))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(nd, "config.yml"), []byte(cfg), 0o600); err != nil {
			return nil, err
		}
		if seed {
			if err := l.seed(ctx, w, n, filepath.Join(nd, "data")); err != nil {
				return nil, fmt.Errorf("seeding %s: %w", n.Name, err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "PLAN.txt"), []byte(l.Plan(w)), 0o600); err != nil {
		return nil, err
	}
	return l, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// personas expands the patterns of an import list ("honest-*").
func personas(w *World, patterns []string) []*Persona {
	var out []*Persona
	for _, p := range w.Personas {
		for _, pat := range patterns {
			if ok, _ := filepath.Match(pat, p.Name); ok {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func (l *Layout) seed(ctx context.Context, w *World, n Node, data string) error {
	if len(n.Imports) == 0 {
		return nil
	}
	st, err := store.Open(ctx, data)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, p := range personas(w, n.Imports) {
		res, err := st.DB.ImportSnapshot(ctx, l.Snapshots[p.Name])
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		fmt.Printf("%-9s <- %-9s %5d accepted, %d known, rejected %v\n", n.Name, p.Name, res.Accepted, res.Duplicate, res.Rejected)
	}
	return st.Lib.Refresh(ctx)
}

func (l *Layout) config(w *World, n Node, data string) (string, error) {
	var b strings.Builder
	f := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	f("# generated by adsgen: a testbed node (see PLAN.txt)")
	f("listen: :%d", n.Port)
	f("public_url: http://%s:%d", l.Hosts[n.Name], n.Port)
	f("data_dir: %s", data)
	f("log: {level: info}")
	f("auth:")
	f("  users:")
	f("    admin: {tokens: [%s], admin: true}", l.Secrets.Admin)
	f("    sync: {tokens: [%s]}", l.Secrets.Sync)
	f("trust:")
	f("  unknown: %g", n.Unknown)
	if len(n.Trust) > 0 {
		f("  origins:")
		for _, t := range n.Trust {
			p := w.Persona(t.Persona)
			if p == nil {
				return "", fmt.Errorf("node %s trusts %q: no such persona", n.Name, t.Persona)
			}
			if t.Blocked {
				f("    %q: {name: %s, blocked: true}", p.Origin(), p.Name)
			} else {
				f("    %q: {name: %s, weight: %g}", p.Origin(), p.Name, t.Weight)
			}
		}
	}
	f("tracking:")
	f("  max_ads: %d", n.MaxAds)
	f("sync:")
	f("  name: %s", n.Name)
	if len(n.Peers) > 0 {
		f("  peers:")
		for _, p := range n.Peers {
			peer, ok := l.node(p.To)
			if !ok {
				return "", fmt.Errorf("node %s peers with unknown node %q", n.Name, p.To)
			}
			f("    - name: %s", p.To)
			f("      url: http://%s:%d", l.Hosts[p.To], peer.Port)
			f("      token: %s", l.Secrets.Sync)
			f("      mode: %s", p.Mode)
			f("      interval: 1m")
		}
	}
	return b.String(), nil
}

func (l *Layout) node(name string) (Node, bool) {
	for _, n := range l.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// Plan is the text of PLAN.txt: what was generated, how to start it, what to look at.
func (l *Layout) Plan(w *World) string {
	var b strings.Builder
	f := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	o := w.Options
	f("adsgen testbed: seed %d, %d honest ads, %d files, %d collisions, %d trash ads, %d trash maps", o.Seed, o.Ads, o.Files, o.Collisions, o.TrashAds, o.TrashMaps)
	f("")
	f("Origins (snapshots/<name>.db; `adsvc import` takes them):")
	for _, p := range w.Personas {
		f("  %-9s %s  %5d ads %5d maps %5d records  %s", p.Name, p.Origin(), p.Ads, p.Maps, len(p.Records), p.Role)
	}
	f("")
	f("Nodes (start each with: adsvc proxy -config nodes/<name>/config.yml):")
	for _, n := range l.Nodes {
		f("  %-9s http://%s:%d  imports %v", n.Name, l.Hosts[n.Name], n.Port, n.Imports)
		f("            %s", n.Role)
	}
	f("")
	f("Login to any node's page with the admin token: %s", l.Secrets.Admin)
	f("Nodes reach each other with the sync user's token (already in the configs).")
	return b.String()
}
