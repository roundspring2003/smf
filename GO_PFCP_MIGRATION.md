# SMF 遷移至 go-pfcp：改動盤點與內部版本核對清單

## 1. 文件目的

本文件整理 SMF 從 `github.com/free5gc/pfcp` 遷移至指定版本
`github.com/wmnsk/go-pfcp@837df543086816a27023644f27c1a31e1c0d371e`
時，需要修改、移除與新增的內容。

預定工作方式：

1. 先盤點目前外部版 SMF。
2. 查看 Saviah 內部版已完成的實作。
3. 將內部版的設計與本文件逐項對照。
4. 確認差異後，再修改外部版 SMF。

內部版已完全移除 `github.com/free5gc/pfcp`，因此外部版的最終目標也應是：

```bash
rg 'github.com/free5gc/pfcp' .
```

沒有任何輸出。

---

## 2. 遷移範圍與原則

### 2.1 最終依賴

移除：

```go
github.com/free5gc/pfcp
```

改用：

```go
github.com/wmnsk/go-pfcp/ie
github.com/wmnsk/go-pfcp/message
```

指定 commit 對應的 Go pseudo-version：

```text
v0.0.25-0.20251110163217-837df5430868
```

### 2.2 不建立舊 API 相容層

這次遷移不應重新建立一套模仿以下 package 的公開相容 API：

```text
github.com/free5gc/pfcp
github.com/free5gc/pfcp/pfcpType
github.com/free5gc/pfcp/pfcpUdp
```

SMF 的 PFCP 程式可以繼續依責任放在 `internal/pfcp` 的不同檔案或 package，
但 Message 與 IE 應直接使用 `go-pfcp/message` 與 `go-pfcp/ie`。

### 2.3 責任分界

`go-pfcp` 負責：

- PFCP Header 與 Message 型別。
- IE constructor 與欄位解析。
- Marshal、Unmarshal 與 `message.Parse()`。
- Message Type、Sequence、SEID 與 `IsRequest()` 等協定資訊。

SMF 仍需負責：

- UDP listen、read、write 與 close。
- Sequence number 配發與 24-bit wrap-around。
- Request/response transaction matching。
- Timeout、retransmission 與最大重送次數。
- 重複 request 的偵測與 response 重送。
- Heartbeat、Association 與 Session procedure。
- SMF context、UPF 狀態、logger、metrics 與 shutdown lifecycle。

---

## 3. 建議的最終程式結構

實際檔名應以 Saviah 內部版為準；外部版不必另外發明不同架構。

```text
internal/pfcp/
├── dispatcher.go              # PFCP message type dispatch
├── transaction.go             # 若內部版將 Tx/Rx transaction 放在此
├── handler/
│   └── handler.go             # incoming request handlers
├── message/
│   ├── build.go               # 使用 ie.NewXXX 組裝 message.Message
│   └── send.go                # PFCP procedure send/response handling
└── udp/
    └── udp.go                 # net.UDPConn、receive loop、server lifecycle
```

這裡的 `internal/pfcp` 是 SMF 內部功能，不是重新建立一套 `free5gc/pfcp`
相容 library。

---

## 4. 現有檔案改動盤點

目前共有 23 個 Go 檔案直接 import `github.com/free5gc/pfcp`。以下依責任分類。

### 4.1 Dependency

| 檔案 | 必要改動 |
|---|---|
| `go.mod` | 加入指定 `go-pfcp` 版本；全部遷移完成後移除 `github.com/free5gc/pfcp v1.1.2`。 |
| `go.sum` | 由 `go mod tidy` 更新；確認不再留下僅由舊 PFCP 引入的 dependency。 |

### 4.2 UDP、transaction 與 dispatch

| 檔案 | 目前耦合 | 必要改動 |
|---|---|---|
| `internal/pfcp/udp/udp.go` | `pfcp.Message`、`pfcpUdp.PfcpServer`、`pfcpUdp.Message` | 改用 `net.UDPConn` 和 `message.Message`；實作 receive loop、parse、request/response 分流、transaction matching、retry、duplicate request、close。 |
| `internal/pfcp/dispatcher.go` | free5GC Message Type constants、`pfcpUdp.Message` | 改收 `message.Message` 與 remote address；使用 `message.MsgTypeXXX` 或 concrete type switch。 |
| `internal/pfcp/udp/udp_test.go` | 舊 server/message/type | 改成 go-pfcp raw packet 與本機 UDP 測試；涵蓋 timeout、retry、response matching、duplicate request。 |

