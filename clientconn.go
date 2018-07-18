/*
 *
 * Copyright 2014 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpc

import (
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"sync/atomic"

	"golang.org/x/net/context"
	"golang.org/x/net/trace"
	"google.golang.org/grpc/balancer"
	_ "google.golang.org/grpc/balancer/roundrobin" // To register roundrobin.
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
	_ "google.golang.org/grpc/resolver/dns"         // To register dns resolver.
	_ "google.golang.org/grpc/resolver/passthrough" // To register passthrough resolver.
	"google.golang.org/grpc/status"
)

const (
	// minimum time to give a connection to complete
	minConnectTimeout = 20 * time.Second
	// must match grpclbName in grpclb/grpclb.go
	grpclbName = "grpclb"
)

var (
	// ErrClientConnClosing indicates that the operation is illegal because
	// the ClientConn is closing.
	//
	// Deprecated: this error should not be relied upon by users; use the status
	// code of Canceled instead.
	ErrClientConnClosing = status.Error(codes.Canceled, "grpc: the client connection is closing")
	// errConnDrain indicates that the connection starts to be drained and does not accept any new RPCs.
	errConnDrain = errors.New("grpc: the connection is drained")
	// errConnClosing indicates that the connection is closing.
	errConnClosing = errors.New("grpc: the connection is closing")
	// errConnUnavailable indicates that the connection is unavailable.
	errConnUnavailable = errors.New("grpc: the connection is unavailable")
	// errBalancerClosed indicates that the balancer is closed.
	errBalancerClosed = errors.New("grpc: balancer is closed")
	// We use an accessor so that minConnectTimeout can be
	// atomically read and updated while testing.
	getMinConnectTimeout = func() time.Duration {
		return minConnectTimeout
	}
)

// The following errors are returned from Dial and DialContext
var (
	// errNoTransportSecurity indicates that there is no transport security
	// being set for ClientConn. Users should either set one or explicitly
	// call WithInsecure DialOption to disable security.
	errNoTransportSecurity = errors.New("grpc: no transport security set (use grpc.WithInsecure() explicitly or set credentials)")
	// errTransportCredentialsMissing indicates that users want to transmit security
	// information (e.g., oauth2 token) which requires secure connection on an insecure
	// connection.
	errTransportCredentialsMissing = errors.New("grpc: the credentials require transport level security (use grpc.WithTransportCredentials() to set)")
	// errCredentialsConflict indicates that grpc.WithTransportCredentials()
	// and grpc.WithInsecure() are both called for a connection.
	errCredentialsConflict = errors.New("grpc: transport credentials are set for an insecure connection (grpc.WithTransportCredentials() and grpc.WithInsecure() are both called)")
	// errNetworkIO indicates that the connection is down due to some network I/O error.
	errNetworkIO = errors.New("grpc: failed with network I/O error")
)

const (
	defaultClientMaxReceiveMessageSize = 1024 * 1024 * 4
	defaultClientMaxSendMessageSize    = math.MaxInt32
	// http2IOBufSize specifies the buffer size for sending frames.
	defaultWriteBufSize = 32 * 1024
	defaultReadBufSize  = 32 * 1024
)

// RegisterChannelz turns on channelz service.
// This is an EXPERIMENTAL API.
func RegisterChannelz() {
	channelz.TurnOn()
}

// Dial creates a client connection to the given target.
func Dial(target string, opts ...DialOption) (*ClientConn, error) {
	return DialContext(context.Background(), target, opts...)
}

// DialContext creates a client connection to the given target. By default, it's
// a non-blocking dial (the function won't wait for connections to be
// established, and connecting happens in the background). To make it a blocking
// dial, use WithBlock() dial option.
//
// In the non-blocking case, the ctx does not act against the connection. It
// only controls the setup steps.
//
// In the blocking case, ctx can be used to cancel or expire the pending
// connection. Once this function returns, the cancellation and expiration of
// ctx will be noop. Users should call ClientConn.Close to terminate all the
// pending operations after this function returns.
//
// The target name syntax is defined in
// https://github.com/grpc/grpc/blob/master/doc/naming.md.
// e.g. to use dns resolver, a "dns:///" prefix should be applied to the target.
func DialContext(ctx context.Context, target string, opts ...DialOption) (conn *ClientConn, err error) {
	cc := &ClientConn{
		target:         target,
		csMgr:          &connectivityStateManager{},
		conns:          make(map[*addrConn]struct{}),
		dopts:          defaultDialOptions(),
		blockingpicker: newPickerWrapper(),
	}
	cc.retryThrottler.Store((*retryThrottler)(nil))
	cc.ctx, cc.cancel = context.WithCancel(context.Background())

	for _, opt := range opts {
		opt.apply(&cc.dopts)
	}

	if channelz.IsOn() {
		if cc.dopts.channelzParentID != 0 {
			cc.channelzID = channelz.RegisterChannel(cc, cc.dopts.channelzParentID, target)
		} else {
			cc.channelzID = channelz.RegisterChannel(cc, 0, target)
		}
	}

	if !cc.dopts.insecure {
		if cc.dopts.copts.TransportCredentials == nil {
			return nil, errNoTransportSecurity
		}
	} else {
		if cc.dopts.copts.TransportCredentials != nil {
			return nil, errCredentialsConflict
		}
		for _, cd := range cc.dopts.copts.PerRPCCredentials {
			if cd.RequireTransportSecurity() {
				return nil, errTransportCredentialsMissing
			}
		}
	}

	cc.mkp = cc.dopts.copts.KeepaliveParams

	if cc.dopts.copts.Dialer == nil {
		cc.dopts.copts.Dialer = newProxyDialer(
			func(ctx context.Context, addr string) (net.Conn, error) {
				network, addr := parseDialTarget(addr)
				return dialContext(ctx, network, addr)
			},
		)
	}

	if cc.dopts.copts.UserAgent != "" {
		cc.dopts.copts.UserAgent += " " + grpcUA
	} else {
		cc.dopts.copts.UserAgent = grpcUA
	}

	if cc.dopts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cc.dopts.timeout)
		defer cancel()
	}

	defer func() {
		select {
		case <-ctx.Done():
			conn, err = nil, ctx.Err()
		default:
		}

		if err != nil {
			cc.Close()
		}
	}()

	scSet := false
	if cc.dopts.scChan != nil {
		// Try to get an initial service config.
		select {
		case sc, ok := <-cc.dopts.scChan:
			if ok {
				cc.sc = sc
				scSet = true
			}
		default:
		}
	}
	if cc.dopts.bs == nil {
		cc.dopts.bs = backoff.Exponential{
			MaxDelay: DefaultBackoffConfig.MaxDelay,
		}
	}
	if cc.dopts.resolverBuilder == nil {
		// Only try to parse target when resolver builder is not already set.
		cc.parsedTarget = parseTarget(cc.target)
		grpclog.Infof("parsed scheme: %q", cc.parsedTarget.Scheme)
		cc.dopts.resolverBuilder = resolver.Get(cc.parsedTarget.Scheme)
		if cc.dopts.resolverBuilder == nil {
			// If resolver builder is still nil, the parse target's scheme is
			// not registered. Fallback to default resolver and set Endpoint to
			// the original unparsed target.
			grpclog.Infof("scheme %q not registered, fallback to default scheme", cc.parsedTarget.Scheme)
			cc.parsedTarget = resolver.Target{
				Scheme:   resolver.GetDefaultScheme(),
				Endpoint: target,
			}
			cc.dopts.resolverBuilder = resolver.Get(cc.parsedTarget.Scheme)
		}
	} else {
		cc.parsedTarget = resolver.Target{Endpoint: target}
	}
	creds := cc.dopts.copts.TransportCredentials
	if creds != nil && creds.Info().ServerName != "" {
		cc.authority = creds.Info().ServerName
	} else if cc.dopts.insecure && cc.dopts.authority != "" {
		cc.authority = cc.dopts.authority
	} else {
		// Use endpoint from "scheme://authority/endpoint" as the default
		// authority for ClientConn.
		cc.authority = cc.parsedTarget.Endpoint
	}

	if cc.dopts.scChan != nil && !scSet {
		// Blocking wait for the initial service config.
		select {
		case sc, ok := <-cc.dopts.scChan:
			if ok {
				cc.sc = sc
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if cc.dopts.scChan != nil {
		go cc.scWatcher()
	}

	var credsClone credentials.TransportCredentials
	if creds := cc.dopts.copts.TransportCredentials; creds != nil {
		credsClone = creds.Clone()
	}
	cc.balancerBuildOpts = balancer.BuildOptions{
		DialCreds:        credsClone,
		Dialer:           cc.dopts.copts.Dialer,
		ChannelzParentID: cc.channelzID,
	}

	// Build the resolver.
	cc.resolverWrapper, err = newCCResolverWrapper(cc)
	if err != nil {
		return nil, fmt.Errorf("failed to build resolver: %v", err)
	}
	// Start the resolver wrapper goroutine after resolverWrapper is created.
	//
	// If the goroutine is started before resolverWrapper is ready, the
	// following may happen: The goroutine sends updates to cc. cc forwards
	// those to balancer. Balancer creates new addrConn. addrConn fails to
	// connect, and calls resolveNow(). resolveNow() tries to use the non-ready
	// resolverWrapper.
	cc.resolverWrapper.start()

	// A blocking dial blocks until the clientConn is ready.
	if cc.dopts.block {
		for {
			s := cc.GetState()
			if s == connectivity.Ready {
				break
			}
			if !cc.WaitForStateChange(ctx, s) {
				// ctx got timeout or canceled.
				return nil, ctx.Err()
			}
		}
	}

	return cc, nil
}

// connectivityStateManager keeps the connectivity.State of ClientConn.
// This struct will eventually be exported so the balancers can access it.
type connectivityStateManager struct {
	mu         sync.Mutex
	state      connectivity.State
	notifyChan chan struct{}
}

// updateState updates the connectivity.State of ClientConn.
// If there's a change it notifies goroutines waiting on state change to
// happen.
func (csm *connectivityStateManager) updateState(state connectivity.State) {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.state == connectivity.Shutdown {
		return
	}
	if csm.state == state {
		return
	}
	csm.state = state
	if csm.notifyChan != nil {
		// There are other goroutines waiting on this channel.
		close(csm.notifyChan)
		csm.notifyChan = nil
	}
}

func (csm *connectivityStateManager) getState() connectivity.State {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	return csm.state
}

func (csm *connectivityStateManager) getNotifyChan() <-chan struct{} {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.notifyChan == nil {
		csm.notifyChan = make(chan struct{})
	}
	return csm.notifyChan
}

// ClientConn represents a client connection to an RPC server.
type ClientConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	target       string
	parsedTarget resolver.Target
	authority    string
	dopts        dialOptions
	csMgr        *connectivityStateManager

	balancerBuildOpts balancer.BuildOptions
	resolverWrapper   *ccResolverWrapper
	blockingpicker    *pickerWrapper

	mu    sync.RWMutex
	sc    ServiceConfig
	scRaw string
	conns map[*addrConn]struct{}
	// Keepalive parameter can be updated if a GoAway is received.
	mkp             keepalive.ClientParameters
	curBalancerName string
	preBalancerName string // previous balancer name.
	curAddresses    []resolver.Address
	balancerWrapper *ccBalancerWrapper
	retryThrottler  atomic.Value

	channelzID          int64 // channelz unique identification number
	czmu                sync.RWMutex
	callsStarted        int64
	callsSucceeded      int64
	callsFailed         int64
	lastCallStartedTime time.Time
}

// WaitForStateChange waits until the connectivity.State of ClientConn changes from sourceState or
// ctx expires. A true value is returned in former case and false in latter.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) WaitForStateChange(ctx context.Context, sourceState connectivity.State) bool {
	ch := cc.csMgr.getNotifyChan()
	if cc.csMgr.getState() != sourceState {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}

// GetState returns the connectivity.State of ClientConn.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) GetState() connectivity.State {
	return cc.csMgr.getState()
}

func (cc *ClientConn) scWatcher() {
	for {
		select {
		case sc, ok := <-cc.dopts.scChan:
			if !ok {
				return
			}
			cc.mu.Lock()
			// TODO: load balance policy runtime change is ignored.
			// We may revist this decision in the future.
			cc.sc = sc
			cc.scRaw = ""
			cc.mu.Unlock()
		case <-cc.ctx.Done():
			return
		}
	}
}

func (cc *ClientConn) handleResolvedAddrs(addrs []resolver.Address, err error) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.conns == nil {
		// cc was closed.
		return
	}

	if reflect.DeepEqual(cc.curAddresses, addrs) {
		return
	}

	cc.curAddresses = addrs

	if cc.dopts.balancerBuilder == nil {
		// Only look at balancer types and switch balancer if balancer dial
		// option is not set.
		var isGRPCLB bool
		for _, a := range addrs {
			if a.Type == resolver.GRPCLB {
				isGRPCLB = true
				break
			}
		}
		var newBalancerName string
		if isGRPCLB {
			newBalancerName = grpclbName
		} else {
			// Address list doesn't contain grpclb address. Try to pick a
			// non-grpclb balancer.
			newBalancerName = cc.curBalancerName
			// If current balancer is grpclb, switch to the previous one.
			if newBalancerName == grpclbName {
				newBalancerName = cc.preBalancerName
			}
			// The following could be true in two cases:
			// - the first time handling resolved addresses
			//   (curBalancerName="")
			// - the first time handling non-grpclb addresses
			//   (curBalancerName="grpclb", preBalancerName="")
			if newBalancerName == "" {
				newBalancerName = PickFirstBalancerName
			}
		}
		cc.switchBalancer(newBalancerName)
	} else if cc.balancerWrapper == nil {
		// Balancer dial option was set, and this is the first time handling
		// resolved addresses. Build a balancer with dopts.balancerBuilder.
		cc.balancerWrapper = newCCBalancerWrapper(cc, cc.dopts.balancerBuilder, cc.balancerBuildOpts)
	}

	cc.balancerWrapper.handleResolvedAddrs(addrs, nil)
}

// switchBalancer starts the switching from current balancer to the balancer
// with the given name.
//
// It will NOT send the current address list to the new balancer. If needed,
// caller of this function should send address list to the new balancer after
// this function returns.
//
// Caller must hold cc.mu.
func (cc *ClientConn) switchBalancer(name string) {
	if cc.conns == nil {
		return
	}

	if strings.ToLower(cc.curBalancerName) == strings.ToLower(name) {
		return
	}

	grpclog.Infof("ClientConn switching balancer to %q", name)
	if cc.dopts.balancerBuilder != nil {
		grpclog.Infoln("ignoring balancer switching: Balancer DialOption used instead")
		return
	}
	// TODO(bar switching) change this to two steps: drain and close.
	// Keep track of sc in wrapper.
	if cc.balancerWrapper != nil {
		cc.balancerWrapper.close()
	}

	builder := balancer.Get(name)
	if builder == nil {
		grpclog.Infof("failed to get balancer builder for: %v, using pick_first instead", name)
		builder = newPickfirstBuilder()
	}
	cc.preBalancerName = cc.curBalancerName
	cc.curBalancerName = builder.Name()
	cc.balancerWrapper = newCCBalancerWrapper(cc, builder, cc.balancerBuildOpts)
}

func (cc *ClientConn) handleSubConnStateChange(sc balancer.SubConn, s connectivity.State) {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return
	}
	// TODO(bar switching) send updates to all balancer wrappers when balancer
	// gracefully switching is supported.
	cc.balancerWrapper.handleSubConnStateChange(sc, s)
	cc.mu.Unlock()
}

// newAddrConn creates an addrConn for addrs and adds it to cc.conns.
//
// Caller needs to make sure len(addrs) > 0.
func (cc *ClientConn) newAddrConn(addrs []resolver.Address) (*addrConn, error) {
	ac := &addrConn{
		cc:                  cc,
		addrs:               addrs,
		dopts:               cc.dopts,
		transports:          map[int32]transport.ClientTransport{},
		successfulHandshake: true, // make the first nextAddr() call _not_ move addrIdx up by 1
	}
	ac.ctx, ac.cancel = context.WithCancel(cc.ctx)
	// Track ac in cc. This needs to be done before any getTransport(...) is called.
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return nil, ErrClientConnClosing
	}
	if channelz.IsOn() {
		ac.channelzID = channelz.RegisterSubChannel(ac, cc.channelzID, "")
	}
	cc.conns[ac] = struct{}{}
	cc.mu.Unlock()
	return ac, nil
}

// removeAddrConn removes the addrConn in the subConn from clientConn.
// It also tears down the ac with the given error.
func (cc *ClientConn) removeAddrConn(ac *addrConn, err error) {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return
	}
	delete(cc.conns, ac)
	cc.mu.Unlock()
	ac.tearDown(err)
}

// ChannelzMetric returns ChannelInternalMetric of current ClientConn.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) ChannelzMetric() *channelz.ChannelInternalMetric {
	state := cc.GetState()
	cc.czmu.RLock()
	defer cc.czmu.RUnlock()
	return &channelz.ChannelInternalMetric{
		State:                    state,
		Target:                   cc.target,
		CallsStarted:             cc.callsStarted,
		CallsSucceeded:           cc.callsSucceeded,
		CallsFailed:              cc.callsFailed,
		LastCallStartedTimestamp: cc.lastCallStartedTime,
	}
}

// Target returns the target string of the ClientConn.
// This is an EXPERIMENTAL API.
func (cc *ClientConn) Target() string {
	return cc.target
}

func (cc *ClientConn) incrCallsStarted() {
	cc.czmu.Lock()
	cc.callsStarted++
	// TODO(yuxuanli): will make this a time.Time pointer improve performance?
	cc.lastCallStartedTime = time.Now()
	cc.czmu.Unlock()
}

func (cc *ClientConn) incrCallsSucceeded() {
	cc.czmu.Lock()
	cc.callsSucceeded++
	cc.czmu.Unlock()
}

func (cc *ClientConn) incrCallsFailed() {
	cc.czmu.Lock()
	cc.callsFailed++
	cc.czmu.Unlock()
}

// connect starts to creating transport and also starts the transport monitor
// goroutine for this ac.
// It does nothing if the ac is not IDLE.
// TODO(bar) Move this to the addrConn section.
// This was part of resetAddrConn, keep it here to make the diff look clean.
func (ac *addrConn) connect() error {
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return errConnClosing
	}
	if ac.state != connectivity.Idle {
		ac.mu.Unlock()
		return nil
	}
	ac.state = connectivity.Connecting
	ac.cc.handleSubConnStateChange(ac.acbw, ac.state)
	ac.mu.Unlock()

	// Start a goroutine connecting to the server asynchronously.
	go ac.resetTransport(true)
	return nil
}

// tryUpdateAddrs tries to update ac.addrs with the new addresses list.
//
// It checks whether current connected address of ac is in the new addrs list.
//  - If true, it updates ac.addrs and returns true. The ac will keep using
//    the existing connection.
//  - If false, it does nothing and returns false.
func (ac *addrConn) tryUpdateAddrs(addrs []resolver.Address) bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	grpclog.Infof("addrConn: tryUpdateAddrs curAddr: %v, addrs: %v", ac.curAddr, addrs)
	if ac.state == connectivity.Shutdown {
		ac.addrs = addrs
		return true
	}

	var curAddrFound bool
	for _, a := range addrs {
		if reflect.DeepEqual(ac.curAddr, a) {
			curAddrFound = true
			break
		}
	}
	grpclog.Infof("addrConn: tryUpdateAddrs curAddrFound: %v", curAddrFound)
	if curAddrFound {
		ac.addrs = addrs
		ac.addrIdx = 0 // Start reconnecting from beginning in the new list.
	}

	return curAddrFound
}

// GetMethodConfig gets the method config of the input method.
// If there's an exact match for input method (i.e. /service/method), we return
// the corresponding MethodConfig.
// If there isn't an exact match for the input method, we look for the default config
// under the service (i.e /service/). If there is a default MethodConfig for
// the service, we return it.
// Otherwise, we return an empty MethodConfig.
func (cc *ClientConn) GetMethodConfig(method string) MethodConfig {
	// TODO: Avoid the locking here.
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	m, ok := cc.sc.Methods[method]
	if !ok {
		i := strings.LastIndex(method, "/")
		m = cc.sc.Methods[method[:i+1]]
	}
	return m
}

func (cc *ClientConn) getTransport(ctx context.Context, failfast bool, method string) (transport.ClientTransport, func(balancer.DoneInfo), error) {
	t, done, err := cc.blockingpicker.pick(ctx, failfast, balancer.PickOptions{
		FullMethodName: method,
	})
	if err != nil {
		return nil, nil, toRPCErr(err)
	}
	return t, done, nil
}

// handleServiceConfig parses the service config string in JSON format to Go native
// struct ServiceConfig, and store both the struct and the JSON string in ClientConn.
func (cc *ClientConn) handleServiceConfig(js string) error {
	if cc.dopts.disableServiceConfig {
		return nil
	}
	sc, err := parseServiceConfig(js)
	if err != nil {
		return err
	}
	cc.mu.Lock()
	cc.scRaw = js
	cc.sc = sc

	if sc.retryThrottling != nil {
		newThrottler := &retryThrottler{
			tokens: sc.retryThrottling.MaxTokens,
			max:    sc.retryThrottling.MaxTokens,
			thresh: sc.retryThrottling.MaxTokens / 2,
			ratio:  sc.retryThrottling.TokenRatio,
		}
		cc.retryThrottler.Store(newThrottler)
	} else {
		cc.retryThrottler.Store((*retryThrottler)(nil))
	}

	if sc.LB != nil && *sc.LB != grpclbName { // "grpclb" is not a valid balancer option in service config.
		if cc.curBalancerName == grpclbName {
			// If current balancer is grpclb, there's at least one grpclb
			// balancer address in the resolved list. Don't switch the balancer,
			// but change the previous balancer name, so if a new resolved
			// address list doesn't contain grpclb address, balancer will be
			// switched to *sc.LB.
			cc.preBalancerName = *sc.LB
		} else {
			cc.switchBalancer(*sc.LB)
			cc.balancerWrapper.handleResolvedAddrs(cc.curAddresses, nil)
		}
	}

	cc.mu.Unlock()
	return nil
}

func (cc *ClientConn) resolveNow(o resolver.ResolveNowOption) {
	cc.mu.RLock()
	r := cc.resolverWrapper
	cc.mu.RUnlock()
	if r == nil {
		return
	}
	go r.resolveNow(o)
}

// Close tears down the ClientConn and all underlying connections.
func (cc *ClientConn) Close() error {
	defer cc.cancel()

	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return ErrClientConnClosing
	}
	conns := cc.conns
	cc.conns = nil
	cc.csMgr.updateState(connectivity.Shutdown)

	rWrapper := cc.resolverWrapper
	cc.resolverWrapper = nil
	bWrapper := cc.balancerWrapper
	cc.balancerWrapper = nil
	cc.mu.Unlock()

	cc.blockingpicker.close()

	if rWrapper != nil {
		rWrapper.close()
	}
	if bWrapper != nil {
		bWrapper.close()
	}

	for ac := range conns {
		ac.tearDown(ErrClientConnClosing)
	}
	if channelz.IsOn() {
		channelz.RemoveEntry(cc.channelzID)
	}
	return nil
}

// addrConn is a network connection to a given address.
type addrConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	cc     *ClientConn
	dopts  dialOptions
	events trace.EventLog
	acbw   balancer.SubConn

	transportIdx int32
	transports   map[int32]transport.ClientTransport // All transports, including current and orphaned.
	transport    transport.ClientTransport           // The current transport.

	addrIdx int                // The index in addrs list to start reconnecting from.
	curAddr resolver.Address   // The current address.
	addrs   []resolver.Address // All addresses that the resolver resolved to.

	state connectivity.State
	// ready is closed and becomes nil when a new transport is up or failed
	// due to timeout.
	ready chan struct{}

	tearDownErr error // The reason this addrConn is torn down.

	backoffIdx int
	// backoffDeadline is the time until which resetTransport needs to
	// wait before increasing backoffIdx count.
	backoffDeadline time.Time
	// connectDeadline is the time by which all connection
	// negotiations must complete.
	connectDeadline time.Time

	channelzID          int64 // channelz unique identification number
	czmu                sync.RWMutex
	callsStarted        int64
	callsSucceeded      int64
	callsFailed         int64
	lastCallStartedTime time.Time

	successfulHandshake bool
}

// adjustParams updates parameters used to create transports upon
// receiving a GoAway.
func (ac *addrConn) adjustParams(r transport.GoAwayReason) {
	switch r {
	case transport.GoAwayTooManyPings:
		v := 2 * ac.dopts.copts.KeepaliveParams.Time
		ac.cc.mu.Lock()
		if v > ac.cc.mkp.Time {
			ac.cc.mkp.Time = v
		}
		ac.cc.mu.Unlock()
	}
}

// printf records an event in ac's event log, unless ac has been closed.
// REQUIRES ac.mu is held.
func (ac *addrConn) printf(format string, a ...interface{}) {
	if ac.events != nil {
		ac.events.Printf(format, a...)
	}
}

// errorf records an error in ac's event log, unless ac has been closed.
// REQUIRES ac.mu is held.
func (ac *addrConn) errorf(format string, a ...interface{}) {
	if ac.events != nil {
		ac.events.Errorf(format, a...)
	}
}

// resetTransport makes sure that a healthy ac.transport exists.
//
// The transport will close when it encounters an error, or on GOAWAY,
// or on deadline waiting for handshake, or when the clientconn is
// closed. Each iteration creating a new transport will try a different
// address that the resolver resolved to, until it has tried all
// addresses. Once it has tried all addresses, it will re-resolve to get
// a new address list. Once a successful handshake has been
// received, the list is re-resolved and the next reset attempt will
// try from the beginning.
//
// This method has backoff built in. The backoff amount starts at 0 and
// increases each time resolution occurs (addresses are exhausted). The
// backoff amount is reset to 0 each time a handshake is received.
//
// If the DialOption WithWaitForHandshake was set, resetTrasport returns
// successfully only after handshake is received.
func (ac *addrConn) resetTransport(resolveNow bool) {
	for {
		// We want to resolve immediately if this is the first in a line of resets, but not on subsequent
		// resets unless we:
		//   - Run out of addresses
		//   - Successfully receive handshake
		if resolveNow {
			ac.mu.Lock()
			ac.cc.resolveNow(resolver.ResolveNowOption{})
			ac.mu.Unlock()
		}

		err := ac.nextAddr()
		if err != nil {
			grpclog.Error(err)
		}

		ac.mu.Lock()
		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			return
		}
		if ac.ready != nil {
			close(ac.ready)
			ac.ready = nil
		}
		ac.transport = nil

		backoffIdx := ac.backoffIdx
		backoffFor := ac.dopts.bs.Backoff(backoffIdx)

		// This will be the duration that dial gets to finish.
		dialDuration := getMinConnectTimeout()
		if backoffFor > dialDuration {
			// Give dial more time as we keep failing to connect.
			dialDuration = backoffFor
		}
		start := time.Now()
		backoffDeadline := start.Add(backoffFor)
		ac.backoffDeadline = backoffDeadline
		ac.connectDeadline = start.Add(dialDuration)

		ac.mu.Unlock()

		ac.cc.mu.RLock()
		ac.dopts.copts.KeepaliveParams = ac.cc.mkp
		ac.cc.mu.RUnlock()

		newTrID := atomic.AddInt32(&ac.transportIdx, 1)

		// Generally, onClose should reset the transport. However, if we get a GO_AWAY,
		// onGoAway will reset the transport instead, which means when the original
		// transport finally gets around to closing (onClose) it should not reset
		// (since we already did it in onGoAway)
		resetOnClose := true

		onGoAway := func(r transport.GoAwayReason) {
			ac.mu.Lock()
			resetOnClose = false
			ac.adjustParams(r)
			ac.mu.Unlock()
			go ac.resetTransport(false)
		}

		onClose := func() {
			ac.mu.Lock()
			delete(ac.transports, newTrID)
			r := resetOnClose
			if r {
				ac.transport = nil
			}
			ac.mu.Unlock()
			if r {
				ac.resetTransport(false)
			}
		}

		ac.mu.Lock()

		if ac.state == connectivity.Shutdown {
			ac.mu.Unlock()
			return
		}

		ac.printf("connecting")
		if ac.state != connectivity.Connecting {
			ac.state = connectivity.Connecting
			ac.cc.handleSubConnStateChange(ac.acbw, ac.state)
		}

		addr := ac.addrs[ac.addrIdx]
		copts := ac.dopts.copts
		ac.mu.Unlock()

		if err := ac.createTransport(newTrID, backoffIdx, addr, copts, backoffDeadline, onGoAway, onClose); err != nil {
			// errReadTimeOut indicates that the handshake was not received before
			// the deadline. We exit here because the transport's reader goroutine will
			// use onClose to reset the transport.
			if err == errReadTimedOut {
				return
			}

			timer := time.NewTimer(backoffDeadline.Sub(time.Now()))
			select {
			case <-timer.C:
			case <-ac.ctx.Done():
				timer.Stop()
				return
			}

			continue
		}

		return
	}
}

var errReadTimedOut = errors.New("read timed out")

// createTransport creates a connection to one of the backends in addrs.
func (ac *addrConn) createTransport(id int32, backoffNum int, addr resolver.Address, copts transport.ConnectOptions, backoffDeadline time.Time, onGoAway func(transport.GoAwayReason), onClose func()) error {
	timedOutWaitingForPreface := make(chan struct{})
	// TODO(deklerk): this is unnecessary. In the reader goroutine, we should be able to signal to onClose that the
	// deadline was exceeded (we can't use a parameter to t.Close because it would mean changes in too many places)
	onDeadline := func() {
		close(timedOutWaitingForPreface)
	}

	target := transport.TargetInfo{
		Addr:      addr.Addr,
		Metadata:  addr.Metadata,
		Authority: ac.cc.authority,
	}
	prefaceReceived := make(chan struct{})

	onPrefaceReceipt := func() {
		// TODO(deklerk): optimization; does anyone else actually use this lock? maybe we can just remove it for this scope
		ac.mu.Lock()
		ac.successfulHandshake = true
		close(prefaceReceived)
		ac.backoffDeadline = time.Time{}
		ac.connectDeadline = time.Time{}
		ac.addrIdx = 0
		ac.backoffIdx = 0
		ac.mu.Unlock()
	}

	ac.mu.Lock()
	connectDeadline := ac.connectDeadline
	ac.mu.Unlock()

	// Do not cancel in the success path because of
	// this issue in Go1.6: https://github.com/golang/go/issues/15078.
	connectCtx, cancel := context.WithDeadline(ac.ctx, connectDeadline)
	if channelz.IsOn() {
		copts.ChannelzParentID = ac.channelzID
	}

	newTr, err := transport.NewClientTransport(connectCtx, ac.cc.ctx, target, copts, backoffDeadline, onPrefaceReceipt, onDeadline, onGoAway, onClose)
	if err != nil {
		cancel()
		ac.cc.blockingpicker.updateConnectionError(err)
		ac.mu.Lock()
		if ac.state == connectivity.Shutdown {
			// ac.tearDown(...) has been invoked.
			ac.mu.Unlock()
			return errConnClosing
		}
		ac.state = connectivity.TransientFailure
		ac.cc.handleSubConnStateChange(ac.acbw, ac.state)
		ac.mu.Unlock()
		grpclog.Warningf("grpc: addrConn.createTransport failed to connect to %v. Err :%v. Reconnecting...", addr, err)

		return err
	}

	if ac.dopts.waitForHandshake {
		select {
		case <-timedOutWaitingForPreface:
			return errReadTimedOut
		case <-prefaceReceived:
		case <-ac.ctx.Done():
			return errConnClosing
		}
	}

	ac.mu.Lock()

	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		// ac.tearDonn(...) has been invoked.
		newTr.Close()
		return errConnClosing
	}

	ac.printf("ready")
	ac.state = connectivity.Ready
	ac.cc.handleSubConnStateChange(ac.acbw, ac.state)
	ac.transport = newTr
	ac.curAddr = addr
	if ac.ready != nil {
		close(ac.ready)
		ac.ready = nil
	}

	ac.transports[id] = newTr
	ac.mu.Unlock()
	return nil
}

// nextAddr increments the addrIdx if there are more addresses to try. If
// there are no more addrs to try it will re-resolve, set addrIdx to 0, and
// increment the backoffIdx.
func (ac *addrConn) nextAddr() error {
	ac.mu.Lock()

	// If a handshake has been observed, we expect the counters to have manually
	// been adjusted and so we'll just re-resolve and return.
	if ac.successfulHandshake {
		ac.successfulHandshake = false
		ac.cc.resolveNow(resolver.ResolveNowOption{})
		ac.mu.Unlock()
		return nil
	}

	if ac.addrIdx < len(ac.addrs)-1 {
		ac.addrIdx++
		ac.mu.Unlock()
		return nil
	}

	ac.addrIdx = 0
	ac.backoffIdx++

	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return errConnClosing
	}
	ac.state = connectivity.TransientFailure
	ac.cc.handleSubConnStateChange(ac.acbw, ac.state)
	ac.cc.resolveNow(resolver.ResolveNowOption{})
	if ac.ready != nil {
		close(ac.ready)
		ac.ready = nil
	}
	backoffDeadline := ac.backoffDeadline
	ac.mu.Unlock()
	timer := time.NewTimer(backoffDeadline.Sub(time.Now()))
	select {
	case <-timer.C:
	case <-ac.ctx.Done():
		timer.Stop()
		return ac.ctx.Err()
	}
	return nil
}

// getReadyTransport returns the transport if ac's state is READY.
// Otherwise it returns nil, false.
// If ac's state is IDLE, it will trigger ac to connect.
func (ac *addrConn) getReadyTransport() (transport.ClientTransport, bool) {
	ac.mu.Lock()
	if ac.state == connectivity.Ready && ac.transport != nil {
		t := ac.transport
		ac.mu.Unlock()
		return t, true
	}
	var idle bool
	if ac.state == connectivity.Idle {
		idle = true
	}
	ac.mu.Unlock()
	// Trigger idle ac to connect.
	if idle {
		ac.connect()
	}
	return nil, false
}

// tearDown starts to tear down the addrConn.
// TODO(zhaoq): Make this synchronous to avoid unbounded memory consumption in
// some edge cases (e.g., the caller opens and closes many addrConn's in a
// tight loop.
// tearDown doesn't remove ac from ac.cc.conns.
func (ac *addrConn) tearDown(err error) {
	ac.cancel()
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return
	}
	ac.curAddr = resolver.Address{}
	if err == errConnDrain && ac.transport != nil {
		// GracefulClose(...) may be executed multiple times when
		// i) receiving multiple GoAway frames from the server; or
		// ii) there are concurrent name resolver/Balancer triggered
		// address removal and GoAway.
		ac.mu.Unlock()
		ac.transport.GracefulClose()
		ac.mu.Lock()
	}
	ac.state = connectivity.Shutdown
	ac.tearDownErr = err
	ac.cc.handleSubConnStateChange(ac.acbw, ac.state)
	if ac.events != nil {
		ac.events.Finish()
		ac.events = nil
	}
	if ac.ready != nil {
		close(ac.ready)
		ac.ready = nil
	}
	if channelz.IsOn() {
		channelz.RemoveEntry(ac.channelzID)
	}

	// TODO(deklerk) is this bad?
	transports := map[int32]transport.ClientTransport{}
	for i, v := range ac.transports {
		transports[i] = v
	}

	ac.mu.Unlock()
	for _, t := range transports {
		err := t.GracefulClose()
		if err != nil {
			grpclog.Error(err)
		}
	}
}

func (ac *addrConn) getState() connectivity.State {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	return ac.state
}

func (ac *addrConn) ChannelzMetric() *channelz.ChannelInternalMetric {
	ac.mu.Lock()
	addr := ac.curAddr.Addr
	ac.mu.Unlock()
	state := ac.getState()
	ac.czmu.RLock()
	defer ac.czmu.RUnlock()
	return &channelz.ChannelInternalMetric{
		State:                    state,
		Target:                   addr,
		CallsStarted:             ac.callsStarted,
		CallsSucceeded:           ac.callsSucceeded,
		CallsFailed:              ac.callsFailed,
		LastCallStartedTimestamp: ac.lastCallStartedTime,
	}
}

func (ac *addrConn) incrCallsStarted() {
	ac.czmu.Lock()
	ac.callsStarted++
	ac.lastCallStartedTime = time.Now()
	ac.czmu.Unlock()
}

func (ac *addrConn) incrCallsSucceeded() {
	ac.czmu.Lock()
	ac.callsSucceeded++
	ac.czmu.Unlock()
}

func (ac *addrConn) incrCallsFailed() {
	ac.czmu.Lock()
	ac.callsFailed++
	ac.czmu.Unlock()
}

type retryThrottler struct {
	max    float64
	thresh float64
	ratio  float64

	mu     sync.Mutex
	tokens float64 // TODO(dfawley): replace with atomic and remove lock.
}

// throttle subtracts a retry token from the pool and returns whether a retry
// should be throttled (disallowed) based upon the retry throttling policy in
// the service config.
func (rt *retryThrottler) throttle() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens--
	if rt.tokens < 0 {
		rt.tokens = 0
	}
	return rt.tokens <= rt.thresh
}

func (rt *retryThrottler) successfulRPC() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens += rt.ratio
	if rt.tokens > rt.max {
		rt.tokens = rt.max
	}
}

// ErrClientConnTimeout indicates that the ClientConn cannot establish the
// underlying connections within the specified timeout.
//
// Deprecated: This error is never returned by grpc and should not be
// referenced by users.
var ErrClientConnTimeout = errors.New("grpc: timed out when dialing")
