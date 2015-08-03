// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package kv

import (
	"bytes"
	"fmt"
	"net"
	"reflect"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/gossip"
	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/proto"
	"github.com/cockroachdb/cockroach/rpc"
	"github.com/cockroachdb/cockroach/security"
	"github.com/cockroachdb/cockroach/storage"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/hlc"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/cockroachdb/cockroach/util/retry"
	"github.com/cockroachdb/cockroach/util/tracer"

	gogoproto "github.com/gogo/protobuf/proto"
)

// Default constants for timeouts.
const (
	defaultSendNextTimeout = 500 * time.Millisecond
	defaultRPCTimeout      = 5 * time.Second
	defaultClientTimeout   = 10 * time.Second
	retryBackoff           = 250 * time.Millisecond
	maxRetryBackoff        = 30 * time.Second

	// The default maximum number of ranges to return
	// from an internal range lookup.
	defaultRangeLookupMaxRanges = 8
	// The default size of the leader cache.
	defaultLeaderCacheSize = 1 << 16
	// The default size of the range descriptor cache.
	defaultRangeDescriptorCacheSize = 1 << 20
)

var defaultRPCRetryOptions = retry.Options{
	InitialBackoff: retryBackoff,
	MaxBackoff:     maxRetryBackoff,
	Multiplier:     2,
}

// A firstRangeMissingError indicates that the first range has not yet
// been gossiped. This will be the case for a node which hasn't yet
// joined the gossip network.
type firstRangeMissingError struct{}

// Error implements the error interface.
func (f firstRangeMissingError) Error() string {
	return "the descriptor for the first range is not available via gossip"
}

// CanRetry implements the retry.Retryable interface.
func (f firstRangeMissingError) CanRetry() bool { return true }

// A noNodesAvailError specifies that no node addresses in a replica set
// were available via the gossip network.
type noNodeAddrsAvailError struct{}

// Error implements the error interface.
func (n noNodeAddrsAvailError) Error() string {
	return "no replica node addresses available via gossip"
}

// CanRetry implements the retry.Retryable interface.
func (n noNodeAddrsAvailError) CanRetry() bool { return true }

// A DistSender provides methods to access Cockroach's monolithic,
// distributed key value store. Each method invocation triggers a
// lookup or lookups to find replica metadata for implicated key
// ranges. RPCs are sent to one or more of the replicas to satisfy
// the method invocation.
type DistSender struct {
	// nodeDescriptor, if set, holds the descriptor of the node the
	// DistSender lives on. It should be accessed via getNodeDescriptor(),
	// which tries to obtain the value from the Gossip network if the
	// descriptor is unknown.
	nodeDescriptor unsafe.Pointer
	// clock is used to set time for some calls. E.g. read-only ops
	// which span ranges and don't require read consistency.
	clock *hlc.Clock
	// gossip provides up-to-date information about the start of the
	// key range, used to find the replica metadata for arbitrary key
	// ranges.
	gossip *gossip.Gossip
	// rangeCache caches replica metadata for key ranges.
	rangeCache           *rangeDescriptorCache
	rangeLookupMaxRanges int32
	// leaderCache caches the last known leader replica for range
	// consensus groups.
	leaderCache *leaderCache
	// rpcSend is used to send RPC calls and defaults to rpc.Send
	// outside of tests.
	rpcSend         rpcSendFn
	rpcRetryOptions retry.Options
}

var _ client.Sender = &DistSender{}

// rpcSendFn is the function type used to dispatch RPC calls.
type rpcSendFn func(rpc.Options, string, []net.Addr,
	func(addr net.Addr) gogoproto.Message, func() gogoproto.Message,
	*rpc.Context) ([]gogoproto.Message, error)

// DistSenderContext holds auxiliary objects that can be passed to
// NewDistSender.
type DistSenderContext struct {
	Clock                    *hlc.Clock
	RangeDescriptorCacheSize int32
	// RangeLookupMaxRanges sets how many ranges will be prefetched into the
	// range descriptor cache when dispatching a range lookup request.
	RangeLookupMaxRanges int32
	LeaderCacheSize      int32
	RPCRetryOptions      *retry.Options
	// nodeDescriptor, if provided, is used to describe which node the DistSender
	// lives on, for instance when deciding where to send RPCs.
	// Usually it is filled in from the Gossip network on demand.
	nodeDescriptor *proto.NodeDescriptor
	// The RPC dispatcher. Defaults to rpc.Send but can be changed here
	// for testing purposes.
	rpcSend           rpcSendFn
	rangeDescriptorDB rangeDescriptorDB
}