### 4.3 Message builder 與 sender

| 檔案 | 目前耦合 | 必要改動 |
|---|---|---|
| `internal/pfcp/message/build.go` | free5GC typed Message、`pfcpType`、PDR/FAR/QER/URR struct | 改用 `message.NewXXX()` 與 `ie.NewXXX()`；將每一項 PDR/FAR/QER/URR 轉成 grouped IE。 |
| `internal/pfcp/message/send.go` | 自行建立 `pfcp.Header`、`pfcp.Message`，回傳 `pfcpUdp.Message` | 直接建立 go-pfcp concrete message；呼叫新的 UDP/transaction 流程；以 `MessageType()`、`Sequence()`、`SEID()` 驗證 response。 |
| `internal/pfcp/message/build_test.go` | 驗證舊 typed PFCP body | 改驗證 concrete go-pfcp message、IE getter 與 marshal/parse round trip。 |

需要遷移的 builder/procedure：

- Association Setup Request／Response。
- Association Release Request／Response。
- Heartbeat Request／Response。
- Session Establishment Request／Response。
- Session Modification Request／Response。
- Session Deletion Request／Response。
- Session Report Response。
- Create／Update／Remove PDR。
- Create／Update／Remove FAR。
- Create QER。
- Create／Update／Remove／Query URR。
- Create BAR。
- Usage Report parsing。

### 4.4 Incoming handler

| 檔案 | 目前耦合 | 必要改動 |
|---|---|---|
| `internal/pfcp/handler/handler.go` | `pfcpUdp.Message`、free5GC body type assertion、`pfcpType.Cause` | 改對 concrete go-pfcp message 做 type assertion；使用 IE getter 取得 NodeID、Cause、ReportType、UsageReport 等值；補齊 getter error 與 missing IE 處理。 |
| `internal/pfcp/handler/handler_test.go` | 建立舊 PFCP request | 改用 `message.NewXXX()` 與 `ie.NewXXX()` 建立輸入；增加 malformed/missing mandatory IE 測試。 |

應特別確認：

- Response 必須沿用 request sequence number。
- Session Report Request 使用 Header SEID 找 SM context。
- NodeID、ReportType、URRID、UsageReportTrigger 缺少時不可 panic。
- IE getter 回傳的 error 必須處理。

### 4.5 SMF context 與 PFCP rule model

這一區是遷移量最大的部分。不能只換 import，因為 free5GC `pfcpType` 是 typed struct，
go-pfcp 則以 `*ie.IE` 與 getter 表示協定欄位。

| 檔案 | 主要內容 | 必要改動 |
|---|---|---|
| `internal/context/context.go` | SMF PFCP 設定與 CP NodeID | NodeID 改成內部 primitive/domain type，或依內部版直接使用 go-pfcp 可接受的表示法。 |
| `internal/context/upf.go` | UPF NodeID、PFCP address、association 狀態 | 移除 `pfcpType.NodeID`、`pfcpUdp`；重寫 NodeID resolve、比較與 key 產生。 |
| `internal/context/user_plane_information.go` | UPF topology 與 NodeID | 調整 NodeID 建立、lookup 與比較。 |
| `internal/context/pfcp_session_context.go` | Local/Remote SEID | 移除舊 PFCP type；保持 session context 與 SEID mapping 行為。 |
| `internal/context/pfcp_rules.go` | PDR、PDI、FAR、QER、URR、BAR | 移除 `pfcpType` 欄位；改成 SMF domain primitive/struct，或依內部版採用的型別。 |
| `internal/context/pfcp_reports.go` | Usage Report | 使用 go-pfcp IE getter 解析 Volume、Duration、Trigger、URRID。 |
| `internal/context/datapath.go` | Source/Destination Interface、FTEID | 改用內部值型別，建立 PFCP message 時再轉成 IE。 |
| `internal/context/ngap_handler.go` | PFCP rule 更新 | 移除舊 `pfcpType` constructor/constant。 |
| `internal/context/sm_context.go` | SM context 內 PFCP 欄位 | 移除所有舊 PFCP type reference。 |
| `internal/context/upf_test.go` | UPF/NodeID test fixture | 改成新 NodeID/domain model。 |

