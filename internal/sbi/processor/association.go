package processor

import (
	"context"
	"fmt"
	"net"
	"time"

	nasie "github.com/free5gc/nas/ie"
	"github.com/free5gc/openapi/mediatype/multipart"
	"github.com/free5gc/openapi/models"
	"github.com/wmnsk/go-pfcp/message"

	smf_context "github.com/free5gc/smf/internal/context"
	"github.com/free5gc/smf/internal/logger"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

const pfcpPeerPort = 8805

// ActivePFCPClient is the active node-level PFCP transport needed by the
// association state machine. *pfcp.PfcpServer satisfies this interface without
// making processor import internal/pfcp and creating an import cycle.
type ActivePFCPClient interface {
	SendAssociationSetupRequest(*net.UDPAddr) (*message.AssociationSetupResponse, error)
	SendHeartbeatRequest(*net.UDPAddr) (*message.HeartbeatResponse, error)
}

func (p *Processor) SetActivePFCPClient(client ActivePFCPClient) {
	p.activePFCPMu.Lock()
	p.activePFCPClient = client
	p.activePFCPMu.Unlock()
}

func (p *Processor) getActivePFCPClient() ActivePFCPClient {
	p.activePFCPMu.RLock()
	defer p.activePFCPMu.RUnlock()
	return p.activePFCPClient
}

func (p *Processor) ToBeAssociatedWithUPF(smfPfcpContext context.Context, upf *smf_context.UPF) {
	upfStr := formatUPF(upf)
	for {
		select {
		case <-smfPfcpContext.Done():
			logger.MainLog.Infoln("Canceled SMF PFCP context")
			return
		default:
		}

		if !p.ensureSetupPfcpAssociation(smfPfcpContext, upf, upfStr) {
			return
		}
		if smf_context.GetSelf().PfcpHeartbeatInterval == 0 {
			return
		}
		if err := p.keepHeartbeatTo(upf, upfStr); err != nil {
			logger.MainLog.Errorf("PFCP Heartbeat error: %v", err)
		}

		// Heartbeat loss, a changed Recovery Time Stamp, or external
		// association cancellation invalidates every PFCP session on this UPF.
		p.releaseAllResourcesOfUPF(upf, upfStr)
	}
}

func formatUPF(upf *smf_context.UPF) string {
	if upf.NodeID.NodeIdType == pfcptype.NodeIdTypeFqdn {
		return fmt.Sprintf("[%s](%s)", upf.NodeID.FQDN, upf.NodeID.ResolveNodeIdToIp().String())
	}
	return fmt.Sprintf("[%s]", upf.NodeID.ResolveNodeIdToIp().String())
}

func (p *Processor) ReleaseAllResourcesOfUPF(upf *smf_context.UPF) {
	p.releaseAllResourcesOfUPF(upf, formatUPF(upf))
}

func (p *Processor) ensureSetupPfcpAssociation(
	parentContext context.Context,
	upf *smf_context.UPF,
	upfStr string,
) bool {
	alertTime := time.Now()
	alertInterval := smf_context.GetSelf().AssocFailAlertInterval
	retryInterval := smf_context.GetSelf().AssocFailRetryInterval

	for {
		select {
		case <-parentContext.Done():
			logger.MainLog.Infoln("Canceled SMF PFCP context")
			return false
		default:
		}

		if err := p.setupPfcpAssociation(upf, upfStr); err == nil {
			upf.AssociationContext, upf.CancelAssociation = context.WithCancel(parentContext)
			return true
		} else {
			logger.MainLog.Warnf("Failed to setup an association with UPF[%s], error:%+v", upfStr, err)
			now := time.Now()
			if now.After(alertTime.Add(alertInterval)) {
				logger.MainLog.Errorf("ALERT for UPF[%s]", upfStr)
				alertTime = now
			}
		}

		timer := time.NewTimer(retryInterval)
		select {
		case <-parentContext.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			logger.MainLog.Infoln("Canceled SMF PFCP context")
			return false
		case <-timer.C:
		}
	}
}

func (p *Processor) setupPfcpAssociation(upf *smf_context.UPF, upfStr string) error {
	logger.MainLog.Infof("Sending PFCP Association Request to UPF%s", upfStr)

	client := p.getActivePFCPClient()
	if client == nil {
		return fmt.Errorf("go-pfcp active client is not configured")
	}

	response, err := client.SendAssociationSetupRequest(&net.UDPAddr{
		IP:   upf.NodeID.ResolveNodeIdToIp(),
		Port: pfcpPeerPort,
	})
	if err != nil {
		return err
	}
	if response == nil || response.RecoveryTimeStamp == nil {
		return fmt.Errorf("PFCP Association Setup Response from UPF%s is missing Recovery Time Stamp", upfStr)
	}
	recoveryTime, err := response.RecoveryTimeStamp.RecoveryTimeStamp()
	if err != nil {
		return fmt.Errorf("decode PFCP Association Setup Recovery Time Stamp from UPF%s: %w", upfStr, err)
	}
	upf.RecoveryTimeStamp = recoveryTime

	logger.MainLog.Infof("Received PFCP Association Setup Accepted Response from UPF%s", upfStr)
	logger.MainLog.Infof("UPF(%s) setup association", upf.NodeID.ResolveNodeIdToIp().String())
	return nil
}

func (p *Processor) keepHeartbeatTo(upf *smf_context.UPF, upfStr string) error {
	for {
		if err := p.doPfcpHeartbeat(upf, upfStr); err != nil {
			return err
		}

		timer := time.NewTimer(smf_context.GetSelf().PfcpHeartbeatInterval)
		select {
		case <-upf.AssociationContext.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			logger.MainLog.Infof("Canceled association to UPF[%s]", upfStr)
			return nil
		case <-timer.C:
		}
	}
}

func (p *Processor) doPfcpHeartbeat(upf *smf_context.UPF, upfStr string) error {
	if err := upf.IsAssociated(); err != nil {
		return fmt.Errorf("cancel heartbeat: %+v", err)
	}

	logger.MainLog.Debugf("Sending PFCP Heartbeat Request to UPF%s", upfStr)
	client := p.getActivePFCPClient()
	if client == nil {
		return fmt.Errorf("go-pfcp active client is not configured")
	}

	response, err := client.SendHeartbeatRequest(&net.UDPAddr{
		IP:   upf.NodeID.ResolveNodeIdToIp(),
		Port: pfcpPeerPort,
	})
	if err != nil {
		cancelUPFAssociation(upf)
		return fmt.Errorf("SendHeartbeatRequest error: %w", err)
	}
	if response == nil || response.RecoveryTimeStamp == nil {
		cancelUPFAssociation(upf)
		return fmt.Errorf("PFCP Heartbeat Response from UPF%s is missing Recovery Time Stamp", upfStr)
	}
	recoveryTime, err := response.RecoveryTimeStamp.RecoveryTimeStamp()
	if err != nil {
		cancelUPFAssociation(upf)
		return fmt.Errorf("decode PFCP Heartbeat Recovery Time Stamp from UPF%s: %w", upfStr, err)
	}
	return acceptHeartbeatRecoveryTime(upf, upfStr, recoveryTime)
}

func acceptHeartbeatRecoveryTime(upf *smf_context.UPF, upfStr string, recoveryTime time.Time) error {
	logger.MainLog.Debugf("Received PFCP Heartbeat Response from UPF%s", upfStr)
	if upf.RecoveryTimeStamp.IsZero() {
		upf.RecoveryTimeStamp = recoveryTime
		return nil
	}
	if upf.RecoveryTimeStamp.Before(recoveryTime) {
		cancelUPFAssociation(upf)
		return fmt.Errorf("received PFCP Heartbeat Response RecoveryTimeStamp has been updated")
	}
	return nil
}

func cancelUPFAssociation(upf *smf_context.UPF) {
	if upf.CancelAssociation != nil {
		upf.CancelAssociation()
	}
	upf.RecoveryTimeStamp = time.Time{}
}

func (p *Processor) releaseAllResourcesOfUPF(upf *smf_context.UPF, upfStr string) {
	logger.MainLog.Infof("Release all resources of UPF %s", upfStr)

	upf.ProcEachSMContext(func(smContext *smf_context.SMContext) {
		smContext.SMLock.Lock()
		defer smContext.SMLock.Unlock()
		switch smContext.State() {
		case smf_context.Active, smf_context.ModificationPending, smf_context.PFCPModification:
			needToSendNotify, removeContext := p.requestAMFToReleasePDUResources(smContext)
			if needToSendNotify {
				p.SendReleaseNotification(smContext)
			}
			if removeContext {
				// Notification has already been sent, if it is needed
				p.RemoveSMContextFromAllNF(smContext, false)
			}
		}
	})
}

func (p *Processor) requestAMFToReleasePDUResources(
	smContext *smf_context.SMContext,
) (sendNotify bool, releaseContext bool) {
	n1n2Request := models.N1N2MessageTransferRequestBody{}
	// TS 23.502 4.3.4.2 3b. Send Namf_Communication_N1N2MessageTransfer Request, SMF->AMF
	n1n2Request.JsonData = &models.Amf_Comm_N1N2MessageTransferReqData{
		PduSessionId: smContext.PDUSessionID,
		SkipInd:      true,
	}
	cause := nasie.Cause5GSM_NwFailure
	if buf, err := smf_context.BuildGSMPDUSessionReleaseCommand(smContext, cause, false); err != nil {
		logger.MainLog.Errorf("Build GSM PDUSessionReleaseCommand failed: %+v", err)
	} else {
		n1n2Request.BinaryDataN1Message = &multipart.RelatedContent{ContentID: "GSM_NAS", Content: buf}
		n1n2Request.JsonData.N1MessageContainer = &models.Amf_Comm_N1MessageContainer{
			N1MessageClass:   "SM",
			N1MessageContent: &models.RefToBinaryData{ContentId: "GSM_NAS"},
		}
	}
	if smContext.UpCnxState != models.Smf_PDUSess_UpCnxState_DEACTIVATED {
		if buf, err := smf_context.BuildPDUSessionResourceReleaseCommandTransfer(smContext); err != nil {
			logger.MainLog.Errorf("Build PDUSessionResourceReleaseCommandTransfer failed: %+v", err)
		} else {
			n1n2Request.BinaryDataN2Information = &multipart.RelatedContent{ContentID: "N2SmInformation", Content: buf}
			n1n2Request.JsonData.N2InfoContainer = &models.Amf_Comm_N2InfoContainer{
				N2InformationClass: models.Amf_Comm_N2InformationClass_SM,
				SmInfo: &models.Amf_Comm_N2SmInformation{
					PduSessionId: smContext.PDUSessionID,
					N2InfoContent: &models.Amf_Comm_N2InfoContent{
						NgapIeType: models.Amf_Comm_NgapIeType_PDU_RES_REL_CMD,
						NgapData: &models.RefToBinaryData{
							ContentId: "N2SmInformation",
						},
					},
					SNssai: smContext.SNssai,
				},
			}
		}
	}

	ctx, _, errToken := smf_context.GetSelf().GetTokenCtx(
		models.Nrf_NFMgmt_ServiceName_NAMF_COMM, models.Nrf_NFMgmt_NFType_AMF)
	if errToken != nil {
		return false, false
	}

	rspData, err := p.Consumer().
		N1N2MessageTransfer(ctx, smContext.Supi, n1n2Request, smContext.CommunicationClientApiPrefix)

	if err != nil || rspData == nil {
		logger.ConsumerLog.Warnf("N1N2MessageTransfer for RequestAMFToReleasePDUResources failed: %+v", err)
		// keep SM Context to avoid inconsistency with AMF
		smContext.SetState(smf_context.InActive)
	} else {
		switch rspData.Cause {
		case models.Amf_Comm_N1N2MessageTransferCause_N1_MSG_NOT_TRANSFERRED:
			// the PDU Session Release Command was not transferred to the UE since it is in CM-IDLE state.
			//   ref. step3b of "4.3.4.2 UE or network requested PDU Session Release for Non-Roaming and
			//        Roaming with Local Breakout" in TS23.502
			// it is needed to remove both AMF's and SMF's SM Contexts immediately
			smContext.SetState(smf_context.InActive)
			return true, true
		case models.Amf_Comm_N1N2MessageTransferCause_N1_N2_TRANSFER_INITIATED:
			// wait for N2 PDU Session Release Response
			smContext.SetState(smf_context.InActivePending)
		default:
			// other causes are unexpected.
			// keep SM Context to avoid inconsistency with AMF
			smContext.SetState(smf_context.InActive)
		}
	}
	return false, false
}
