package processor

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/ie"

	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

func TestProcessorPassiveAssociationStateUsesConfiguredUPF(t *testing.T) {
	nodeID := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.81").To4(),
	}
	upf := smf_context.NewUPF(&nodeID, nil)
	t.Cleanup(func() { smf_context.RemoveUPFNodeByNodeID(nodeID) })
	peer := nodeID
	p := &Processor{}

	cause, afterResponse := p.SetupAssociation(peer, time.Now())
	if cause != ie.CauseRequestAccepted {
		t.Fatalf("SetupAssociation() cause = %d, want Request Accepted", cause)
	}
	if afterResponse != nil {
		t.Fatal("SetupAssociation() returned unexpected recovery work")
	}

	upf.AssociationContext, upf.CancelAssociation = context.WithCancel(context.Background())
	upf.RecoveryTimeStamp = time.Now()
	if cause = p.ReleaseAssociation(peer); cause != ie.CauseRequestAccepted {
		t.Fatalf("ReleaseAssociation() cause = %d, want Request Accepted", cause)
	}
	if err := upf.IsAssociated(); err == nil {
		t.Fatal("UPF remains associated after passive Association Release")
	}
	if !upf.RecoveryTimeStamp.IsZero() {
		t.Fatalf("RecoveryTimeStamp = %v, want zero", upf.RecoveryTimeStamp)
	}
}

func TestProcessorPassiveAssociationRejectsUnknownUPF(t *testing.T) {
	p := &Processor{}
	peer := pfcptype.NodeID{
		NodeIdType: pfcptype.NodeIdTypeIpv4Address,
		IP:         net.ParseIP("192.0.2.82").To4(),
	}
	cause, _ := p.SetupAssociation(peer, time.Now())
	if cause != ie.CauseRequestRejected {
		t.Fatalf("SetupAssociation() cause = %d, want Request Rejected", cause)
	}
	if cause = p.ReleaseAssociation(peer); cause != ie.CauseNoEstablishedPFCPAssociation {
		t.Fatalf("ReleaseAssociation() cause = %d, want No Established Association", cause)
	}
}
