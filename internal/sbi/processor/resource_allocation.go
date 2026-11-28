package processor

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"time"

	nasie "github.com/free5gc/nas/ie"
	"github.com/free5gc/nas/message"
	"github.com/free5gc/openapi/models"
	smf_context "github.com/free5gc/smf/internal/context"
)

const policyAllocationTimeout = 30 * time.Second

func preparePolicyAllocation(
	sm *smf_context.SMContext, decision *models.Pcf_SMPolCtrl_SmPolicyDecision,
) *smf_context.PolicyAllocation {
	if len(decision.LastReqRuleData) == 0 {
		return nil
	}
	allocation := &smf_context.PolicyAllocation{
		RuleQFIs:      make(map[string]uint8),
		PreviousRules: make(map[string]*smf_context.PCCRule),
		PreviousFlows: make(map[uint8]*smf_context.QoSFlow),
		Outcomes:      make(map[string]bool),
		NewNasRuleIDs: make(map[string]uint8),
		Done:          make(chan struct{}),
		Rollback: &models.Pcf_SMPolCtrl_SmPolicyDecision{
			PccRules:      make(map[string]*models.Pcf_SMPolCtrl_PccRule),
			QosDecs:       make(map[string]*models.Pcf_SMPolCtrl_QosData),
			ChgDecs:       make(map[string]*models.Pcf_SMPolCtrl_ChargingData),
			TraffContDecs: make(map[string]*models.Pcf_SMPolCtrl_TrafficControlData),
		},
	}
	for _, request := range decision.LastReqRuleData {
		if !slices.Contains(request.ReqData, models.Pcf_SMPolCtrl_RequestedRuleDataType_SUCC_RES_ALLO) {
			continue
		}
		for _, id := range request.RefPccRuleIds {
			pccRule, present := decision.PccRules[id]
			old := sm.PCCRules[id]
			if (present && pccRule == nil) || (!present && old == nil) {
				continue
			}
			allocation.RuleQFIs[id] = 0
			if old == nil {
				allocation.Rollback.PccRules[id] = nil
				continue
			}
			allocation.PreviousRules[id] = old
			allocation.Rollback.PccRules[id] = old.Pcf_SMPolCtrl_PccRule
			if qosID := old.RefQosDataID(); qosID != "" {
				allocation.Rollback.QosDecs[qosID] = sm.QosDatas[qosID]
			}
			if tcID := old.RefTcDataID(); tcID != "" && sm.TrafficControlDatas[tcID] != nil {
				allocation.Rollback.TraffContDecs[tcID] = sm.TrafficControlDatas[tcID].Pcf_SMPolCtrl_TrafficControlData
			}
			if chgID := old.RefChgDataID(); chgID != "" {
				allocation.Rollback.ChgDecs[chgID] = sm.ChargingData[chgID]
			}
		}
	}
	if len(allocation.RuleQFIs) == 0 {
		return nil
	}
	for qfi, flow := range sm.AdditionalQosFlows {
		previous := *flow
		allocation.PreviousFlows[qfi] = &previous
	}
	return allocation
}

