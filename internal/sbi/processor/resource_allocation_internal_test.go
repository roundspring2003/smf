package processor

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wmnsk/go-pfcp/ie"

	"github.com/free5gc/nas/message"
	ngapie "github.com/free5gc/ngap/ie"
	"github.com/free5gc/openapi"
	"github.com/free5gc/openapi/models"
)

func TestPCFInitiatedAllocationReport(t *testing.T) {
	const id = "PccRuleId-2"
	for _, tc := range []struct {
		name       string
		pfcpCause  uint8
		ranAccepts bool
	}{
		{name: "RAN accepts the new QFI", pfcpCause: ie.CauseRequestAccepted, ranAccepts: true},
		{name: "RAN rejects the new QFI", pfcpCause: ie.CauseRequestAccepted},
		{name: "UPF rejects the allocation", pfcpCause: ie.CauseRequestRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &ueQosModPFCPClient{cause: tc.pfcpCause}
			p, sm := newUEQosModTestSession(t, client)
			sm.UpCnxState = models.Smf_PDUSess_UpCnxState_ACTIVATED

			reports := make(chan models.Pcf_SMPolCtrl_SmPolicyUpdateContextData, 2)
			pcf := newQoSAllocationTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.True(t, strings.HasSuffix(r.URL.Path, "/sm-policies/"+sm.SMPolicyID+"/update"))
				var report models.Pcf_SMPolCtrl_SmPolicyUpdateContextData
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&report))
				reports <- report
				w.WriteHeader(http.StatusNoContent)
			})
			addr := pcf.Listener.Addr().(*net.TCPAddr)
			sm.SelectedPCFProfile.NfServices[0].IpEndPoints[0].Ipv4Address = addr.IP.String()
			sm.SelectedPCFProfile.NfServices[0].IpEndPoints[0].Port = int32(addr.Port)

			transfers := make(chan models.N1N2MessageTransferRequestBody, 2)
			amf := newQoSAllocationTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				assert.True(t, strings.HasSuffix(r.URL.Path, "/n1-n2-messages"))
				assert.NotEmpty(t, client.sent(), "UPF must be updated before N1/N2 transfer")
				var transfer models.N1N2MessageTransferRequestBody
				body, err := io.ReadAll(r.Body)
				if !assert.NoError(t, err) ||
					!assert.NoError(t, openapi.Deserialize(&transfer, body, r.Header.Get("Content-Type"))) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				transfers <- transfer
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.NewEncoder(w).Encode(models.Amf_Comm_N1N2MessageTransferRspData{
					Cause: models.Amf_Comm_N1N2MessageTransferCause_N1_N2_TRANSFER_INITIATED,
				}))
			})
			sm.CommunicationClientApiPrefix = amf.URL

			decision := ueQosModTestDecision()
			qos := decision.QosDecs[ueQosModTestQosID]
			qos.Var5qi = 2
			qos.GbrUl, qos.GbrDl = "1000 Kbps", "2000 Kbps"
			qos.MaxbrUl, qos.MaxbrDl = "3000 Kbps", "4000 Kbps"
			decision.PolicyCtrlReqTriggers = []models.Pcf_SMPolCtrl_PolicyControlRequestTrigger{
				models.Pcf_SMPolCtrl_PolicyControlRequestTrigger_SUCC_RES_ALLO,
			}
			decision.LastReqRuleData = []models.Pcf_SMPolCtrl_RequestedRuleData{{
				RefPccRuleIds: []string{id},
				ReqData: []models.Pcf_SMPolCtrl_RequestedRuleDataType{
					models.Pcf_SMPolCtrl_RequestedRuleDataType_SUCC_RES_ALLO,
				},
			}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			p.HandleSMPolicyUpdateNotify(c, models.Pcf_SMPolCtrl_SmPolicyNotification{SmPolicyDecision: &decision}, sm.Ref)
			c.Writer.WriteHeaderNow()
			require.Equal(t, http.StatusNoContent, recorder.Code)

			if tc.pfcpCause == ie.CauseRequestAccepted {
				require.Empty(t, reports, "AMF acknowledgement is not a resource allocation outcome")
				var transfer models.N1N2MessageTransferRequestBody
				select {
				case transfer = <-transfers:
				default:
					t.Fatal("SMF did not send an N1/N2 modification")
				}
				info := transfer.JsonData.N2InfoContainer.SmInfo.N2InfoContent
				require.Equal(t, models.Amf_Comm_NgapIeType_PDU_RES_MOD_REQ, info.NgapIeType)
				gsm, err := message.ParseGSM(relatedBytes(transfer.BinaryDataN1Message))
				require.NoError(t, err)
				command, ok := gsm.(*message.PDUSessModCmd)
				require.True(t, ok, "expected a NAS PDU Session Modification Command")
				require.Zero(t, command.PTI)
				require.Len(t, command.AuthoQosRules.Rules, 1)
				qfi := command.AuthoQosRules.Rules[0].QFI
				require.Equal(t, sm.PCCRules[id].QFI, qfi)
				require.Len(t, command.AuthoQosFlowDescs.Descs, 1)
				require.Equal(t, uint8(2), command.AuthoQosFlowDescs.Descs[0].FiveQI)

				transferRsp := ngapie.PDUSessionResourceModifyResponseTransfer{}
				qfiID := &ngapie.QosFlowIdentifier{Value: int64(qfi)}
				if tc.ranAccepts {
					transferRsp.QosFlowAddOrModifyResponseList = &ngapie.QosFlowAddOrModifyResponseList{
						List: []ngapie.QosFlowAddOrModifyResponseItem{{QosFlowIdentifier: qfiID}},
					}
				} else {
					transferRsp.QosFlowFailedToAddOrModifyList = &ngapie.QosFlowListWithCause{
						List: []ngapie.QosFlowWithCauseItem{{
							QosFlowIdentifier: qfiID,
							Cause: &ngapie.Cause{Choice: &ngapie.CauseRadioNetwork{
								Value: ngapie.CauseRadioNetworkPresentUnspecified,
							}},
						}},
					}
				}
				receiveN2 := func(response *ngapie.PDUSessionResourceModifyResponseTransfer) {
					t.Helper()
					body, encodeErr := ngapie.MarshalBinary(response)
					require.NoError(t, encodeErr)
					ranRecorder := httptest.NewRecorder()
					ran, _ := gin.CreateTestContext(ranRecorder)
					p.HandlePDUSessionSMContextUpdate(ran, models.UpdateSmContextRequestBody{
						JsonData: &models.Smf_PDUSess_SmContextUpdateData{
							N2SmInfoType: models.Smf_PDUSess_N2SmInfoType_PDU_RES_MOD_RSP,
						},
						BinaryDataN2SmInformation: related("PDU_RES_MOD_RSP", body),
					}, sm.Ref)
					require.Equal(t, http.StatusOK, ranRecorder.Code)
				}
				receiveN2(&ngapie.PDUSessionResourceModifyResponseTransfer{})
				require.Empty(t, reports, "a response without the requested QFI must not report success")
				receiveN2(&transferRsp)
			} else {
				require.Empty(t, transfers)
			}

			require.Nil(t, sm.PendingPolicyAllocation)
			if !tc.ranAccepts {
				require.NotContains(t, sm.PCCRules, id)
				requireNoSuccessfulAllocation(t, reports)
				return
			}
			select {
			case report := <-reports:
				require.Equal(t, decision.PolicyCtrlReqTriggers, report.RepPolicyCtrlReqTriggers)
				require.Equal(t, []models.Pcf_SMPolCtrl_RuleReport{{
					PccRuleIds: []string{id},
					RuleStatus: models.Pcf_SMPolCtrl_RuleStatus_ACTIVE,
				}}, report.RuleReports)
			case <-time.After(time.Second):
				t.Fatal("PCF did not receive a successful allocation report")
			}
			require.NotZero(t, sm.PCCRuleIDToQoSRuleID[id])
		})
	}
}

func requireNoSuccessfulAllocation(t *testing.T, reports <-chan models.Pcf_SMPolCtrl_SmPolicyUpdateContextData) {
	t.Helper()
	timeout := time.NewTimer(50 * time.Millisecond)
	defer timeout.Stop()
	failures := 0
	for {
		select {
		case report := <-reports:
			require.Empty(t, report.RepPolicyCtrlReqTriggers)
			require.NotEmpty(t, report.RuleReports)
			for _, rule := range report.RuleReports {
				require.NotEmpty(t, rule.FailureCode)
			}
			// A companion failure reporter can deliver the rejection once.
			failures++
			require.LessOrEqual(t, failures, 1)
		case <-timeout.C:
			return
		}
	}
}

func newQoSAllocationTestServer(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(fn)
	server.Config.Protocols = new(http.Protocols)
	server.Config.Protocols.SetUnencryptedHTTP2(true)
	server.Start()
	t.Cleanup(server.Close)
	return server
}
