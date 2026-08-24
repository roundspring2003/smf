package processor

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/free5gc/smf/internal/pfcp/pfcptype"
	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"

	smf_context "github.com/free5gc/smf/internal/context"
)

type fakeActivePFCPClient struct {
	setup     func(*net.UDPAddr) (*message.AssociationSetupResponse, error)
	heartbeat func(*net.UDPAddr) (*message.HeartbeatResponse, error)
}

func (f *fakeActivePFCPClient) SendAssociationSetupRequest(
	addr *net.UDPAddr,
) (*message.AssociationSetupResponse, error) {
	return f.setup(addr)
}

func (f *fakeActivePFCPClient) SendHeartbeatRequest(
	addr *net.UDPAddr,
) (*message.HeartbeatResponse, error) {
	return f.heartbeat(addr)
}

func newActiveAssociationTestUPF(t *testing.T) *smf_context.UPF {
	t.Helper()
	upf := &smf_context.UPF{
		NodeID: pfcptype.NodeID{
			NodeIdType: pfcptype.NodeIdTypeIpv4Address,
			IP:         net.ParseIP("192.0.2.10").To4(),
		},
	}
	upf.AssociationContext, upf.CancelAssociation = context.WithCancel(context.Background())
	t.Cleanup(upf.CancelAssociation)
	return upf
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

	if p.ensureSetupPfcpAssociation(parentContext, upf, "[192.0.2.10]") {
		t.Fatal("ensureSetupPfcpAssociation() succeeded after parent cancellation")
	}
	if setupCalled {
		t.Fatal("Association Setup was sent after parent cancellation")
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

	if err := p.setupPfcpAssociation(upf, "[192.0.2.10]"); err != nil {
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

	if err := p.doPfcpHeartbeat(upf, "[192.0.2.10]"); err != nil {
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

	err := p.doPfcpHeartbeat(upf, "[192.0.2.10]")
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

	err := p.doPfcpHeartbeat(upf, "[192.0.2.10]")
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

	if err := p.doPfcpHeartbeat(upf, "[192.0.2.10]"); err != nil {
		t.Fatalf("doPfcpHeartbeat() error: %v", err)
	}
	if got, want := upf.RecoveryTimeStamp.Unix(), recoveryTime.Unix(); got != want {
		t.Fatalf("UPF RecoveryTimeStamp = %d, want %d", got, want)
	}
}