func (p *Processor) requestPolicyAllocation(sm *smf_context.SMContext, allocation *smf_context.PolicyAllocation) error {
	command := &message.PDUSessModCmd{PDUSessId: uint8(sm.PDUSessionID)}
	rules := &nasie.QosRules{}
	descriptions := &nasie.QosFlowDescs{}
	var flows []*smf_context.QoSFlow
	seenQFI := make(map[uint8]bool)

	for id := range allocation.RuleQFIs {
		rule := sm.PCCRules[id]
		if rule == nil || rule.QFI == 0 {
			return fmt.Errorf("no QoS flow bound to PCC rule %s", id)
		}
		flow := sm.AdditionalQosFlows[rule.QFI]
		previous := allocation.PreviousFlows[rule.QFI]
		nasID := sm.PCCRuleIDToQoSRuleID[id]
		if ruleAlreadyAllocated(sm, id, rule, allocation, nasID) {
			allocation.Outcomes[id] = true
			continue
		}
		if flow == nil {
			return fmt.Errorf("PCC rule %s needs a dedicated QoS flow for resource modification", id)
		}

		allocation.RuleQFIs[id] = rule.QFI
		operation := nasie.OpCode_ModifyReplaceAllPktFilters
		if nasID == 0 {
			allocated, err := sm.QoSRuleIDGenerator.Allocate()
			if err != nil {
				return err
			}
			nasID = uint8(allocated)
			allocation.NewNasRuleIDs[id] = nasID
			operation = nasie.OpCode_CreateNewQosRule
		}
		nasRule, err := rule.BuildNasQoSRule(sm, operation)
		if err != nil {
			return fmt.Errorf("build NAS QoS rule %s: %w", id, err)
		}
		if len(rule.FlowInfos) != 0 && len(nasRule.PktFilterList) == 0 {
			return fmt.Errorf("no NAS packet filters for PCC rule %s", id)
		}
		nasRule.RuleId = nasID
		rules.Rules = append(rules.Rules, *nasRule)

		if !seenQFI[rule.QFI] {
			seenQFI[rule.QFI] = true
			flows = append(flows, flow)
			op := nasie.QFD_Create
			if previous != nil && previous.State == smf_context.QoSFlowSet {
				op = nasie.QFD_Mod
			}
			desc, descErr := flow.BuildNasQoSDesc(op)
			if descErr != nil {
				return descErr
			}
			descriptions.Descs = append(descriptions.Descs, desc)
		}
	}
	if len(flows) == 0 {
		p.finishPolicyAllocation(sm, false)
		return nil
	}

	command.AuthoQosRules = rules
	command.AuthoQosFlowDescs = descriptions
	nas, err := command.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode PDU session modification command: %w", err)
	}
	n2, err := smf_context.BuildPDUSessionResourceModifyRequestForFlows(flows)
	if err != nil {
		return err
	}
	if transferErr := p.transferPolicyAllocation(sm, nas, n2); transferErr != nil {
		return transferErr
	}
	for _, flow := range flows {
		if previous := allocation.PreviousFlows[flow.QFI]; previous != nil && previous.State == smf_context.QoSFlowSet {
			flow.State = smf_context.QoSFlowToBeModify
		}
	}

	upi := smf_context.GetUserPlaneInformation()
	allocation.Timer = time.AfterFunc(policyAllocationTimeout, func() {
		if upi != nil {
			upi.Mu.RLock()
			defer upi.Mu.RUnlock()
		}
		sm.SMLock.Lock()
		defer sm.SMLock.Unlock()
		if sm.PendingPolicyAllocation == allocation {
			p.finishPolicyAllocation(sm, true)
		}
	})
	return nil
}

func ruleAlreadyAllocated(
	sm *smf_context.SMContext, id string, rule *smf_context.PCCRule,
	allocation *smf_context.PolicyAllocation, nasID uint8,
) bool {
	old := allocation.PreviousRules[id]
	if old == nil || old.QFI != rule.QFI || nasID == 0 ||
		!reflect.DeepEqual(old.Pcf_SMPolCtrl_PccRule, rule.Pcf_SMPolCtrl_PccRule) {
		return false
	}
	flow, previous := sm.AdditionalQosFlows[rule.QFI], allocation.PreviousFlows[rule.QFI]
	if previous != nil && flow != nil {
		return previous.State == smf_context.QoSFlowSet && reflect.DeepEqual(previous.QoSProfile, flow.QoSProfile)
	}
	return sm.SelectedSessionRule() != nil && sm.SelectedSessionRule().DefQosQFI == rule.QFI &&
		sm.UpCnxState == models.Smf_PDUSess_UpCnxState_ACTIVATED
}

func (p *Processor) transferPolicyAllocation(sm *smf_context.SMContext, nas, n2 []byte) error {
	request := models.N1N2MessageTransferRequestBody{
		BinaryDataN1Message:     related("GSM_NAS", nas),
		BinaryDataN2Information: related("PDU_RES_MOD", n2),
		JsonData: &models.Amf_Comm_N1N2MessageTransferReqData{
			PduSessionId: sm.PDUSessionID,
			N1MessageContainer: &models.Amf_Comm_N1MessageContainer{
				N1MessageClass:   models.Amf_Comm_N1MessageClass_SM,
				N1MessageContent: &models.RefToBinaryData{ContentId: "GSM_NAS"},
			},
			N2InfoContainer: &models.Amf_Comm_N2InfoContainer{
				N2InformationClass: models.Amf_Comm_N2InformationClass_SM,
				SmInfo: &models.Amf_Comm_N2SmInformation{
					PduSessionId: sm.PDUSessionID,
					SNssai:       sm.SNssai,
					N2InfoContent: &models.Amf_Comm_N2InfoContent{
						NgapIeType: models.Amf_Comm_NgapIeType_PDU_RES_MOD_REQ,
						NgapData:   &models.RefToBinaryData{ContentId: "PDU_RES_MOD"},
					},
				},
			},
		},
	}

	ctx, _, err := smf_context.GetSelf().GetTokenCtxForNFInstance(
		models.Nrf_NFMgmt_ServiceName_NAMF_COMM, models.Nrf_NFMgmt_NFType_AMF, sm.AMFProfile.NfInstanceId,
	)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, policyAllocationTimeout)
	defer cancel()
	response, err := p.Consumer().N1N2MessageTransfer(ctx, sm.Supi, request, sm.CommunicationClientApiPrefix)
	if err != nil {
		return err
	}
	if response == nil {
		return fmt.Errorf("no AMF response to QoS modification")
	}
	switch response.Cause {
	case models.Amf_Comm_N1N2MessageTransferCause_N1_N2_TRANSFER_INITIATED,
		models.Amf_Comm_N1N2MessageTransferCause_ATTEMPTING_TO_REACH_UE:
		return nil
	default:
		return fmt.Errorf("AMF could not initiate QoS modification: %s", response.Cause)
	}
}