// NewDistSender returns a client.Sender instance which connects to the
// Cockroach cluster via the supplied gossip instance. Supplying a
// DistSenderContext or the fields within is optional. For omitted values, sane
// defaults will be used.
func NewDistSender(ctx *DistSenderContext, gossip *gossip.Gossip) *DistSender {
	if ctx == nil {
		ctx = &DistSenderContext{}
	}
	clock := ctx.Clock
	if clock == nil {
		clock = hlc.NewClock(hlc.UnixNano)
	}
	ds := &DistSender{
		clock:  clock,
		gossip: gossip,
	}
	if ctx.nodeDescriptor != nil {
		atomic.StorePointer(&ds.nodeDescriptor, unsafe.Pointer(ctx.nodeDescriptor))
	}
	rcSize := ctx.RangeDescriptorCacheSize
	if rcSize <= 0 {
		rcSize = defaultRangeDescriptorCacheSize
	}
	rdb := ctx.rangeDescriptorDB
	if rdb == nil {
		rdb = ds
	}
	ds.rangeCache = newRangeDescriptorCache(rdb, int(rcSize))
	lcSize := ctx.LeaderCacheSize
	if lcSize <= 0 {
		lcSize = defaultLeaderCacheSize
	}
	ds.leaderCache = newLeaderCache(int(lcSize))
	if ctx.RangeLookupMaxRanges <= 0 {
		ds.rangeLookupMaxRanges = defaultRangeLookupMaxRanges
	}
	ds.rpcSend = rpc.Send
	if ctx.rpcSend != nil {
		ds.rpcSend = ctx.rpcSend
	}
	ds.rpcRetryOptions = defaultRPCRetryOptions
	if ctx.RPCRetryOptions != nil {
		ds.rpcRetryOptions = *ctx.RPCRetryOptions
	}
	return ds
}

// verifyPermissions verifies that the requesting user (header.User)
// has permission to read/write (capabilities depend on method
// name). In the event that multiple permission configs apply to the
// key range implicated by the command, the lowest common denominator
// for permission. For example, if a scan crosses two permission
// configs, both configs must allow read permissions or the entire
// scan will fail.
func (ds *DistSender) verifyPermissions(args proto.Request) error {
	// The root user can always proceed.
	header := args.Header()
	if header.User == security.RootUser {
		return nil
	}
	// Check for admin methods.
	if proto.IsAdmin(args) {
		if header.User != security.RootUser {
			return util.Errorf("user %q cannot invoke admin command %s", header.User, args.Method())
		}
		return nil
	}
	// Get permissions map from gossip.
	configMap, err := ds.gossip.GetInfo(gossip.KeyConfigPermission)
	if err != nil {
		return util.Errorf("permissions not available via gossip")
	}
	if configMap == nil {
		return util.Errorf("perm configs not available; cannot execute %s", args.Method())
	}
	permMap := configMap.(storage.PrefixConfigMap)
	headerEnd := header.EndKey
	if len(headerEnd) == 0 {
		headerEnd = header.Key
	}
	// Visit PermConfig(s) which apply to the method's key range.
	//   - For each perm config which the range covers, verify read or writes
	//     are allowed as method requires.
	//   - Verify the permissions hierarchically; that is, if permissions aren't
	//     granted at the longest prefix, try next longest, then next, etc., up
	//     to and including the default prefix.
	//
	// TODO(spencer): it might make sense to visit prefixes from the
	//   shortest to longest instead for performance. Keep an eye on profiling
	//   for this code path as permission sets grow large.
	return permMap.VisitPrefixes(header.Key, headerEnd,
		func(start, end proto.Key, config interface{}) (bool, error) {
			hasPerm := false
			if err := permMap.VisitPrefixesHierarchically(start, func(start, end proto.Key, config interface{}) (bool, error) {
				perm := config.(*proto.PermConfig)
				if proto.IsRead(args) && !perm.CanRead(header.User) {
					return false, nil
				}
				if proto.IsWrite(args) && !perm.CanWrite(header.User) {
					return false, nil
				}
				// Return done = true, as permissions have been granted by this config.
				hasPerm = true
				return true, nil
			}); err != nil {
				return false, err
			}
			if !hasPerm {
				if len(header.EndKey) == 0 {
					return false, util.Errorf("user %q cannot invoke %s at %q", header.User, args.Method(), start)
				}
				return false, util.Errorf("user %q cannot invoke %s at %q-%q", header.User, args.Method(), start, end)
			}
			return false, nil
		})
}