需從內部版確認 context 最終採用哪一種策略：

1. context 保存 primitive/domain struct，builder 才轉 `*ie.IE`；或
2. context 直接保存部分 `*ie.IE`。

外部版應跟隨內部版，避免產生第三種模型。

### 4.6 SBI processor 與 procedure

| 檔案 | 必要改動 |
|---|---|
| `internal/sbi/processor/association.go` | Heartbeat/Association response 改成 go-pfcp concrete message；重寫 Cause、Recovery Timestamp、NodeID 解析。 |
| `internal/sbi/processor/datapath.go` | Establishment/Modification/Deletion response 改用 go-pfcp IE getter；處理 CreatedPDR、FSEID、Cause、Usage Report。 |
| `internal/sbi/processor/charging_trigger.go` | Query/Update URR 及 Usage Report 改成新 IE model。 |
| `internal/sbi/processor/pdu_session.go` | 移除 `pfcpType` constant/type，改用新 domain model 或 go-pfcp constant。 |
| `internal/sbi/processor/ulcl_procedure.go` | NodeID、PFCP response 與 rule type 改成新模型。 |

---

## 5. 必須新增或補回的功能

以下功能原本由 `free5gc/pfcp/pfcpUdp` 或 `free5gc/pfcp/transaction.go` 提供，
指定版本的 go-pfcp 不會代為處理。

### 5.1 PFCP server lifecycle

- 綁定 SMF PFCP listen IP 與 UDP 8805。
- 保存 `*net.UDPConn`。
- 啟動 receive goroutine／event loop。
- 記錄 recovery/start time。
- 支援 context cancellation 與 graceful close。
- close 後 pending transaction 必須解除等待。

### 5.2 Receive path

- 每個 datagram 複製成獨立 buffer，避免下一次 read 覆蓋。
- 使用 `message.Parse(buf[:n])`。
- 驗證 packet/header length 是否一致。
- 遇到 unknown 或 malformed message 時記錄並丟棄，不可 panic。
- request 送 dispatcher；response 送到對應 Tx transaction。

### 5.3 Sequence number

- thread-safe 配發 sequence number。
- PFCP sequence number 僅有 24 bits。
- wrap-around 時不得與仍存在的 transaction 衝突。
- Response 沿用收到的 Request sequence number，不另外配置。

### 5.4 Tx transaction

- 建議 key 至少包含 remote address 與 sequence number。
- 保存原始 request 與 marshaled bytes。
- 設定 retransmission timer。
- timeout 時重送相同 bytes。
- 到達最大次數後刪除 transaction、釋放 sequence，並以 `Msg == nil` 通知等待者 timeout。
- 收到 response 時停止 timer、刪除 transaction、釋放 sequence、通知等待者。
- timer callback 綁定原 Tx object 並檢查 generation；stale callback 不得作用於 reset 或同 key replacement transaction。
- timer callback panic 採 transaction-level recovery：記錄 error/stack、清除該 Tx、釋放 sequence，並以 timeout 通知解除 `RspCh` 等待；不終止整個 SMF。
- Tx map cleanup 使用 object-aware `CompareAndDelete`，舊 callback 不得刪除 replacement 或錯誤釋放其 sequence。
- 檢查 response type 是否與 request 對應。
- Session message 檢查 SEID。

### 5.5 Rx transaction

- 以 remote address 與 sequence number 識別重複 request。
- 保存已送出的 response bytes。
- 收到重複 request 時重送相同 response，不重跑 handler。
- request 在 dispatch queue 等待時保留 timeout；worker 取出時先 claim transaction，已逾時的 stale queue item 不執行 handler。
- handler 開始後暫停 queue timeout，避免慢 handler 處理途中失去 transaction；送出 response 時從送出時間重新計算 response cache timeout。
- timeout 後清除尚未處理的 request 或已送出的 response cache。

### 5.6 Message/IE conversion helper

