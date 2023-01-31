package libp2p

import (
	"fmt"

	"github.com/dustin/go-humanize"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/pbnjay/memory"

	"github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core/node/libp2p/fd"
)

var infiniteResourceLimit = &rcmgr.ResourceLimits{
	Streams:         rcmgr.Unlimited,
	StreamsInbound:  rcmgr.Unlimited,
	StreamsOutbound: rcmgr.Unlimited,
	Conns:           rcmgr.Unlimited,
	ConnsInbound:    rcmgr.Unlimited,
	ConnsOutbound:   rcmgr.Unlimited,
	FD:              rcmgr.Unlimited,
	Memory:          rcmgr.Unlimited64,
}

// This file defines implicit limit defaults used when Swarm.ResourceMgr.Enabled

// createDefaultLimitConfig creates LimitConfig to pass to libp2p's resource manager.
// The defaults follow the documentation in docs/libp2p-resource-management.md.
// Any changes in the logic here should be reflected there.
func createDefaultLimitConfig(cfg config.SwarmConfig, applyConfigLimits bool) (rcmgr.ConcreteLimitConfig, error) {
	maxMemoryDefaultString := humanize.Bytes(uint64(memory.TotalMemory()) / 2)
	maxMemoryString := cfg.ResourceMgr.MaxMemory.WithDefault(maxMemoryDefaultString)
	maxMemory, err := humanize.ParseBytes(maxMemoryString)
	if err != nil {
		return rcmgr.ConcreteLimitConfig{}, err
	}

	maxMemoryMB := maxMemory / (1024 * 1024)
	maxFD := int(cfg.ResourceMgr.MaxFileDescriptors.WithDefault(int64(fd.GetNumFDs()) / 2))

	// We want to see this message on startup, that's why we are using fmt instead of log.
	fmt.Printf(`
Computing default go-libp2p Resource Manager limits based on:
    - 'Swarm.ResourceMgr.MaxMemory': %q
    - 'Swarm.ResourceMgr.MaxFileDescriptors': %d

Applying any user-supplied overrides on top.
Run 'ipfs swarm limit all' to see the resulting limits.

`, maxMemoryString, maxFD)

	// At least as of 2023-01-25, it's possible to open a connection that
	// doesn't ask for any memory usage with the libp2p Resource Manager/Accountant
	// (see https://github.com/libp2p/go-libp2p/issues/2010#issuecomment-1404280736).
	// As a result, we can't curretly rely on Memory limits to full protect us.
	// Until https://github.com/libp2p/go-libp2p/issues/2010 is addressed,
	// we take a proxy now of restricting to 1 inbound connection per MB.
	// Note: this is more generous than go-libp2p's default autoscaled limits which do
	// 64 connections per 1GB
	// (see https://github.com/libp2p/go-libp2p/blob/master/p2p/host/resource-manager/limit_defaults.go#L357 ).
	systemConnsInbound := int(1 * maxMemoryMB)

	partialLimits := rcmgr.PartialLimitConfig{
		System: &rcmgr.ResourceLimits{
			Memory: rcmgr.LimitVal64(maxMemory),
			FD:     rcmgr.LimitVal(maxFD),

			Conns:         rcmgr.Unlimited,
			ConnsInbound:  rcmgr.LimitVal(systemConnsInbound),
			ConnsOutbound: rcmgr.Unlimited,

			Streams:         rcmgr.Unlimited,
			StreamsOutbound: rcmgr.Unlimited,
			StreamsInbound:  rcmgr.Unlimited,
		},

		// Transient connections won't cause any memory to accounted for by the resource manager.
		// Only established connections do.
		// As a result, we can't rely on System.Memory to protect us from a bunch of transient connection being opened.
		// We limit the same values as the System scope, but only allow the Transient scope to take 25% of what is allowed for the System scope.
		Transient: &rcmgr.ResourceLimits{
			Memory: rcmgr.LimitVal64(maxMemory / 4),
			FD:     rcmgr.LimitVal(maxFD / 4),

			Conns:         rcmgr.Unlimited,
			ConnsInbound:  rcmgr.LimitVal(systemConnsInbound / 4),
			ConnsOutbound: rcmgr.Unlimited,

			Streams:         rcmgr.Unlimited,
			StreamsInbound:  rcmgr.Unlimited,
			StreamsOutbound: rcmgr.Unlimited,
		},

		// Lets get out of the way of the allow list functionality.
		// If someone specified "Swarm.ResourceMgr.Allowlist" we should let it go through.
		AllowlistedSystem: infiniteResourceLimit,

		AllowlistedTransient: infiniteResourceLimit,

		// Keep it simple by not having Service, ServicePeer, Protocol, ProtocolPeer, Conn, or Stream limits.
		ServiceDefault: infiniteResourceLimit,

		ServicePeerDefault: infiniteResourceLimit,

		ProtocolDefault: infiniteResourceLimit,

		ProtocolPeerDefault: infiniteResourceLimit,

		Conn: infiniteResourceLimit,

		Stream: infiniteResourceLimit,

		// Limit the resources consumed by a peer.
		// This doesn't protect us against intentional DoS attacks since an attacker can easily spin up multiple peers.
		// We specify this limit against unintentional DoS attacks (e.g., a peer has a bug and is sending too much traffic intentionally).
		// In that case we want to keep that peer's resource consumption contained.
		// To keep this simple, we only constrain inbound connections and streams.
		PeerDefault: &rcmgr.ResourceLimits{
			Memory:          rcmgr.Unlimited64,
			FD:              rcmgr.Unlimited,
			Conns:           rcmgr.Unlimited,
			ConnsInbound:    rcmgr.DefaultLimit,
			ConnsOutbound:   rcmgr.Unlimited,
			Streams:         rcmgr.Unlimited,
			StreamsInbound:  rcmgr.DefaultLimit,
			StreamsOutbound: rcmgr.Unlimited,
		},
	}

	// Simple checks to overide autoscaling ensuring limits make sense versus the connmgr values.
	// There are ways to break this, but this should catch most problems already.
	// We might improve this in the future.
	// See: https://github.com/ipfs/kubo/issues/9545
	if cfg.ConnMgr.Type.WithDefault(config.DefaultConnMgrType) != "none" {
		maxInboundConns := int64(partialLimits.System.ConnsInbound)
		if connmgrHighWaterTimesTwo := cfg.ConnMgr.HighWater.WithDefault(config.DefaultConnMgrHighWater) * 2; maxInboundConns < connmgrHighWaterTimesTwo {
			maxInboundConns = connmgrHighWaterTimesTwo
		}

		if maxInboundConns < config.DefaultResourceMgrMinInboundConns {
			maxInboundConns = config.DefaultResourceMgrMinInboundConns
		}

		// Scale System.StreamsInbound as well, but use the existing ratio of StreamsInbound to ConnsInbound
		partialLimits.System.StreamsInbound = rcmgr.LimitVal(maxInboundConns * int64(partialLimits.System.StreamsInbound) / int64(partialLimits.System.ConnsInbound))
		partialLimits.System.ConnsInbound = rcmgr.LimitVal(maxInboundConns)
	}

	limitConfig := partialLimits

	// The logic for defaults and overriding with specified SwarmConfig.ResourceMgr.Limits
	// is documented in docs/config.md.
	// Any changes here should be reflected there.
	if cfg.ResourceMgr.Limits != nil && applyConfigLimits {
		userSuppliedOverrideLimitConfig := *cfg.ResourceMgr.Limits
		// This effectively overrides the computed default LimitConfig with any non-zero values from cfg.ResourceMgr.Limits.
		// Because of how how Apply works, any 0 value for a user supplied override
		// will be overriden with a computed default value.
		// There currently isn't a way for a user to supply a 0-value override.
		userSuppliedOverrideLimitConfig.Apply(partialLimits)
		limitConfig = userSuppliedOverrideLimitConfig
	}

	return limitConfig.Build(rcmgr.DefaultLimits.Scale(int64(maxMemory), maxFD)), nil
}