// lookupOptions capture additional options to pass to InternalRangeLookup.
type lookupOptions struct {
	ignoreIntents bool
}

// internalRangeLookup dispatches an InternalRangeLookup request for the given
// metadata key to the replicas of the given range. Note that we allow
// inconsistent reads when doing range lookups for efficiency. Getting stale
// data is not a correctness problem but instead may infrequently result in
// additional latency as additional range lookups may be required. Note also
// that internalRangeLookup bypasses the DistSender's Send() method, so there
// is no error inspection and retry logic here; this is not an issue since the
// lookup performs a single inconsistent read only.
func (ds *DistSender) internalRangeLookup(key proto.Key, options lookupOptions,
	desc *proto.RangeDescriptor) ([]proto.RangeDescriptor, error) {
	args := &proto.InternalRangeLookupRequest{
		RequestHeader: proto.RequestHeader{
			Key:             key,
			User:            security.RootUser,
			ReadConsistency: proto.INCONSISTENT,
		},
		MaxRanges:     ds.rangeLookupMaxRanges,
		IgnoreIntents: options.ignoreIntents,
	}
	replicas := newReplicaSlice(ds.gossip, desc)
	// TODO(tschottdorf) consider a Trace here, potentially that of the request
	// that had the cache miss and waits for the result.
	reply, err := ds.sendRPC(nil /* Trace */, desc.RangeID, replicas, rpc.OrderRandom, args)
	if err != nil {
		return nil, err
	}
	rlReply := reply.(*proto.InternalRangeLookupResponse)
	if rlReply.Error != nil {
		return nil, rlReply.GoError()
	}
	return rlReply.Ranges, nil
}

// getFirstRangeDescriptor returns the RangeDescriptor for the first range on
// the cluster, which is retrieved from the gossip protocol instead of the
// datastore.
func (ds *DistSender) getFirstRangeDescriptor() (*proto.RangeDescriptor, error) {
	infoI, err := ds.gossip.GetInfo(gossip.KeyFirstRangeDescriptor)
	if err != nil {
		return nil, firstRangeMissingError{}
	}
	info := infoI.(proto.RangeDescriptor)
	return &info, nil
}

// getRangeDescriptors returns a sorted slice of RangeDescriptors for a set of
// consecutive ranges, the first of which must contain the requested key. The
// additional RangeDescriptors are returned with the intent of pre-caching
// subsequent ranges which are likely to be requested soon by the current
// workload.
func (ds *DistSender) getRangeDescriptors(key proto.Key, options lookupOptions) ([]proto.RangeDescriptor, error) {
	var (
		// metadataKey is sent to internalRangeLookup to find the
		// RangeDescriptor which contains key.
		metadataKey = keys.RangeMetaKey(key)
		// desc is the RangeDescriptor for the range which contains
		// metadataKey.
		desc *proto.RangeDescriptor
		err  error
	)
	if bytes.Equal(metadataKey, proto.KeyMin) {
		// In this case, the requested key is stored in the cluster's first
		// range. Return the first range, which is always gossiped and not
		// queried from the datastore.
		rd, err := ds.getFirstRangeDescriptor()
		if err != nil {
			return nil, err
		}
		return []proto.RangeDescriptor{*rd}, nil
	}
	if bytes.HasPrefix(metadataKey, keys.Meta1Prefix) {
		// In this case, desc is the cluster's first range.
		if desc, err = ds.getFirstRangeDescriptor(); err != nil {
			return nil, err
		}
	} else {
		// Look up desc from the cache, which will recursively call into
		// ds.getRangeDescriptors if it is not cached.
		desc, err = ds.rangeCache.LookupRangeDescriptor(metadataKey, options)
		if err != nil {
			return nil, err
		}
	}

	return ds.internalRangeLookup(metadataKey, options, desc)
}