應依 Saviah 內部版決定是否建立 helper。可能需要：

- NodeID string/IP/FQDN 與 `ie.NewNodeID` 的轉換。
- F-SEID、F-TEID getter 與 domain struct 轉換。
- Source/Destination Interface constant mapping。
- Outer Header Creation/Removal flags mapping。
- Apply Action flags mapping。
- Gate Status、MBR、GBR、QFI mapping。
- Reporting Trigger、Usage Report 與 Volume mapping。
- Cause constant mapping。

原則是集中處理容易出錯的 flag/bit mapping，但不要重建一份完整 `pfcpType`
相容 package。

---

## 6. 查看 Saviah 內部版時的核對清單

以下項目應逐項記錄內部版的檔案位置與做法。

### 6.1 Dependency 與整體結構

- [ ] 使用的 go-pfcp commit/version 是否與任務指定版本一致。
- [ ] `go.mod` 是否完全移除 `github.com/free5gc/pfcp`。
- [ ] PFCP server、transaction、builder、handler 分別放在哪些檔案。
- [ ] 是否參考或共用 go-upf 的 PFCP 實作。
- [ ] 是否有未公開或內部共用 package 需要一併帶出。

### 6.2 UDP server

- [ ] listen address 與 port 如何設定。
- [x] request dispatch 採固定大小 worker pool；handler 同步在 worker 內執行，不為每筆 request 建立 goroutine。
- [x] dispatcher panic 採 request-level isolation，不再 `Fatalf` 終止 SMF；固定 worker recovery 後繼續下一筆工作。
- [x] shutdown 在 channel select 後再次檢查 `stopCh`，避免 stop 與 queued request 同時 ready 時開始新 handler。

Worker 數可在 PFCP config 設定；省略或設為 `0` 時使用預設 `64`，允許範圍為 `1..1024`：

```yaml
configuration:
  pfcp:
    dispatchWorkerCount: 64
```
- [ ] maximum PFCP packet size。
- [ ] buffer 是否在送進 channel 前複製。
- [ ] shutdown 如何通知 receive loop 與 pending request。
- [ ] UDP read error、closed connection、temporary error 如何處理。

### 6.3 Transaction

- [ ] Tx/Rx transaction struct 定義。
- [x] transaction key 採 Saviah 作法：`RemoteIP-Sequence`，不包含 UDP port。
- [ ] transaction map 是否需要 mutex，或只由單一 event loop存取。
- [ ] retransmission timeout 與 max retransmission 來源。
- [x] response 到達後透過 Tx `RspCh` 喚醒同步等待中的 sender。
- [x] timeout 或 Tx timer panic 以 `RcvPfcpMsg{Msg: nil}` 回傳，並關閉 `RspCh`。
- [x] duplicate request 由 Rx transaction 重送 cached response，不重跑 handler。
- [x] queued request timeout 後由 worker claim 失敗並略過，避免執行 stale handler。
- [x] handler 執行期間暫停 Rx timer；response 後 reset cache timer；shutdown 強制 cleanup。

### 6.4 Sequence 與 header

- [ ] sequence 是否限制為 24 bits。
- [ ] sequence wrap-around 的處理方式。
- [ ] Node-level message 的 S flag、SEID、priority 設定。
- [ ] Session-level message 的 S flag、SEID、priority 設定。
- [ ] response 是否沿用 request sequence。

### 6.5 Message builder

- [ ] Association Setup／Release 的 IE 清單。
- [ ] Heartbeat 的 Recovery Time Stamp。
- [ ] Session Establishment 的 NodeID、CP F-SEID、Create PDR/FAR/QER/URR/BAR。
- [ ] Session Modification 的 Update/Remove/Create rule mapping。
- [ ] Session Deletion 與 Usage Report。
- [ ] Session Report Request／Response 的 DLDR、USAR 等 flags。
- [ ] grouped IE 是集中 builder 還是直接在 procedure 中建立。

### 6.6 Context model

- [ ] NodeID 在 context 中使用 string、`net.IP`、domain struct 或 `*ie.IE`。
- [ ] PDR/PDI/FAR/QER/URR/BAR 是否仍保存 codec-specific type。
- [ ] FTEID/FSEID 如何保存與轉換。
- [ ] ApplyAction、ReportingTrigger 等 flags 如何表示。
- [ ] 是否有可以直接移植到外部版的 domain model 修改。

