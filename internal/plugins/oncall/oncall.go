// Package oncall is Omni-Notify's on-call plugin: incidents, schedules and
// escalation for the Omni On-Call app (docs/plugins/oncall.md).
//
// It meets the core at two points only. Notify routes alerts to it through a
// provider of kind "oncall" (as it would to PagerDuty), and it pages people
// by asking Notify to deliver through their own providers. Its data is in its
// own SQLite file and its API is under /oncall/api/v1. Notify's core stays a
// router and delivery engine, and runs exactly as before with the plugin off.
package oncall

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
	"github.com/pod32g/omni-notify/internal/plugin"
	"github.com/pod32g/omni-notify/internal/providers"
	"gopkg.in/yaml.v3"
)

// Plugin is the on-call plugin.
type Plugin struct {
	cfg  Config
	dir  *directory
	host plugin.Host
	log  *slog.Logger
	db   *store
	// verifier checks Omni Identity tokens; nil without an identity section.
	verifier *verifier
	// signIns is the device each person last signed in from, to log a change.
	signIns sync.Map // user id → device id

	mu         sync.Mutex
	incidents  map[string]*incident
	overrides  map[string][]override // by schedule, in creation order
	nextNumber int
	changeSeq  int64
	oncallSeq  int64             // the last change to who is on call
	lastOnCall map[string]string // schedule → user, for handoffs

	cancel context.CancelFunc
	done   chan struct{}
}

// New builds the plugin from its config section.
func New(section yaml.Node) (plugin.Plugin, error) {
	var cfg Config
	if err := section.Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg.Storage.Path == "" {
		cfg.Storage.Path = "oncall.db"
	}
	dir, err := buildDirectory(cfg)
	if err != nil {
		return nil, err
	}
	p := &Plugin{cfg: cfg, dir: dir}
	if cfg.Identity.Issuer != "" {
		p.verifier = newVerifier(cfg.Identity)
	}
	return p, nil
}

// Name is the config key and URL prefix.
func (p *Plugin) Name() string { return "oncall" }

// Start opens the plugin's database, registers the oncall provider kind and
// starts the escalation clock.
func (p *Plugin) Start(ctx context.Context, host plugin.Host) error {
	p.host = host
	p.log = host.Log
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	db, err := openStore(p.cfg.Storage.Path)
	if err != nil {
		return err
	}
	p.db = db
	if p.verifier != nil {
		p.verifier.now = p.now
	}
	if err := p.load(ctx); err != nil {
		_ = db.close()
		return err
	}
	host.Providers.Register("oncall", p.newProvider)

	loopCtx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go p.run(loopCtx)
	p.log.Info("on-call plugin ready", "users", len(p.dir.users), "services", len(p.dir.services),
		"schedules", len(p.dir.schedules), "incidents", len(p.incidents), "db", p.cfg.Storage.Path,
		"identity", p.cfg.Identity.Issuer)
	return nil
}

func (p *Plugin) load(ctx context.Context) error {
	p.incidents = map[string]*incident{}
	p.overrides = map[string][]override{}
	p.lastOnCall = map[string]string{}
	incs, err := p.db.incidents(ctx)
	if err != nil {
		return err
	}
	for _, inc := range incs {
		p.incidents[inc.ID] = inc
	}
	ovs, err := p.db.overrides(ctx)
	if err != nil {
		return err
	}
	for _, o := range ovs {
		p.overrides[o.ScheduleID] = append(p.overrides[o.ScheduleID], o)
	}
	var c counters
	if err := p.db.meta(ctx, "counters", &c); err != nil {
		return err
	}
	p.nextNumber, p.changeSeq, p.oncallSeq = c.NextNumber, c.ChangeSeq, c.OnCallSeq
	if err := p.db.meta(ctx, "last_oncall", &p.lastOnCall); err != nil {
		return err
	}
	return p.db.prune(ctx, p.now())
}

// run escalates every second and prunes hourly.
func (p *Plugin) run(ctx context.Context) {
	defer close(p.done)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastPrune := p.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick(ctx)
			if now := p.now(); now.Sub(lastPrune) > time.Hour {
				lastPrune = now
				if err := p.db.prune(ctx, now); err != nil {
					p.log.Warn("prune", "err", err)
				}
			}
		}
	}
}

// Stop ends the escalation clock and closes the database.
func (p *Plugin) Stop() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
	if p.db != nil {
		_ = p.db.close()
	}
}

func (p *Plugin) now() time.Time { return p.host.Clock.Now().UTC() }

// provider is a Notify provider of kind "oncall": config.service names the
// plugin service its events open incidents on.
type provider struct {
	p       *Plugin
	service string
}

func (p *Plugin) newProvider(cfg map[string]any, _ string) (providers.Provider, error) {
	svc, _ := cfg["service"].(string)
	if _, ok := p.dir.services[svc]; !ok {
		return nil, fmt.Errorf("oncall: config.service must be one of the plugin's services (%s)",
			strings.Join(p.dir.serviceIDs, ", "))
	}
	return &provider{p: p, service: svc}, nil
}

func (pr *provider) Send(ctx context.Context, msg models.NotificationMessage) error {
	return pr.p.ingest(ctx, pr.service, msg)
}
