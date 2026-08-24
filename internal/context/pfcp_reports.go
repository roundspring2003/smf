package context

import (
	"fmt"

	"github.com/free5gc/openapi/models"
	"github.com/wmnsk/go-pfcp/ie"

	"github.com/free5gc/smf/internal/logger"
	"github.com/free5gc/smf/internal/pfcp/pfcptype"
)

// HandleReports converts grouped go-pfcp Usage Report IEs into the charging
// domain records consumed by the CHF flow. The same parser is used for Session
// Modification Response, Session Deletion Response and Session Report Request.
func (smContext *SMContext) HandleReports(
	reports []*ie.IE,
	nodeID pfcptype.NodeID,
	reportType models.Chf_ConvCharging_TriggerType,
) error {
	upf := RetrieveUPFNodeByNodeID(nodeID)
	if upf == nil {
		return fmt.Errorf("UPF for Node ID %s not found", nodeID.String())
	}

	// Decode the whole set before mutating charging state. A malformed report
	// must not leave a partially-applied batch behind.
	decoded := make([]UsageReport, 0, len(reports))
	for index, grouped := range reports {
		report, err := usageReportFromIE(grouped, upf.UUID(), reportType)
		if err != nil {
			return fmt.Errorf("Usage Report[%d]: %w", index, err)
		}
		decoded = append(decoded, report)
	}
	for _, report := range decoded {
		logger.PduSessLog.Tracef(
			"[HandleReports] URRID=%d, UpfId=%s, ReportType=%s, TotalVol=%d, UlVol=%d, DlVol=%d",
			report.UrrId, report.UpfId, report.ReportTpye, report.TotalVolume,
			report.UplinkVolume, report.DownlinkVolume,
		)
		smContext.UrrReports = append(smContext.UrrReports, report)
	}
	return nil
}

func usageReportFromIE(
	grouped *ie.IE,
	upfID string,
	override models.Chf_ConvCharging_TriggerType,
) (UsageReport, error) {
	if grouped == nil {
		return UsageReport{}, fmt.Errorf("nil grouped IE")
	}
	children, err := grouped.UsageReport()
	if err != nil {
		return UsageReport{}, fmt.Errorf("decode grouped IE: %w", err)
	}

	var urrIDIE, volumeIE, triggerIE *ie.IE
	for _, child := range children {
		if child == nil {
			continue
		}
		switch child.Type {
		case ie.URRID:
			urrIDIE = child
		case ie.VolumeMeasurement:
			volumeIE = child
		case ie.UsageReportTrigger:
			triggerIE = child
		}
	}
	if urrIDIE == nil {
		return UsageReport{}, fmt.Errorf("missing URR ID")
	}
	urrID, err := urrIDIE.URRID()
	if err != nil {
		return UsageReport{}, fmt.Errorf("decode URR ID: %w", err)
	}
	if triggerIE == nil {
		return UsageReport{}, fmt.Errorf("missing Usage Report Trigger for URR ID %d", urrID)
	}
	if _, err = triggerIE.UsageReportTrigger(); err != nil {
		return UsageReport{}, fmt.Errorf("decode Usage Report Trigger for URR ID %d: %w", urrID, err)
	}

	report := UsageReport{
		UrrId:      urrID,
		UpfId:      upfID,
		ReportTpye: identityTriggerType(triggerIE),
	}
	if override != "" {
		report.ReportTpye = override
	}
	if volumeIE == nil {
		logger.PduSessLog.Warnf("Usage Report missing Volume Measurement for URRID[%d]", urrID)
		return report, nil
	}
	volume, err := volumeIE.VolumeMeasurement()
	if err != nil {
		return UsageReport{}, fmt.Errorf("decode Volume Measurement for URR ID %d: %w", urrID, err)
	}
	report.TotalVolume = volume.TotalVolume
	report.UplinkVolume = volume.UplinkVolume
	report.DownlinkVolume = volume.DownlinkVolume
	report.TotalPktNum = volume.TotalNumberOfPackets
	report.UplinkPktNum = volume.UplinkNumberOfPackets
	report.DownlinkPktNum = volume.DownlinkNumberOfPackets
	return report, nil
}

func identityTriggerType(trigger *ie.IE) models.Chf_ConvCharging_TriggerType {
	switch {
	case trigger.HasVOLTH():
		return models.Chf_ConvCharging_TriggerType_QUOTA_THRESHOLD
	case trigger.HasVOLQU():
		return models.Chf_ConvCharging_TriggerType_QUOTA_EXHAUSTED
	case trigger.HasQUVTI():
		return models.Chf_ConvCharging_TriggerType_VALIDITY_TIME
	case trigger.HasSTART():
		return models.Chf_ConvCharging_TriggerType_START_OF_SERVICE_DATA_FLOW
	case trigger.HasIMMER():
		logger.PduSessLog.Trace("Reports Query by SMF, trigger should be filled later")
		return ""
	case trigger.HasTERMR():
		return models.Chf_ConvCharging_TriggerType_FINAL
	default:
		logger.PduSessLog.Trace("Report is not a charging trigger")
		return ""
	}
}