### 6.7 Error handling 與安全性

- [ ] missing mandatory IE 是否回傳正確 Cause。
- [ ] 所有 IE getter error 是否處理。
- [ ] malformed packet 是否可能造成 panic。
- [ ] remote address 與 sequence spoofing 的處理。
- [ ] unexpected response type、SEID、NodeID 的處理。
- [ ] transaction channel 是否可能 block 或被重複 close。

### 6.8 Tests

- [ ] UDP request/response 測試。
- [ ] timeout/retransmission 測試。
- [ ] duplicate request/response cache 測試。
- [ ] marshal/parse round-trip 測試。
- [ ] Association integration test。
- [ ] Session Establishment/Modification/Deletion test。
- [ ] Session Report/Usage Report test。
- [ ] malformed/missing IE test。
- [ ] race test。

---

## 7. 內部版對照紀錄

查看 Saviah 內部版後，在此填入結果，作為外部版實作依據。

| 項目 | Saviah 內部版檔案/做法 | 外部版採用方式 | 差異或注意事項 |
|---|---|---|---|
| go-pfcp version | 待確認 | 指定 commit | |
| PFCP server | 待確認 | 待確認 | |
| Receive loop | 待確認 | 待確認 | |
| Tx transaction | 待確認 | 待確認 | |
| Rx transaction | 待確認 | 待確認 | |
| Sequence allocation | 待確認 | 待確認 | |
| Retry/timeout | 待確認 | 待確認 | |
| Duplicate request | 待確認 | 待確認 | |
| NodeID model | 待確認 | 待確認 | |
| PDR/FAR/QER/URR model | 待確認 | 待確認 | |
| Message builders | 待確認 | 待確認 | |
| Handler parsing | 待確認 | 待確認 | |
| Shutdown | 待確認 | 待確認 | |
| Tests | 待確認 | 待確認 | |

---

## 8. 建議外部版實作順序

查看並確認內部版後，建議按照以下順序修改，避免同時打破所有 PFCP procedure。

### Phase 0：建立 baseline

- 記錄目前完整 test 結果。
- 保存關鍵 PFCP message 的 raw bytes/golden fixtures。
- 記錄與目前 UPF 的 Association、Heartbeat、Session flow。

### Phase 1：移植 server 與 transaction

- 將 Saviah 內部版已驗證的 UDP server、Tx/Rx transaction 移至外部版對應位置。
- 先完成 `message.Parse()`、request/response dispatch、timeout/retry、duplicate request。
- 不在這一階段自行改變 timeout/retry policy。

### Phase 2：Heartbeat

- [x] 被動方向：新 dispatcher 接收 go-pfcp `HeartbeatRequest`，由 handler 回覆 `HeartbeatResponse`。
- [x] 被動方向：驗證 response 沿用 request sequence，並攜帶 SMF Recovery Time Stamp。
- [x] 主動方向 foundation：SMF Heartbeat Request sender、transaction response matching，以及 concrete response/Recovery Time Stamp validation。
- [ ] 使用實際 UPF 驗證 Heartbeat timeout、retry 與 Recovery Time Stamp 變更行為。

### Phase 3：Association

- [x] 被動方向 foundation：go-pfcp Association Setup／Release dispatch、mandatory IE 驗證與 Cause response。
- [x] 主動方向 foundation：Association Setup／Release send、transaction matching 與 concrete response/Cause validation。
- [x] Node ID 的 IPv4／IPv6／FQDN IE ↔ `pfcptype.NodeID` 轉換。
- [x] `AssociationStateManager` 注入介面與 response 寫出後才執行的 `afterResponse` hook。
- [ ] Runtime wiring：由 startup 上層注入 adapter，將 setup/release 寫入現有 `context.UPF` association state。
- [ ] UPF restart recovery：實作/確認 Saviah `afterResponse` 對應的 session recovery 行為。
- [ ] 使用實際 go-upf 驗證雙向 Association Setup／Release。

### Phase 4：Session basic procedures

