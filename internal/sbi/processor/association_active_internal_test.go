package processor

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

type fakeActivePFCPClient struct {
	setup        func(*net.UDPAddr) (*message.AssociationSetupResponse, error)
	setupCtx     func(context.Context, *net.UDPAddr) (*message.AssociationSetupResponse, error)
	heartbeat    func(*net.UDPAddr) (*message.HeartbeatResponse, error)
	heartbeatCtx func(context.Context, *net.UDPAddr) (*message.HeartbeatResponse, error)
	release      func(*net.UDPAddr) (*message.AssociationReleaseResponse, error)
	releaseCtx   func(context.Context, *net.UDPAddr) (*message.AssociationReleaseResponse, error)
	deleteCtx    func(
		context.Context, *message.SessionDeletionRequest, *net.UDPAddr, uint64,
	) (*message.SessionDeletionResponse, error)
}

func (f *fakeActivePFCPClient) SendAssociationSetupRequest(
	ctx context.Context,
	addr *net.UDPAddr,
) (*message.AssociationSetupResponse, error) {
	if f.setupCtx != nil {
		return f.setupCtx(ctx, addr)
	}
	return f.setup(addr)
}

func (f *fakeActivePFCPClient) SendHeartbeatRequest(
	ctx context.Context,
	addr *net.UDPAddr,
) (*message.HeartbeatResponse, error) {
	if f.heartbeatCtx != nil {
		return f.heartbeatCtx(ctx, addr)
	}
	return f.heartbeat(addr)
}

func (f *fakeActivePFCPClient) SendAssociationReleaseRequest(
	ctx context.Context,
	addr *net.UDPAddr,
) (*message.AssociationReleaseResponse, error) {
	if f.releaseCtx != nil {
		return f.releaseCtx(ctx, addr)
	}
	return f.release(addr)
}

func (f *fakeActivePFCPClient) SendSessionDeletionRequest(
	ctx context.Context,
	request *message.SessionDeletionRequest,
	addr *net.UDPAddr,
	localSEID uint64,
) (*message.SessionDeletionResponse, error) {
	return f.deleteCtx(ctx, request, addr, localSEID)
}

func newActiveAssociationTestUPF(t *testing.T) *smf_context.UPF {
	t.Helper()
	upf := &smf_context.UPF{
		NodeID: pfcptype.NodeID{
			NodeIdType: pfcptype.NodeIdTypeIpv4Address,
			IP:         net.ParseIP("192.0.2.10").To4(),
		},
	}
	upf.EstablishAssociation(context.Background())
	t.Cleanup(upf.CancelAssociation)
	return upf
}

func TestEnsureSetupReturnsAssociationContext(t *testing.T) {
	upf := &smf_context.UPF{
		NodeID: pfcptype.NodeID{
			NodeIdType: pfcptype.NodeIdTypeIpv4Address,
			IP:         net.ParseIP("192.0.2.11").To4(),
		},
	}
	parentContext, cancelParent := context.WithCancel(context.Background())
	t.Cleanup(cancelParent)
	recoveryTime := time.Now().Add(-time.Minute).Truncate(time.Second)
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		setupCtx: func(ctx context.Context, _ *net.UDPAddr) (*message.AssociationSetupResponse, error) {
			if ctx != parentContext {
				t.Errorf("Association Setup context does not match PFCP parent context")
			}
			return message.NewAssociationSetupResponse(
				1,
				ie.NewNodeIDHeuristic("192.0.2.11"),
				ie.NewCause(ie.CauseRequestAccepted),
				ie.NewRecoveryTimeStamp(recoveryTime),
			), nil
		},
	})

	associationContext, established := p.ensureSetupPfcpAssociation(
		parentContext, upf, "[192.0.2.11]",
	)
	if !established || associationContext == nil {
		t.Fatal("Association Setup did not return an association context")
	}
	if associationContext == parentContext {
		t.Fatal("association context is the PFCP parent instead of a child context")
	}
	select {
	case <-associationContext.Done():
		t.Fatal("new association context is already canceled")
	default:
	}

	upf.CancelAssociation()
	select {
	case <-associationContext.Done():
	case <-time.After(time.Second):
		t.Fatal("CancelAssociation did not cancel the returned association context")
	}
}

