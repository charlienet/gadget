package redis

import (
	"context"
	"strings"
	"sync"

	"github.com/hashicorp/go-version"
)

// Capability provides cached information about the Redis server's version
// and loaded modules. It is lazily populated on first access and can be
// refreshed via Probe or Refresh.
type Capability struct {
	mu    sync.Mutex
	rdb   *redisClient
	ready bool

	version    string
	versionSem *version.Version
	modules    []moduleInfo
	stack      bool
}

type moduleInfo struct {
	Name    string
	Version string
}

func newCapability(rdb *redisClient) *Capability {
	return &Capability{rdb: rdb}
}

// Probe sends INFO commands to the server and caches version and module info.
// Returns an error only if the server is unreachable.
func (c *Capability) Probe(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probeLocked(ctx)
}

// Refresh forces a re-probe on the next access (discards cached data).
func (c *Capability) Refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = false
}

func (c *Capability) probeLocked(ctx context.Context) error {
	c.ready = true

	// --- Server version ---
	info, err := c.rdb.Info(ctx, "Server").Result()
	if err == nil {
		for _, line := range strings.Split(info, "\r\n") {
			if after, found := strings.CutPrefix(line, "redis_version:"); found {
				c.version = after
				if v, err := version.NewVersion(after); err == nil {
					c.versionSem = v
				}
				break
			}
		}
	}

	// --- Modules ---
	modInfo, err := c.rdb.Info(ctx, "Modules").Result()
	if err == nil {
		c.modules = c.modules[:0]
		for _, line := range strings.Split(modInfo, "\r\n") {
			if after, found := strings.CutPrefix(line, "module:"); found {
				m := parseModuleLine(after)
				if m.Name != "" {
					c.modules = append(c.modules, m)
				}
			}
		}
		c.stack = len(c.modules) > 0
	}

	return nil
}

// parseModuleLine parses a module info line like:
//
//	module:name=ReJSON,ver=20000,api=1,filters=0,usedby=[],...
func parseModuleLine(line string) moduleInfo {
	var m moduleInfo
	for _, part := range strings.Split(line, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "name":
			m.Name = v
		case "ver":
			m.Version = v
		}
	}
	return m
}

// ensureLoaded probes once on first access.
func (c *Capability) ensureLoaded() {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	if !ready {
		c.Probe(context.Background())
	}
}

// Version returns the Redis server version string (e.g. "7.2.5").
func (c *Capability) Version() string {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// VersionAtLeast returns true if the server version >= minVersion (e.g. "7.4").
func (c *Capability) VersionAtLeast(minVersion string) bool {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.versionSem == nil {
		return false
	}
	constraint, err := version.NewConstraint(">= " + minVersion)
	if err != nil {
		return false
	}
	return constraint.Check(c.versionSem)
}

// IsStack returns true if any Redis Stack modules are loaded.
func (c *Capability) IsStack() bool {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stack
}

// HasModule returns true if a module with the given name is loaded.
// Matching is case-insensitive.
func (c *Capability) HasModule(name string) bool {
	c.ensureLoaded()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.modules {
		if strings.EqualFold(m.Name, name) {
			return true
		}
	}
	return false
}

// Convenience module checks.
func (c *Capability) HasJSON() bool        { return c.HasModule("ReJSON") }
func (c *Capability) HasSearch() bool      { return c.HasModule("search") }
func (c *Capability) HasBloom() bool       { return c.HasModule("bf") }
func (c *Capability) HasCuckoo() bool      { return c.HasModule("cf") }
func (c *Capability) HasTimeSeries() bool  { return c.HasModule("timeseries") }
func (c *Capability) HasTopK() bool        { return c.HasModule("topk") }
func (c *Capability) HasTDigest() bool     { return c.HasModule("tdigest") }
func (c *Capability) HasGraph() bool       { return c.HasModule("graph") }
func (c *Capability) HasGears() bool       { return c.HasModule("gears") }
func (c *Capability) HasVectorSet() bool   { return c.HasModule("vectorset") }
