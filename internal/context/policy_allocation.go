package context

import (
	"time"

	ngapie "github.com/free5gc/ngap/ie"
	"github.com/free5gc/openapi/models"
)

// PolicyAllocation tracks one PCF-initiated QoS modification under SMLock.
type PolicyAllocation struct {
	RuleQFIs      map[string]uint8
	PreviousRules map[string]*PCCRule
	PreviousFlows map[uint8]*QoSFlow
	Rollback      *models.Pcf_SMPolCtrl_SmPolicyDecision
	Outcomes      map[string]bool
	NewNasRuleIDs map[string]uint8
	Done          chan struct{}
	Timer         *time.Timer
}

// recordPolicyAllocationResponse matches the NG-RAN result before the processor
// completes the procedure. Install/modify failures can still be reported by
// the normal N2 handler before the allocation procedure restores local rules.
func (ctx *SMContext) recordPolicyAllocationResponse(response *ngapie.PDUSessionResourceModifyResponseTransfer) {
	allocation := ctx.PendingPolicyAllocation
	if allocation == nil {
		return
	}
	results := make(map[uint8]bool)
	if accepted := response.QosFlowAddOrModifyResponseList; accepted != nil {
		for _, item := range accepted.List {
			results[uint8(item.QosFlowIdentifier.Value)] = true
		}
	}
	if rejected := response.QosFlowFailedToAddOrModifyList; rejected != nil {
		for _, item := range rejected.List {
			results[uint8(item.QosFlowIdentifier.Value)] = false
		}
	}
	for id, qfi := range allocation.RuleQFIs {
		if accepted, seen := results[qfi]; seen && qfi != 0 {
			rule := ctx.PCCRules[id]
			allocation.Outcomes[id] = accepted && rule != nil && rule.QFI == qfi
		}
	}
}
