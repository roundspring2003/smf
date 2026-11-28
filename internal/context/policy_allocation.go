package context

import (
	"time"

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