func TestActiveAssociationLoopUsesAssociationContextForHeartbeat(t *testing.T) {
	upf := &smf_context.UPF{
		NodeID: pfcptype.NodeID{
			NodeIdType: pfcptype.NodeIdTypeIpv4Address,
			IP:         net.ParseIP("192.0.2.12").To4(),
		},
	}
	parentContext, cancelParent := context.WithCancel(context.Background())
	t.Cleanup(cancelParent)

	smfSelf := smf_context.GetSelf()
	previousInterval := smfSelf.PfcpHeartbeatInterval
	smfSelf.PfcpHeartbeatInterval = time.Hour
	t.Cleanup(func() { smfSelf.PfcpHeartbeatInterval = previousInterval })

	recoveryTime := time.Now().Add(-time.Minute).Truncate(time.Second)
	heartbeatContexts := make(chan context.Context, 1)
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		setup: func(*net.UDPAddr) (*message.AssociationSetupResponse, error) {
			return message.NewAssociationSetupResponse(
				1,
				ie.NewNodeIDHeuristic("192.0.2.12"),
				ie.NewCause(ie.CauseRequestAccepted),
				ie.NewRecoveryTimeStamp(recoveryTime),
			), nil
		},
		heartbeatCtx: func(ctx context.Context, _ *net.UDPAddr) (*message.HeartbeatResponse, error) {
			heartbeatContexts <- ctx
			return message.NewHeartbeatResponse(2, ie.NewRecoveryTimeStamp(recoveryTime)), nil
		},
	})

	loopDone := make(chan struct{})
	go func() {
		p.ToBeAssociatedWithUPF(parentContext, upf)
		close(loopDone)
	}()

	var heartbeatContext context.Context
	select {
	case heartbeatContext = <-heartbeatContexts:
	case <-time.After(time.Second):
		t.Fatal("association loop did not send Heartbeat")
	}
	if heartbeatContext == parentContext {
		t.Fatal("Heartbeat used the PFCP parent instead of the association context")
	}
	cancelParent()
	select {
	case <-heartbeatContext.Done():
	case <-time.After(time.Second):
		t.Fatal("PFCP parent cancellation did not cancel Heartbeat association context")
	}
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("association loop did not stop after PFCP parent cancellation")
	}
}

func TestActiveAssociationDoesNotRetryAfterParentCancellation(t *testing.T) {
	upf := newActiveAssociationTestUPF(t)
	parentContext, cancel := context.WithCancel(context.Background())
	cancel()
	setupCalled := false
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		setup: func(*net.UDPAddr) (*message.AssociationSetupResponse, error) {
			setupCalled = true
			return nil, errors.New("unexpected setup")
		},
	})

	if associationContext, established := p.ensureSetupPfcpAssociation(
		parentContext, upf, "[192.0.2.10]",
	); established || associationContext != nil {
		t.Fatal("ensureSetupPfcpAssociation() succeeded after parent cancellation")
	}
	if setupCalled {
		t.Fatal("Association Setup was sent after parent cancellation")
	}
}

func TestActiveAssociationRetryWaitStopsOnParentCancellation(t *testing.T) {
	upf := newActiveAssociationTestUPF(t)
	upf.CancelAssociation()
	parentContext, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	smfSelf := smf_context.GetSelf()
	previousRetryInterval := smfSelf.AssocFailRetryInterval
	smfSelf.AssocFailRetryInterval = time.Hour
	t.Cleanup(func() { smfSelf.AssocFailRetryInterval = previousRetryInterval })

	setupCalled := make(chan struct{}, 1)
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		setupCtx: func(context.Context, *net.UDPAddr) (*message.AssociationSetupResponse, error) {
			setupCalled <- struct{}{}
			return nil, errors.New("UPF unavailable")
		},
	})

	type setupResult struct {
		associationContext context.Context
		established        bool
	}
	resultChannel := make(chan setupResult, 1)
	go func() {
		associationContext, established := p.ensureSetupPfcpAssociation(
			parentContext, upf, "[192.0.2.10]",
		)
		resultChannel <- setupResult{associationContext, established}
	}()

	select {
	case <-setupCalled:
	case <-time.After(time.Second):
		t.Fatal("Association Setup was not attempted")
	}
	cancel()
	select {
	case result := <-resultChannel:
		if result.established || result.associationContext != nil {
			t.Fatal("Association Setup succeeded after parent cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("retry wait did not stop after parent cancellation")
	}
	if got := upf.AssociationState(); got != smf_context.AssociationDown {
		t.Fatalf("association state after canceled retry = %s, want down", got)
	}
}

func TestActiveAssociationSetupStoresUPFRecoveryTime(t *testing.T) {
	upf := newActiveAssociationTestUPF(t)
	recoveryTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		setup: func(addr *net.UDPAddr) (*message.AssociationSetupResponse, error) {
			if got, want := addr.IP.String(), "192.0.2.10"; got != want {
				t.Fatalf("destination IP = %s, want %s", got, want)
			}
			if got, want := addr.Port, 8805; got != want {
				t.Fatalf("destination port = %d, want %d", got, want)
			}
			return message.NewAssociationSetupResponse(
				1,
				ie.NewNodeIDHeuristic("192.0.2.10"),
				ie.NewCause(ie.CauseRequestAccepted),
				ie.NewRecoveryTimeStamp(recoveryTime),
			), nil
		},
	})

	if err := p.setupPfcpAssociation(context.Background(), upf, "[192.0.2.10]"); err != nil {
		t.Fatalf("setupPfcpAssociation() error: %v", err)
	}
	if got, want := upf.RecoveryTimeStamp.Unix(), recoveryTime.Unix(); got != want {
		t.Fatalf("UPF RecoveryTimeStamp = %d, want %d", got, want)
	}
}