func (ds *DistSender) optimizeReplicaOrder(replicas replicaSlice) rpc.OrderingPolicy {
	// Unless we know better, send the RPCs randomly.
	order := rpc.OrderingPolicy(rpc.OrderRandom)
	nodeDesc := ds.getNodeDescriptor()
	// If we don't know which node we're on, don't optimize anything.
	if nodeDesc == nil {
		return order
	}
	// Sort replicas by attribute affinity, which we treat as a stand-in for
	// proximity (for now).
	if replicas.SortByCommonAttributePrefix(nodeDesc.Attrs.Attrs) > 0 {
		// There's at least some attribute prefix, and we hope that the
		// replicas that come early in the slice are now located close to
		// us and hence better candidates.
		order = rpc.OrderStable
	}
	// If there is a replica in local node, move it to the front.
	if i := replicas.FindReplicaByNodeID(nodeDesc.NodeID); i > 0 {
		replicas.MoveToFront(i)
		order = rpc.OrderStable
	}
	return order
}

// getNodeDescriptor returns ds.nodeDescriptor, but makes an attempt to load
// it from the Gossip network if a nil value is found.
// We must jump through hoops here to get the node descriptor because it's not available
// until after the node has joined the gossip network and been allowed to initialize
// its stores.
func (ds *DistSender) getNodeDescriptor() *proto.NodeDescriptor {
	if desc := atomic.LoadPointer(&ds.nodeDescriptor); desc != nil {
		return (*proto.NodeDescriptor)(desc)
	}

	ownNodeID := ds.gossip.GetNodeID()
	if ownNodeID > 0 {
		nodeDesc, err := ds.gossip.GetInfo(gossip.MakeNodeIDKey(ownNodeID))
		if err == nil {
			d := nodeDesc.(*proto.NodeDescriptor)
			atomic.StorePointer(&ds.nodeDescriptor, unsafe.Pointer(d))
			return d
		}
	}
	log.Infof("unable to determine this node's attributes for replica " +
		"selection; node is most likely bootstrapping")
	return nil
}

// sendRPC sends one or more RPCs to replicas from the supplied proto.Replica
// slice. First, replicas which have gossiped addresses are corralled (and
// rearranged depending on proximity and whether the request needs to go to a
// leader) and then sent via rpc.Send, with requirement that one RPC to a
// server must succeed. Returns an RPC error if the request could not be sent.
// Note that the reply may contain a higher level error and must be checked in
// addition to the RPC error.
func (ds *DistSender) sendRPC(trace *tracer.Trace, raftID proto.RangeID, replicas replicaSlice, order rpc.OrderingPolicy,
	args proto.Request) (proto.Response, error) {
	if len(replicas) == 0 {
		return nil, util.Errorf("%s: replicas set is empty", args.Method())
	}

	// Build a slice of replica addresses (if gossiped).
	var addrs []net.Addr
	replicaMap := map[string]*proto.Replica{}
	for i := range replicas {
		nd := &replicas[i].NodeDesc
		addr := util.MakeUnresolvedAddr(nd.Address.Network, nd.Address.Address)
		addrs = append(addrs, addr)
		replicaMap[addr.String()] = &replicas[i].Replica
	}
	if len(addrs) == 0 {
		return nil, noNodeAddrsAvailError{}
	}

	// TODO(pmattis): This needs to be tested. If it isn't set we'll
	// still route the request appropriately by key, but won't receive
	// RangeNotFoundErrors.
	args.Header().RaftID = raftID

	// Set RPC opts with stipulation that one of N RPCs must succeed.
	rpcOpts := rpc.Options{
		N:               1,
		Ordering:        order,
		SendNextTimeout: defaultSendNextTimeout,
		Timeout:         defaultRPCTimeout,
		Trace:           trace,
	}
	// getArgs clones the arguments on demand for all but the first replica.
	firstArgs := true
	getArgs := func(addr net.Addr) gogoproto.Message {
		var a proto.Request
		// Use the supplied args proto if this is our first address.
		if firstArgs {
			firstArgs = false
			a = args
		} else {
			// Otherwise, copy the args value and set the replica in the header.
			a = gogoproto.Clone(args).(proto.Request)
		}
		a.Header().Replica = *replicaMap[addr.String()]
		return a
	}
	// RPCs are sent asynchronously and there is no synchronized access to
	// the reply object, so we don't pass itself to rpcSend.
	// Otherwise there maybe a race case:
	// If the RPC call times out using our original reply object,
	// we must not use it any more; the rpc call might still return
	// and just write to it at any time.
	// args.CreateReply() should be cheaper than gogoproto.Clone which use reflect.
	getReply := func() gogoproto.Message {
		return args.CreateReply()
	}

	replies, err := ds.rpcSend(rpcOpts, "Node."+args.Method().String(),
		addrs, getArgs, getReply, ds.gossip.RPCContext)
	if err != nil {
		return nil, err
	}
	return replies[0].(proto.Response), nil
}