- 先遷移 Session Deletion。
- 再遷移 Session Establishment。
- 最後遷移 Session Modification。
- 每個 procedure 分別測試 Cause、SEID、CreatedPDR 與 F-SEID。

### Phase 5：Session Report 與 Charging

- 遷移 Downlink Data Report。
- 遷移 Usage Report、URRID、UsageReportTrigger 與 volume/duration。
- 驗證與 CHF charging/update quota 流程。

### Phase 6：清除 context 舊型別

- 移除剩餘 `pfcpType`。
- 統一 NodeID、FTEID、rule 與 report domain model。
- 刪除不再使用的 conversion/workaround。

### Phase 7：移除舊 dependency

```bash
rg 'github.com/free5gc/pfcp' .
go mod tidy
go test ./...
go test -race ./internal/pfcp/...
```

---

## 9. 驗證項目

### 9.1 Unit test

- Message builder marshal/parse round trip。
- IE 必填欄位與 flag mapping。
- Sequence allocator。
- Tx/Rx transaction lifecycle。
- Timeout/retry/cleanup。
- Tx/Rx timer panic 只清除所屬 transaction；stale generation/replacement 不受影響。
- Duplicate request。
- Dispatch concurrency 不超過 worker 上限。
- Dispatch queue overflow 會釋放未接納的 Rx transaction。
- Dispatch queue 內已逾時的 request 不會執行 handler。
- 已被 worker claim 的 request 即使 handler 慢於 queue timeout，transaction 仍維持有效。
- handler 在 response 前 panic 時只 abort unanswered Rx transaction，且單一 fixed worker 仍可處理下一筆 request。
- response 後／`afterResponse` panic 時保留 duplicate-response cache，不重跑 handler。
- stopCh 關閉後不再開始 queued handler。
- malformed packet 與 missing IE。

### 9.2 Integration test

- SMF 啟動並 bind PFCP UDP 8805。
- SMF 與 go-upf Association Setup 成功。
- Heartbeat 持續正常。
- PDU Session Establishment 成功。
- Session Modification 成功。
- Session Deletion 成功。
- UPF Session Report 能被 SMF 處理並回覆。
- Charging Usage Report 流程正常。
- Usage Report storm 下 worker/goroutine 數維持上限，queue/heap 進入 steady state。
- SMF shutdown 無 goroutine、timer、socket leak。

### 9.3 Wire compatibility

使用 Wireshark/tcpdump 比較遷移前後：

- Message Type。
- S/MP flags。
- Sequence number。
- SEID。
- IE type、instance、length 與 value。
- Retransmission 時序。
- Response 是否對應正確 request。

---

## 10. 完成條件

- [ ] `go.mod` 不再依賴 `github.com/free5gc/pfcp`。
- [ ] 程式與測試中沒有 `github.com/free5gc/pfcp` import。
- [ ] 不存在為了相容舊 API 而重建的 `pfcpType`/`pfcpUdp` facade。
- [ ] 所有 PFCP Message/IE 使用指定版本的 go-pfcp。
- [ ] Heartbeat、Association、Session Establishment、Modification、Deletion、Report 全部通過。
- [x] Timeout、retry、duplicate request 與 bounded dispatch 行為有 foundation unit test。
- [ ] malformed packet 與 missing mandatory IE 不會造成 panic。
- [ ] `go test ./...` 通過。
- [ ] `go test -race ./internal/pfcp/...` 通過。
- [ ] 與實際 go-upf 的端對端測試通過。

---

## 11. 參考

- 指定 go-pfcp commit：<https://github.com/wmnsk/go-pfcp/tree/837df543086816a27023644f27c1a31e1c0d371e>
- go-pfcp Message API：<https://github.com/wmnsk/go-pfcp/blob/837df543086816a27023644f27c1a31e1c0d371e/message/message.go>
- go-pfcp Heartbeat UDP 範例：<https://github.com/wmnsk/go-pfcp/blob/837df543086816a27023644f27c1a31e1c0d371e/examples/heartbeat/hb-server/main.go>
- 外部版可參考既有 `go-upf/internal/pfcp` 的 server 與 transaction 設計，但最後應優先與 Saviah 內部 SMF 對齊。