func TestActiveHeartbeatKeepsAssociationForSameRecoveryTime(t *testing.T) {
	upf := newActiveAssociationTestUPF(t)
	recoveryTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	upf.RecoveryTimeStamp = recoveryTime
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		heartbeat: func(*net.UDPAddr) (*message.HeartbeatResponse, error) {
			return message.NewHeartbeatResponse(
				2,
				ie.NewRecoveryTimeStamp(recoveryTime),
			), nil
		},
	})

	if err := p.doPfcpHeartbeat(context.Background(), upf, "[192.0.2.10]"); err != nil {
		t.Fatalf("doPfcpHeartbeat() error: %v", err)
	}
	if err := upf.IsAssociated(); err != nil {
		t.Fatalf("association was canceled: %v", err)
	}
}

func TestActiveHeartbeatFailureCancelsAssociation(t *testing.T) {
	upf := newActiveAssociationTestUPF(t)
	upf.RecoveryTimeStamp = time.Now().Add(-time.Hour).Truncate(time.Second)
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		heartbeat: func(*net.UDPAddr) (*message.HeartbeatResponse, error) {
			return nil, errors.New("timeout")
		},
	})

	err := p.doPfcpHeartbeat(context.Background(), upf, "[192.0.2.10]")
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("doPfcpHeartbeat() error = %v, want timeout", err)
	}
	if err = upf.IsAssociated(); err == nil {
		t.Fatal("UPF remains associated after heartbeat failure")
	}
	if !upf.RecoveryTimeStamp.IsZero() {
		t.Fatalf("UPF RecoveryTimeStamp = %v, want zero", upf.RecoveryTimeStamp)
	}
}

func TestActiveHeartbeatDetectsUPFRestart(t *testing.T) {
	upf := newActiveAssociationTestUPF(t)
	oldRecoveryTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	upf.RecoveryTimeStamp = oldRecoveryTime
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		heartbeat: func(*net.UDPAddr) (*message.HeartbeatResponse, error) {
			return message.NewHeartbeatResponse(
				3,
				ie.NewRecoveryTimeStamp(oldRecoveryTime.Add(time.Minute)),
			), nil
		},
	})

	err := p.doPfcpHeartbeat(context.Background(), upf, "[192.0.2.10]")
	if err == nil || !strings.Contains(err.Error(), "updated") {
		t.Fatalf("doPfcpHeartbeat() error = %v, want updated Recovery Time Stamp", err)
	}
	if err = upf.IsAssociated(); err == nil {
		t.Fatal("UPF remains associated after Recovery Time Stamp changed")
	}
	if !upf.RecoveryTimeStamp.IsZero() {
		t.Fatalf("UPF RecoveryTimeStamp = %v, want zero", upf.RecoveryTimeStamp)
	}
}

func TestActiveHeartbeatUsesFirstRecoveryTimeAsBaseline(t *testing.T) {
	upf := newActiveAssociationTestUPF(t)
	recoveryTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	p := &Processor{}
	p.SetActivePFCPClient(&fakeActivePFCPClient{
		heartbeat: func(*net.UDPAddr) (*message.HeartbeatResponse, error) {
			return message.NewHeartbeatResponse(
				4,
				ie.NewRecoveryTimeStamp(recoveryTime),
			), nil
		},
	})

	if err := p.doPfcpHeartbeat(context.Background(), upf, "[192.0.2.10]"); err != nil {
		t.Fatalf("doPfcpHeartbeat() error: %v", err)
	}
	if got, want := upf.RecoveryTimeStamp.Unix(), recoveryTime.Unix(); got != want {
		t.Fatalf("UPF RecoveryTimeStamp = %d, want %d", got, want)
	}
}