// getDescriptors takes a call and looks up the corresponding range
// descriptors associated with it. First, the range descriptor for
// call.Args.Key is looked up. If call.Args.EndKey exceeds that of the
// returned descriptor, the next descriptor is obtained as well.
func (ds *DistSender) getDescriptors(call proto.Call) (*proto.RangeDescriptor, *proto.RangeDescriptor, error) {
	// If this is an InternalPushTxn, set ignoreIntents option as
	// necessary. This prevents a potential infinite loop; see the
	// comments in proto.InternalRangeLookupRequest.
	options := lookupOptions{}
	if pushArgs, ok := call.Args.(*proto.InternalPushTxnRequest); ok {
		options.ignoreIntents = pushArgs.RangeLookup
	}

	desc, err := ds.rangeCache.LookupRangeDescriptor(call.Args.Header().Key, options)
	if err != nil {
		return nil, nil, err
	}

	var descNext *proto.RangeDescriptor
	// If the request accesses keys beyond the end of this range,
	// get the descriptor of the adjacent range to address next.
	if desc.EndKey.Less(call.Args.Header().EndKey) {
		if _, ok := call.Reply.(proto.Combinable); !ok {
			return nil, nil, util.Error("illegal cross-range operation")
		}
		// If there's no transaction and op spans ranges, possibly
		// re-run as part of a transaction for consistency. The
		// case where we don't need to re-run is if the read
		// consistency is not required.
		if call.Args.Header().Txn == nil &&
			call.Args.Header().ReadConsistency != proto.INCONSISTENT {
			return nil, nil, &proto.OpRequiresTxnError{}
		}
		// This next lookup is likely for free since we've read the
		// previous descriptor and range lookups use cache
		// prefetching.
		descNext, err = ds.rangeCache.LookupRangeDescriptor(desc.EndKey, options)
		if err != nil {
			return nil, nil, err
		}
	}
	return desc, descNext, nil
}

// sendAttempt is invoked by Send. It temporarily truncates the arguments to
// match the descriptor's EndKey (if necessary) and gathers and rearranges the
// replicas before making a single attempt at sending the request. It returns
// the result of sending the RPC; a potential error contained in the reply has
// to be handled separately by the caller.
func (ds *DistSender) sendAttempt(trace *tracer.Trace, args proto.Request, desc *proto.RangeDescriptor) (proto.Response, error) {
	defer trace.Epoch("sending RPC")()
	// Truncate the request to our current range, making sure not to
	// touch it unless we have to (it is illegal to send EndKey on
	// commands which do not operate on ranges).
	if endKey := args.Header().EndKey; endKey != nil && !endKey.Less(desc.EndKey) {
		defer func(k proto.Key) { args.Header().EndKey = k }(endKey)
		args.Header().EndKey = desc.EndKey
	}
	leader := ds.leaderCache.Lookup(proto.RangeID(desc.RangeID))

	// Try to send the call.
	replicas := newReplicaSlice(ds.gossip, desc)

	// Rearrange the replicas so that those replicas with long common
	// prefix of attributes end up first. If there's no prefix, this is a
	// no-op.
	order := ds.optimizeReplicaOrder(replicas)

	// If this request needs to go to a leader and we know who that is, move
	// it to the front.
	if !(proto.IsRead(args) && args.Header().ReadConsistency == proto.INCONSISTENT) &&
		leader.StoreID > 0 {
		if i := replicas.FindReplica(leader.StoreID); i >= 0 {
			replicas.MoveToFront(i)
			order = rpc.OrderStable
		}
	}

	return ds.sendRPC(trace, desc.RangeID, replicas, order, args)
}