func (p *Processor) completePolicyAllocationFromN2(
	sm *smf_context.SMContext, infoType models.Smf_PDUSess_N2SmInfoType,
) {
	allocation := sm.PendingPolicyAllocation
	if allocation == nil {
		return
	}
	switch infoType {
	case models.Smf_PDUSess_N2SmInfoType_PDU_RES_MOD_RSP:
		if len(allocation.Outcomes) == len(allocation.RuleQFIs) {
			p.finishPolicyAllocation(sm, true)
		}
	case models.Smf_PDUSess_N2SmInfoType_PDU_RES_MOD_FAIL:
		p.finishPolicyAllocation(sm, true)
	}
}

// finishPolicyAllocation is called under SMLock. Only explicit NG-RAN acceptance
// (or existing confirmed resources) can report ACTIVE.
func (p *Processor) finishPolicyAllocation(sm *smf_context.SMContext, rollback bool) {
	allocation := sm.PendingPolicyAllocation
	if allocation == nil {
		return
	}
	sm.PendingPolicyAllocation = nil
	defer close(allocation.Done)
	if allocation.Timer != nil {
		allocation.Timer.Stop()
	}
	state := sm.State()
	if smf_context.GetSMContextByRef(sm.Ref) != sm ||
		state == smf_context.InActive || state == smf_context.InActivePending || sm.PFCPReleaseDone {
		return
	}

	var confirmed []string
	for id := range allocation.RuleQFIs {
		nasID := allocation.NewNasRuleIDs[id]
		if allocation.Outcomes[id] {
			delete(allocation.Rollback.PccRules, id)
			if nasID != 0 {
				sm.PCCRuleIDToQoSRuleID[id] = nasID
			}
			confirmed = append(confirmed, id)
		} else if nasID != 0 {
			sm.QoSRuleIDGenerator.FreeID(int64(nasID))
		}
	}

	if rollback && len(allocation.Rollback.PccRules) != 0 {
		p.restorePolicyAllocation(sm, allocation)
	}
	if len(confirmed) == 0 {
		return
	}
	slices.Sort(confirmed)

	policyID, profile, log := sm.SMPolicyID, sm.SelectedPCFProfile, sm.Log
	if policyID != "" {
		go func() {
			if err := p.Consumer().ReportSuccessfulResourceAllocation(policyID, &profile, confirmed); err != nil {
				log.Warnf("Report successful resource allocation: %v", err)
			}
		}()
	}
}

func (p *Processor) restorePolicyAllocation(sm *smf_context.SMContext, allocation *smf_context.PolicyAllocation) {
	// Only restore data referenced by a failed rule, to keep successful updates.
	restoration := allocation.Rollback
	decision := &models.Pcf_SMPolCtrl_SmPolicyDecision{
		PccRules:      restoration.PccRules,
		QosDecs:       make(map[string]*models.Pcf_SMPolCtrl_QosData),
		ChgDecs:       make(map[string]*models.Pcf_SMPolCtrl_ChargingData),
		TraffContDecs: make(map[string]*models.Pcf_SMPolCtrl_TrafficControlData),
	}
	for id := range decision.PccRules {
		if old := allocation.PreviousRules[id]; old != nil {
			if qosID := old.RefQosDataID(); qosID != "" {
				decision.QosDecs[qosID] = restoration.QosDecs[qosID]
			}
			if tcID := old.RefTcDataID(); tcID != "" {
				decision.TraffContDecs[tcID] = restoration.TraffContDecs[tcID]
			}
			if chgID := old.RefChgDataID(); chgID != "" {
				decision.ChgDecs[chgID] = restoration.ChgDecs[chgID]
			}
		}
	}
	if err := sm.ApplyPccRules(decision); err != nil {
		sm.Log.Errorf("Restore PCC rules after rejected QoS modification: %v", err)
		return
	}
	if !p.ActivateUPFSession(sm, nil) {
		sm.Log.Warn("UPF could not restore PCC rules after rejected QoS modification")
		return
	}
	for id := range decision.PccRules {
		if rule := sm.PCCRules[id]; rule != nil && sm.AdditionalQosFlows[rule.QFI] != nil {
			if previous := allocation.PreviousFlows[rule.QFI]; previous != nil {
				sm.AdditionalQosFlows[rule.QFI].State = previous.State
			}
		}
	}
	sm.PostRemoveDataPath()
}
