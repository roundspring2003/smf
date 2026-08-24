package processor

import (
	"time"

	"github.com/wmnsk/go-pfcp/ie"

	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

// SetupAssociation adapts the new passive Association handler to the existing
// configured UPF registry. Active Association/Heartbeat remains the authority
// that creates the long-lived AssociationContext and detects UPF restart.
func (p *Processor) SetupAssociation(
	peer pfcptype.NodeID,
	_ time.Time,
) (uint8, func()) {
	if smf_context.RetrieveUPFNodeByNodeID(peer) == nil {
		return ie.CauseRequestRejected, nil
	}
	return ie.CauseRequestAccepted, nil
}

// ReleaseAssociation invalidates the existing association without deleting the
// configured UPF topology. The active state machine performs session cleanup
// and decides whether to establish the configured association again.
func (p *Processor) ReleaseAssociation(peer pfcptype.NodeID) uint8 {
	upf := smf_context.RetrieveUPFNodeByNodeID(peer)
	if upf == nil {
		return ie.CauseNoEstablishedPFCPAssociation
	}
	cancelUPFAssociation(upf)
	return ie.CauseRequestAccepted
}