// Send implements the client.Sender interface. It verifies
// permissions and looks up the appropriate range based on the
// supplied key and sends the RPC according to the specified options.
//
// If the request spans multiple ranges (which is possible for Scan or
// DeleteRange requests), Send sends requests to the individual ranges
// sequentially and combines the results transparently.
//
// This may temporarily adjust the request headers, so the proto.Call
// must not be used concurrently until Send has returned.
func (ds *DistSender) Send(ctx context.Context, call proto.Call) {
	args := call.Args

	// Verify permissions.
	if err := ds.verifyPermissions(call.Args); err != nil {
		call.Reply.Header().SetGoError(err)
		return
	}

	trace := tracer.FromCtx(ctx)

	// In the event that timestamp isn't set and read consistency isn't
	// required, set the timestamp using the local clock.
	if args.Header().ReadConsistency == proto.INCONSISTENT && args.Header().Timestamp.Equal(proto.ZeroTimestamp) {
		// Make sure that after the call, args hasn't changed.
		defer func(timestamp proto.Timestamp) {
			args.Header().Timestamp = timestamp
		}(args.Header().Timestamp)
		args.Header().Timestamp = ds.clock.Now()
	}

	// If this is a bounded request, we will change its bound as we receive
	// replies. This undoes that when we return.
	boundedArgs, argsBounded := args.(proto.Bounded)

	if argsBounded {
		defer func(bound int64) {
			boundedArgs.SetBound(bound)
		}(boundedArgs.GetBound())
	}

	defer func(key proto.Key) {
		args.Header().Key = key
	}(args.Header().Key)

	first := true

	// Retry logic for lookup of range by key and RPCs to range replicas.
	for {
		var curReply proto.Response
		var desc, descNext *proto.RangeDescriptor
		var err error
		for r := retry.Start(ds.rpcRetryOptions); r.Next(); {
			// Get range descriptor (or, when spanning range, descriptors). Our
			// error handling below may clear them on certain errors, so we
			// refresh (likely from the cache) on every retry.
			descDone := trace.Epoch("meta descriptor lookup")
			// It is safe to pass call here (with its embedded reply) because
			// the reply is only used to check that it implements
			// proto.Combinable if the request spans multiple ranges.
			desc, descNext, err = ds.getDescriptors(call)
			descDone()
			// getDescriptors may fail retryably if the first range isn't
			// available via Gossip.
			if err != nil {
				if rErr, ok := err.(retry.Retryable); ok && rErr.CanRetry() {
					if log.V(1) {
						log.Warning(err)
					}
					continue
				}
				break
			}
			// At this point reply.Header().Error may be non-nil!
			curReply, err = ds.sendAttempt(trace, args, desc)
			if err != nil {
				trace.Event(fmt.Sprintf("send error: %T", err))
				// For an RPC error to occur, we must've been unable to contact any
				// replicas. In this case, likely all nodes are down (or not getting back
				// to us within a reasonable amount of time).
				// We may simply not be trying to talk to the up-to-date replicas, so
				// clearing the descriptor here should be a good idea.
				// TODO(tschottdorf): If a replica group goes dead, this will cause clients
				// to put high read pressure on the first range, so there should be some
				// rate limiting here.
				ds.rangeCache.EvictCachedRangeDescriptor(args.Header().Key, desc)
			} else {
				err = curReply.Header().GoError()
			}

			if err == nil {
				break
			}

			if log.V(1) {
				log.Warningf("failed to invoke %s: %s", call.Method(), err)
			}

			// If retryable, allow retry. For range not found or range
			// key mismatch errors, we don't backoff on the retry,
			// but reset the backoff loop so we can retry immediately.
			switch tErr := err.(type) {
			case *proto.RangeNotFoundError, *proto.RangeKeyMismatchError:
				trace.Event(fmt.Sprintf("reply error: %T", err))
				// Range descriptor might be out of date - evict it.
				ds.rangeCache.EvictCachedRangeDescriptor(args.Header().Key, desc)
				// On addressing errors, don't backoff; retry immediately.
				r.Reset()
				if log.V(1) {
					log.Warning(err)
				}
				continue
			case *proto.NotLeaderError:
				trace.Event(fmt.Sprintf("reply error: %T", err))
				newLeader := tErr.GetLeader()
				// Verify that leader is a known replica according to the
				// descriptor. If not, we've got a stale replica; evict cache.
				// Next, cache the new leader.
				if newLeader != nil {
					if i, _ := desc.FindReplica(newLeader.StoreID); i == -1 {
						if log.V(1) {
							log.Infof("error indicates unknown leader %s, expunging descriptor %s", newLeader, desc)
						}
						ds.rangeCache.EvictCachedRangeDescriptor(args.Header().Key, desc)
					}
				} else {
					newLeader = &proto.Replica{}
				}
				ds.updateLeaderCache(proto.RangeID(desc.RangeID), *newLeader)
				if log.V(1) {
					log.Warning(err)
				}
				r.Reset()
				continue
			case retry.Retryable:
				if tErr.CanRetry() {
					if log.V(1) {
						log.Warning(err)
					}
					trace.Event(fmt.Sprintf("reply error: %T", err))
					continue
				}
			}
			break
		}

		// Immediately return if querying a range failed non-retryably.
		// For multi-range requests, we return the failing range's reply.
		if err != nil {
			call.Reply.Header().SetGoError(err)
			return
		}

		if first {
			// Equivalent of `*call.Reply = curReply`. Generics!
			dst := reflect.ValueOf(call.Reply).Elem()
			dst.Set(reflect.ValueOf(curReply).Elem())
		} else {
			// This was the second or later call in a multi-range request.
			// Combine the new response with the existing one.
			if cReply, ok := call.Reply.(proto.Combinable); ok {
				cReply.Combine(curReply)
			} else {
				// This should never apply in practice, as we'll only end up here
				// for range-spanning requests.
				call.Reply.Header().SetGoError(util.Errorf("multi-range request with non-combinable response type"))
				return
			}
		}

		first = false

		// If this request has a bound, such as MaxResults in
		// ScanRequest, check whether enough rows have been retrieved.
		if argsBounded {
			if prevBound := boundedArgs.GetBound(); prevBound > 0 {
				if cReply, ok := curReply.(proto.Countable); ok {
					if nextBound := prevBound - cReply.Count(); nextBound > 0 {
						// Update bound for the next round.
						// We've deferred restoring the original bound earlier.
						boundedArgs.SetBound(nextBound)
					} else {
						// Set flag to break the loop.
						descNext = nil
					}
				}
			}
		}

		// If this was the last range accessed by this call, exit loop.
		if descNext == nil {
			break
		}

		// In next iteration, query next range.
		// It's important that we use the EndKey of the current descriptor
		// as opposed to the StartKey of the next one: if the former is stale,
		// it's possible that the next range has since merged the subsequent
		// one, and unless both descriptors are stale, the next descriptor's
		// StartKey would move us to the beginning of the current range,
		// resulting in a duplicate scan.
		args.Header().Key = desc.EndKey
		trace.Event("querying next range")
	}
}

// updateLeaderCache updates the cached leader for the given Raft group,
// evicting any previous value in the process.
func (ds *DistSender) updateLeaderCache(rid proto.RangeID, leader proto.Replica) {
	oldLeader := ds.leaderCache.Lookup(rid)
	if leader.StoreID != oldLeader.StoreID {
		if log.V(1) {
			log.Infof("raft %d: new cached leader store %d (old: %d)", rid, leader.StoreID, oldLeader.StoreID)
		}
		ds.leaderCache.Update(rid, leader)
	}
}
