# GO_PFCP_MIGRATION.md 核對結果與外部版修改想法

> 本文件是針對 `GO_PFCP_MIGRATION.md` 的回答。已比對 Saviah 內部版
> (`/home/alonza/smf`，已完全遷移) 與外部版 (`/home/alonza/smf/smf-opensource`，
> 尚在使用 `github.com/free5gc/pfcp v1.1.2`) 的實際程式碼。本文件只做分析與建議，
> 未修改任何程式碼。

---

## 0. 結論摘要

1. **內部版確實已完全移除 `free5gc/pfcp`**：`rg 'github.com/free5gc/pfcp'` 在
   `/home/alonza/smf`（排除 `smf-opensource/` 子目錄）沒有任何輸出；外部版仍有
   23 個檔案 import，與文件第 4 節盤點的檔案清單完全一致。
2. **go-pfcp 版本一致**：內部版 `go.mod` 已釘選
   `github.com/wmnsk/go-pfcp v0.0.25-0.20251110163217-837df5430868`，
   與文件指定的 commit `837df543086816a27023644f27c1a31e1c0d371e` 相符，可直接照抄。
3. **內部版的實際檔案結構跟文件第 3 節「建議的最終程式結構」不一樣**——
   內部版**沒有** `internal/pfcp/handler/`、`internal/pfcp/message/`、
   `internal/pfcp/udp/` 這三個子目錄，也沒有獨立的 `internal/pfcp/transaction.go`
   之外的 tx/rx 分離檔。實際切法是：
   - `internal/pfcp/`：UDP server 生命週期、tx/rx transaction、sequence、
     **被動方向**（SMF 收到 UPF 的 Request 後如何組 Response）的 handler，
     全部留在 `internal/pfcp` 這個 flat package 裡。
   - `internal/context/`：**主動方向**（SMF 主動送給 UPF 的 Request，例如
     Association Setup、Heartbeat、Session Establishment/Modification/Deletion）
     的 message builder（`pfcp_build.go`）、送出與收 Response 比對
     （`pfcp_send.go`）、收到 Response 後的業務處理（`pfcp_handler.go`），
     以及 PDR/FAR/QER/URR domain model（`pfcp_rules.go`）、SEID/NodeID 等
     context 資料本身。
   - `internal/pfcp/pfcpType/`：SMF 自己的 domain type package（不是
     `free5gc/pfcp/pfcpType` 的相容層，是全新命名相同、內容重新設計的
     package），見第 2 節說明。

   建議外部版**不要**照抄文件第 3 節那個目錄樹，改成貼近內部版「被動方向留在
   `internal/pfcp`、主動方向 + domain model 放進 `internal/context`」的切法，
   理由與細節見第 4 節。

---

## 1. 內部版對照紀錄（回填文件第 7 節表格）

| 項目 | Saviah 內部版檔案/做法 | 外部版建議採用方式 | 差異或注意事項 |
|---|---|---|---|
| go-pfcp version | `go.mod:27` `v0.0.25-0.20251110163217-837df5430868` | 完全比照 | 版本一致，無需另外驗證 |
| PFCP server | `internal/pfcp/server.go` `PfcpServer` struct，持有 `*net.UDPConn`、`rcvCh`（有界 channel + overload drop）、`txCh`/`dispCh`（`uchan.UnboundedChan`）、`txTrans`/`rxTrans`（`sync.Map`） | 在外部版對應位置（可保留 `internal/pfcp/udp/udp.go` 或整併成單一 `server.go`）重建同等能力；channel 三層分工（rcv/tx/disp）不是必要設計，但 overload drop 策略建議保留精神 | 內部版把 UDP 生命週期、tx/rx transaction、dispatcher 全放同一 package，沒有 `udp/` 子目錄 |
| Receive loop | 單一 `receiver()` goroutine 呼叫 `conn.ReadFromUDP`，每個 datagram **複製成獨立 buffer** 後才 `message.Parse()`（`server.go:271-299`），未知型別（`*message.Generic`）直接丟棄記錄 warning，不 dispatch | 完全比照，這是必須的正確性要求（buffer 重用會造成 race） | `server.go:281-282` 的 `copy(msgBuf, buf)` 是關鍵，外部版容易漏掉 |
| Tx transaction | `internal/pfcp/transaction.go:52-171` `TxTransaction`，key 為 `TransactionID(addr, seq) = "<addr.IP>-<seq#x>"`（`transaction.go:48-50`，**只用 IP，不含 port**），`time.AfterFunc` 做 retransmit timer，`mu sync.Mutex` 保護 req/msgBuf/timer | 完全比照邏輯；key 是否要含 port 需依外部版 UPF 是否可能用不同來源 port 送 response 決定 | maxRetrans/timeout 來自 `s.Config().GetPfcpRetransTimer()`（`transaction.go:61-62`），外部版需自行決定設定來源（config 檔或寫死常數） |
| Rx transaction | `transaction.go:216-305` `RxTransaction`，收到重複 request 時直接重送 cached `msgBuf`（`recv()` `transaction.go:268-289`），不重跑 handler；timeout 後由 `handleTimeout()` 刪除（不重送 timeout 通知，因為 Rx 沒有上層等待者） | 完全比照 | `rx.timeout = expTime * (maxRetry+1)`（`transaction.go:227`），比 Tx 的單次 retransTimeout 長，確保 Rx 存活時間覆蓋對方所有 retransmission |
| Sequence allocation | `idgenerator.NewGenerator(0x0, 0xFFFFFF)`（`server.go:97`），3 bytes = 24-bit，`txSeqGen.FreeID()` 在 `DeleteTxTransaction` 時釋放（`transaction.go:79-84`） | 完全比照，24-bit 上限與 free/allocate 生命週期是正確性關鍵 | 用的是內部 util package `idgenerator`，外部版若無此 lib 需自己實作 wrap-around allocator |
| Retry/timeout | Timer 到期呼叫 `TransTimeout(trType, trID)`（`transaction.go:173-214`），內部先檢查 `s.stopCh` 是否已關閉（`abort` flag）避免 shutdown 後還在重送；達到 `maxRetrans` 後刪除 transaction 並對 `rspCh` 送 `RcvPfcpMsg{Msg: nil}` 通知上層 timeout（`transaction.go:151-160`） | 完全比照；「送 nil Msg 進 channel 代表 timeout」這個約定必須讓所有呼叫端一致遵守 | 呼叫端統一寫法：`if rcvPkt.Msg == nil { return nil, errors.Errorf("...timeout") }`（`internal/context/pfcp_send.go` 每個 `SendPfcp*Request` 都重複這段，例如 `:95-96,140-141,381-382` 等） |
| Duplicate request | 見上「Rx transaction」列 | 同上 | — |
| NodeID model | `pfcpType.NodeID{NodeIdType uint8; IP net.IP; FQDN string}`（`internal/pfcp/pfcpType/objects.go:83-98`）是 **context 內唯一權威表示法**；`SMFContext.CPNodeID`（`context.go:89`）、`UPF.NodeID`（`upf.go:62`）、`PFCPSessionContext.NodeID`（`pfcp_session_context.go:45`）都存這個型別。轉成 `*ie.IE` 只在組 message 當下做，唯一入口是 `NewNodeIE(nfCtx UPTunnelNFContext) *ie.IE`（`context.go:50-56`，內部呼叫 `ie.NewNodeIDHeuristic(...)`） | 外部版採同一策略：**context 存 domain struct，builder 才轉 `*ie.IE`**（文件 4.5 節列的兩個選項中的選項 1） | 收到對方 NodeID 時（例如 Association Setup Request 的 `req.NodeID`），走反向 helper `toNodeId(nodeId *ie.IE) *pfcpType.NodeID`（`internal/pfcp/association.go:108-132`）；但 Response 側的 `pfcp_handler.go` 只用 `rsp.NodeID.NodeID()` 取字串做 log，並未寫回 `pfcpType.NodeID`，屬於非對稱設計，外部版可視需要選擇是否補齊 |
| PDR/FAR/QER/URR model | Domain struct 定義在 `internal/context/pfcp_rules.go`；轉換函式全部集中在 `internal/context/pfcp_build.go`：`toPdr/toFar/toQer/toUrr/toBAR` + `xxxToCreateXXX`/`xxxToUpdateXXX` wrapper，用 `ops string`（`CREATE_OPS`/`UPDATE_OPS`）共用同一組轉換邏輯（`pfcp_build.go:14-17` 及各函式） | 外部版比照「集中在一個 build 檔、用 ops 常數區分 create/update」的模式，不要為 create 和 update 各寫一份重複邏輯 | QER/URR 在組 Session Establishment 時會先用 `map[uint32]*QER`/`map[uint32]*URR` 依 ID 去重（`pfcp_build.go:577-595`），因為同一個 QER/URR 可能被多個 PDR 引用——這是正確性要求，不是內部版特有的最佳化，外部版必須保留 |
| Message builders | Session Establishment/Modification 有獨立 `BuildPfcpSessionEstablishmentRequest`/`BuildPfcpSessionModificationRequest`（`pfcp_build.go:540-600, 602-689`）；Association Setup/Release、Heartbeat、Session Deletion 則是**直接 inline 寫在 `pfcp_send.go` 的 `SendPfcpXxxRequest` 函式裡**，沒有獨立 Build 函式 | 外部版可以選擇統一都拆成獨立 Build 函式（比文件建議的 `message/build.go` 更一致），或比照內部版只在複雜度高（有 rule list）時才拆——兩種都合理，重點是不要把 build 邏輯和 transaction/send 邏輯混在同一層 | 內部版這個不一致其實是歷史包袱，外部版重新設計時可以做得比內部版更乾淨 |
| Handler parsing | 兩層：①`internal/pfcp` 內處理**被動收到的 Request**（Heartbeat/Association/SessionReport，見 `dispatcher.go`, `association.go`, `report.go`）；②`internal/context/pfcp_handler.go` 處理**主動送出後收到的 Response**（`HandlePfcpAssociationSetupResponse` 等 6 個函式，`pfcp_handler.go:79-424`）。兩層一致採用「先 nil 檢查 mandatory IE → 再呼叫 getter 檢查 error → Cause 一律比對 `ie.CauseRequestAccepted`」的模式 | 外部版比照兩層分工與統一的 IE 檢查模式 | 見第 5 節「內部版本身尚未完成/有風險的地方」——`HandlePfcpSessionEstablishmentResponse` 目前**沒有處理 `CreatedPDR`**（UPF 選的 F-TEID 讀不回來），外部版若需要這個功能不能照抄，要自己補 |
| Shutdown | `PfcpServer.Stop()` 關 `stopCh` + `conn.Close()`（`server.go:150-158`）；`main()` 的 defer 呼叫 `stopTrTimers()`（`server.go:449-479`）走過所有 pending tx/rx transaction，停掉 timer 並對還在等待的 `rspCh` 送 `{Msg: nil}` 後 `close()`，確保沒有 goroutine 卡在等 channel | 完全比照；這是文件 5.1「close 後 pending transaction 必須解除等待」的具體實作範例 | 每個 goroutine（`Run`/`main`/`receiver`/`dispatcher`）都包了 `recover()` + `Fatalf`，panic 不會被吞掉但也不會讓整個程序處於未知狀態；外部版建議保留這個 pattern |
| Tests | `internal/pfcp/server_test.go`（`TestTxTransactionRaceOnServerClose`, `TestTransaction`）、`server_unbounded_test.go`（overload drop / type assertion 相關 6 個測試）、`report_test.go`、`dump_test.go`；另有 `internal/context/pfcp_build_test.go`, `pfcp_send_test.go`, `pfcp_handler_test.go`, `pfcp_session_context_test.go` | 外部版至少要涵蓋文件 6.8 列的全部項目；`reliable_pfcp_request_test.go` 這個檔名兩邊都有，但**內容整份被註解掉、是空測試**（見第 5 節），不要照抄內容，只是提醒這個檔名/測試意圖是舊的 | race test 對應 `TestTxTransactionRaceOnServerClose`，用 `go test -race` 才有意義 |

---

## 2. `pfcpType` 這個 package：為什麼不算違反「不建立相容層」原則

文件 2.2 節明確禁止的是重建**可 import 相容替換** `github.com/free5gc/pfcp`、
`pfcpType`、`pfcpUdp` 的 facade（也就是讓舊 import path 或舊 API 簽名繼續能用）。

內部版在 `internal/pfcp/pfcpType/objects.go` 建了一個**全新、SMF 自己 own** 的
package，恰好也叫 `pfcpType`，內容也是 `NodeID`/`Cause`/`FTEID`/`GateStatus`/
`MBR`/`ApplyAction`/`OuterHeaderCreation`/`OuterHeaderRemoval`/`SourceInterface`/
`DestinationInterface`/`PDNType`/`QFI`/`GBR`/`VLANTag`/`SDFFilter`/
`EthernetPacketFilter`/`UEIPAddress`/`DownlinkDataNotificationDelay`/
`SuggestedBufferingPacketsCount` 這些型別——概念上很像舊版 `free5gc/pfcp/pfcpType`，
但：

- import path 完全不同（`internal/pfcp/pfcpType`，不是任何外部 module）；
- 欄位/常數是重新設計的（例如 `Cause` 常數值跟 go-pfcp 的 `ie.CauseXxx` 是兩套
  平行系統，實際組 message 時用的是 `ie.CauseXxx`，`pfcpType.Cause` 反而較少被用到）；
- 用途是**SMF 內部 context 儲存用的 domain model**，不是拿來讓呼叫端誤以為在用
  舊 library。

這正是文件第 4.5 節開放的兩個選項之一（「context 保存 primitive/domain struct，
builder 才轉 `*ie.IE`」），**外部版應該採用同一策略**：可以沿用
`pfcpType` 這個名字（貼合內部版方便日後對照），也可以取別的名字，但核心是
「domain struct 只在 SMF 內部使用、跟 go-pfcp 的型別完全解耦、轉換只在
build/parse 邊界發生」。

---

## 3. 逐項核對清單回答（對應文件第 6 節）

### 6.1 Dependency 與整體結構
- go-pfcp 版本一致（見第 0 節）。
- `go.mod` 完全移除 `free5gc/pfcp`：內部版確認無殘留。
- Server/transaction/builder/handler 檔案位置：見第 0 節第 3 點與第 1 節表格，
  **不是**文件第 3 節猜測的四個子目錄，是 `internal/pfcp`（被動+transport）
  + `internal/context`（主動 build/send/handler + domain model）兩塊。
- 未參考 go-upf 實作：repo 內搜尋不到任何 `go-upf` 字樣（除了這份遷移文件本身），
  純內部重新設計。外部版若時間有限，用 go-upf 的設計起步、最後仍以本文件的
  內部版對照結果收斂即可，符合文件 11 節的建議順序。
- 沒有發現需要一併帶出的未公開 shared package；`pfcpType` 是 in-repo package，
  隨 `internal/pfcp` 一起搬就好。

### 6.2 UDP server
- Listen address/port：`net.ListenUDP` 綁 `PFCP_PORT = 8805`（`server.go:28-125`），
  address 可設定（空字串時退回 `0.0.0.0`）。
- Receive loop：**單一** event loop 讀 UDP（`receiver()`），parse 完丟進
  bounded `rcvCh`（8192 容量，滿了就 drop 並記 log，不 block）；真正處理
  request 是 `dispatcher()` 讀 `dispCh` 後**每個 request 開一個 goroutine**
  執行 `s.Dispatch(msg, addr)`（`server.go:318-348`），理由是避免單一 handler
  卡 SMContext lock 拖慢整個 receive loop。
- Max PFCP packet size：`MAX_PFCP_MSG_LEN = 65536`（`server.go:30`）。
- Buffer 複製：`receiver()` 內 `copy(msgBuf, buf)` 後才丟進 channel
  （`server.go:281-282`），避免下一次 `ReadFromUDP` 覆寫尚未處理的資料。
- Shutdown 通知：`Stop()` 關閉 `conn`，`ReadFromUDP` 出錯後 `receiver()` 自行
  `close(rcvCh)` + `dispCh.Close()` 並跳出迴圈（`server.go:272-280`）；
  pending transaction 靠 `stopTrTimers()`（見第 1 節表格「Shutdown」列）處理。
- UDP 錯誤處理：目前 `receiver()` 對**任何** `ReadFromUDP` 錯誤都視為致命、
  直接跳出迴圈結束整個 receive loop（沒有區分 temporary error），這是可以
  接受但值得留意的簡化——外部版若要更保守，可以評估是否要區分
  `net.Error.Temporary()`。

### 6.3 Transaction
- Tx/Rx transaction struct 定義：見第 1 節表格。
- Transaction key：`fmt.Sprintf("%v-%#x", raddr.IP, seq)`（`transaction.go:48-50`）
  ——**只用 IP，不含 port**。外部版要注意：如果 UPF 是用固定 well-known port
  8805 送 request/response（PFCP 規範預期行為），這樣沒問題；但如果外部版
  環境會用 ephemeral port 回應，這個 key 設計可能造成衝突，需要評估後決定是否
  要把 port 也納入 key。
- Transaction map：`sync.Map`，多個 goroutine（main loop 存取 + per-request
  goroutine 存取）並發存取，不需要額外的單一 event loop 限制。
- Retransmission timeout/max retransmission 來源：`s.Config().GetPfcpRetransTimer()`
  （config 檔），外部版需自行決定要不要做成可設定項。
- Response 到達喚醒 sender：靠 `chan smf_context.RcvPfcpMsg`，`recv()` 拿到
  response 後把值寫進 channel 並 `close()`；呼叫端用 `<-ch` 同步等待
  （每個 `SendPfcpXxxRequest` 都是這個 pattern）。
- Timeout 回傳給上層：達到 max retrans 時往同一個 channel 送
  `RcvPfcpMsg{Msg: nil}` 再 `close()`，上層看到 `Msg == nil` 就知道 timeout。
- Duplicate request 重送 cached response：見第 1 節表格「Rx transaction」列。
- Timer stop/reset/cleanup：`mu sync.Mutex` 保護 `timer` 欄位本身，
  在 `recv()`、`handleTimeout()`、`stopTrTimers()` 三處都會 `Stop()` +
  設 `nil`，避免重複觸發或 leak。

### 6.4 Sequence 與 header
- Sequence 限制 24-bit：`idgenerator.NewGenerator(0x0, 0xFFFFFF)`。
- Wrap-around：由 `idgenerator` 這個 lib 處理 allocate/free，內部版沒有自己
  重寫 wrap-around 邏輯，是借用內部共用 util。外部版沒有這個 lib 的話，
  需要自己寫一個「配發後可回收、24-bit 上限、避免與仍存在的 transaction
  衝突」的 allocator，或找等價的開源 lib。
- Node-level / Session-level message 的 S flag、SEID：組 message 時直接用
  `message.NewXxxRequest(...)` 的參數位（例如 `message.NewSessionEstablishmentRequest(0,0,0,0,0, ...)`
  前幾個參數是 mp/fo/seid/seq/pri），go-pfcp 的 constructor 會處理 header
  encoding，SMF 端不用自己組 bit。
- Response 沿用 request sequence：被動方向由 go-pfcp 的
  `message.NewXxxResponse(req.Sequence(), ...)` 直接帶入；主動方向送出時
  `req.SetSequenceNumber(tx.seq)`（`transaction.go:87`）由 transaction 層統一設定，
  builder 端建 message 時序號可以先填 0，送出前才被覆寫。

### 6.5 Message builder
- 各 procedure 的 IE 清單：完整對照見第 1 節表格「PDR/FAR/QER/URR model」與
  「Message builders」兩列，細節函式清單見第 4 節（modification ideas 裡的
  Phase 對照）。
- Grouped IE 是集中 builder：**是**，全部集中在 `internal/context/pfcp_build.go`
  一個檔案，不是分散在各 procedure 呼叫點各自組。外部版建議照抄這個集中化
  策略，方便日後維護 flag/bit mapping。

### 6.6 Context model
已在第 1 節表格「NodeID model」「PDR/FAR/QER/URR model」兩列完整回答，摘要：
- NodeID：`pfcpType.NodeID`（domain struct），不是 `*ie.IE`。
- PDR/PDI/FAR/QER/URR/BAR：都是 SMF domain struct（定義在
  `internal/context/pfcp_rules.go`），不保存 codec-specific type；`ReportingTrigger`
  是唯一一個「domain type 自己有 `.IE()` method」的例外（`pfcp_rules.go:139-147`）。
- FTEID：domain struct `pfcpType.FTEID`，透過 `fteidFlag()` 轉 flag byte 再組
  `ie.NewFTEID(...)`；**但目前沒有反向路徑**——收到 UPF 回傳的 F-TEID
  （`CreatedPDR` 裡的）並不會被解析回 `pfcpType.FTEID`，這是內部版目前的
  gap（見第 5 節）。
- FSEID：本地端組 `ie.NewFSEID(...)` inline；遠端 SEID 收到後直接拿
  go-pfcp 的 `*ie.FSEIDFields`，`.SEID` 欄位複製進
  `PFCPSessionContext.RemoteSEID`（純 `uint64`），沒有走 SMF 自己的 `FSEID` struct
  （雖然那個 struct 存在於 `pfcp_session_context.go`，但實際沒被拿來當儲存型別，
  屬於歷史包袱，外部版不需要照抄這個不一致）。
- Flags：見第 1 節表格與第 4 節。

### 6.7 Error handling 與安全性
- Missing mandatory IE → 正確 Cause：一致模式（例如 Association Setup 收到
  `req.NodeID == nil` 就回 `ie.CauseMandatoryIEIncorrect`/`CauseMandatoryIEMissing`，
  見 `association.go:59-67`）。
- IE getter error 處理：兩層 handler 都一致做「先 nil 檢查、再檢查 getter
  回傳的 error」，唯一例外是 `HandleUSARs` 對單筆 usage report 的 getter 失敗
  採用 `log.Warnf` + `continue` 略過（非致命，因為是 batch 處理），這個設計
  合理，外部版可以照抄。
- Malformed packet 是否可能 panic：`receiver()` 對 `message.Parse()` 失敗會
  `log.Warnf` + `continue`，不會 panic；但 `pfcp_send.go` 裡有多處
  **未檢查 ok 的單值型別斷言**，例如
  `return rcvPkt.Msg.(*message.AssociationSetupResponse), nil`，
  這些只有在「呼叫前已經檢查過 `MessageType()`」的前提下才安全——
  外部版要嚴格維持「先查 MessageType 再斷言」這個不變量，否則有 panic 風險
  （見第 5 節，這是內部版本身留下的技術債，值得外部版重寫時直接改成
  `.(type)` + `ok` 判斷更安全）。
- Remote address / sequence spoofing：內部版**沒有**額外做來源驗證（例如
  沒檢查回應是不是真的來自已知的 UPF NodeID 對應的 IP）——這個風險文件
  6.7 節有列出但內部版本身也沒特別處理，外部版若要加強可以視為額外硬化項目，
  不是「內部版已驗證過的行為」，需要自行評估要不要做。
- Unexpected response type/SEID/NodeID：見第 1 節表格「Sending / response
  matching」，MessageType 與（session 訊息時的）SEID 都有比對，NodeID 沒有
  額外比對（信任 transaction key 已經用 remote addr 做了粗略配對）。
- Transaction channel block/重複 close 風險：`rspCh` 的 `close()` 只會在
  `recv()` 或 timeout 兩條路徑之一發生（互斥，因為 transaction 一旦被
  處理就會從 `sync.Map` 刪除），設計上不會重複 close；`stopTrTimers()`
  對已經是 `nil` 的 timer 會 `continue` 跳過，避免對已清空的 transaction
  重複操作。

### 6.8 Tests
內部版涵蓋範圍見第 1 節表格「Tests」列；逐項對照文件要求：
- UDP request/response、timeout/retransmission、duplicate request：
  `server_test.go`/`server_unbounded_test.go` 有涵蓋核心邏輯（用 mock 而非
  真實 UDP socket），`reliable_pfcp_request_test.go` **這個檔名雖然存在，
  但內容整份被註解掉、是空的**——這不是內部版「已驗證完成」的測試，是
  兩邊都留下的歷史殘骸，外部版若要做端對端 UDP retry/dup 測試需要自己重寫，
  不能參考這個檔案的內容。
- marshal/parse round trip：`internal/context/pfcp_build_test.go` 有針對
  builder 輸出做驗證。
- race test：`TestTxTransactionRaceOnServerClose`（`server_test.go:27`）
  專門測 server close 時的 race，執行時要用 `-race`。
- malformed/missing IE test：`pfcp_handler_test.go` 有涵蓋。
- Association/Session E2E test：內部版測試以 unit/mock 為主，沒有看到與
  真實 UPF（或 go-upf）跑的 E2E test 檔案在這個 repo 內——這類測試如果
  存在應該是在 CI/整合測試環境，不在程式碼庫的 unit test 範圍，外部版
  仍需依文件 9.2 節自行安排整合測試。

---

## 4. 外部版修改想法（對照文件第 8 節 Phase 規劃調整）

以下是根據上面核對結果，對文件原本 Phase 0-7 規劃的**具體調整建議**：

1. **Phase 1 起手前，先決定目錄切法**，不要照文件第 3 節的四個子目錄硬套。
   建議：
   - `internal/pfcp/`（或維持外部版現有的這個 package）：UDP server 生命週期
     + tx/rx transaction + sequence allocator + 被動方向 request handler
     （Heartbeat/Association/SessionReport 的 Request→Response）。
   - `internal/context/`：主動方向 Session Establishment/Modification/Deletion/
     Association 的 builder + sender + response handler + PDR/FAR/QER/URR
     domain model。
   - 外部版目前已經有 `internal/pfcp/message/`、`internal/pfcp/handler/`、
     `internal/pfcp/udp/` 這三個子目錄（見 `smf-opensource/internal/pfcp/`），
     如果想保留現有目錄骨架、不要大搬家，也可以維持這個切法，把內部版對應的
     邏輯「灌進」這三個子目錄即可，不強制要求跟內部版檔名一致——**這是合理
     的替代方案**，重點是保留內部版驗證過的邏輯與不變量（buffer 複製、
     transaction key、timeout→nil 約定、集中化 builder），不是檔案路徑本身。

2. **Phase 1（server + transaction）直接照抄內部版的正確性細節**，這些是
   最容易出錯、內部版已經踩過坑並修好的地方：
   - `receiver()` 裡一定要 `copy` buffer 再 parse。
   - Transaction key 目前設計只用 IP 不含 port，外部版要先確認目標 UPF
     （或測試用的 go-upf）送 response 的來源 port 行為，再決定要不要沿用。
   - timeout 用「送 `nil` Msg 進 channel 再 close」表示，所有呼叫端要遵守
     同一約定，不要有的地方用 error、有的地方用 nil channel、造成不一致。
   - Server close 時要遍歷所有 pending transaction 停 timer、喚醒等待者
     （對應 `stopTrTimers()`），這個測試 `TestTxTransactionRaceOnServerClose`
     值得外部版直接搬過去當 race test 範本。

3. **Phase 2-4（Heartbeat/Association/Session）builder 邏輯建議整併**，
   不要學內部版「有的 procedure 有獨立 Build 函式、有的 inline 在 send 函式裡」
   這個不一致——外部版重新設計時全部拆成獨立 Build 函式即可，可讀性更好，
   也更方便寫 unit test（可以直接測 Build 函式的輸出，不用連 transaction
   一起測）。

4. **Phase 4（Session Establishment）注意 QER/URR 去重**：同一個 QER/URR
   可能被多個 PDR 引用，組 grouped IE 前要先用 map 依 ID 去重，這是協定正確性
   要求（避免送出重複的 Create QER/URR IE 給 UPF），不是可選的最佳化，
   內部版 `pfcp_build.go:577-595` 的做法要照搬。

5. **Phase 4 額外補強項**：內部版目前**沒有實作 CreatedPDR / 收到的 F-TEID
   讀回**（見第 5 節）。如果外部版的既有 free5gc/pfcp 實作有處理這塊
   （UPF 選擇的 F-TEID 讀回並存進 context），遷移到 go-pfcp 時**這段邏輯要
   自己補**，不能參考內部版，因為內部版本身就沒做。

6. **Phase 6（清 context 舊型別）順便做 domain type 命名決策**：是否要把
   domain type package 取名 `pfcpType`（貼合內部版方便對照）由團隊決定，
   但不管取什麼名字，都要維持「這是 SMF 自己的型別，不 import 任何舊
   `free5gc/pfcp` 相關 path」這個底線，並且要讓 `Cause` 這類型別的常數
   全部改用 `ie.CauseXxx`（go-pfcp 提供的），不要自己重複定義一套容易對不齊
   的常數（內部版 `pfcpType.Cause` 常數其實很少被用到，多數地方直接用
   `ie.CauseXxx`，這點值得外部版一開始就統一，不要留下內部版這種「兩套
   Cause 常數並存」的歷史包袱）。

7. **不要照抄的內部版特有機制**（Saviah-internal only，外部開源版不需要，
   搬過去反而增加不必要的相依）：
   - OpenTelemetry vendor-specific IE tracing（`smfotel.NewSpanMarshalToVendorSpecificIE`/
     `GetVendorSpecificPayload`，`SAVIAH_SPECIFIC_ID`）——這是把 trace context
     塞進 PFCP 訊息的 vendor-specific IE 裡跨進程傳遞，屬於 Saviah 內部
     可觀測性基礎設施，外部開源版沒有對應的 tracer 就不需要這段。
   - `rcvCh`/`txCh`/`dispCh` 三層 channel + unbounded channel + overload drop
     的細節設計是回應 Saviah 內部一次真實事故（程式碼註解提到 SVC-4946）調校出來的
     容量參數，外部版**可以參考「overload 時要 drop 而不是無限堆積」這個
     設計精神**，但不需要照搬 `PFCP_RCV_CH_LEN = 8192` 這種依 Saviah 流量特性
     調出來的數字，也不需要一定要用 `uchan.UnboundedChan` 這個內部 lib——
     用標準的 buffered channel 或其他佇列實作都可以，只要保留「滿了就丟棄
     並記錄，不要 block receive loop」的原則。
   - `idgenerator`、`dnscache.LookupIpWithRetry`、`unbounded_channel` 這幾個
     都是 `bitbucket.org/free5GC/util` 內部套件，外部版沒有這些 lib，要嘛找
     開源替代、要嘛自己寫等價的最小實作（sequence allocator 大概 30-50 行
     就能寫完，不需要整個 lib）。
   - `assocEpoch` 這個防止「UPF 剛重新關聯後、舊的 heartbeat 失敗回報還沒
     處理完就把新關聯狀態覆蓋掉」的機制（`upf.go:66-69`, `context.go:489-524`）
     是修一個真實 race condition（票號 SVC-4745）留下的設計，**邏輯本身是
     通用的、值得外部版參考**，但屬於「UPF association 狀態機的強化」，
     跟 go-pfcp 遷移本身無直接關係，可以晚一點再做，不需要卡在遷移
     的必要路徑上。
   - import path 是 `bitbucket.org/free5GC/smf/...`，外部版 module 是
     `github.com/free5gc/smf`——這只是提醒，直接複製貼上程式碼時記得改
     import path，不是遷移邏輯的一部分。

---

## 5. 內部版本身尚未完成/有風險的地方（引用內部版當參考前要知道的限制）

在把內部版當「金標準」照抄之前，以下幾點是內部版自己也還沒做好、或是有
已知風險的地方，外部版不應該照單全收：

1. **`CreatedPDR` / 收到的 F-TEID 沒有被解析**：
   `HandlePfcpSessionEstablishmentResponse`（`pfcp_handler.go:138-199`）只讀了
   `NodeID`、`Cause`、`UPFSEID` 三個 IE，完全沒有處理 `rsp.CreatedPDR`。
   `pfcpType.FTEID` 裡的 `Ch`/`Chid` flag 在 build 端有支援組
   「請 UPF 自己選 F-TEID」的 request，但收到 UPF 選的結果後沒有任何程式碼
   把它讀回存進 context。如果外部版需要「UPF-chosen F-TEID」這個功能，
   這段要自己新寫，內部版沒有可抄的實作。

2. **`BAR` 的 `SuggestedBufferingPacketsCount` 沒接上**：
   `toBAR()`（`pfcp_build.go:456-470`）目前固定用
   `ie.NewDownlinkDataNotificationDelay(0)`，domain struct `BAR` 裡雖然定義了
   `SuggestedBufferingPacketsCount` 欄位（`pfcp_rules.go:87` 附近），但沒有
   被組進 IE。

3. **單值型別斷言的 panic 風險**：`pfcp_send.go` 多處在檢查完
   `MessageType()` 後直接用 `x.(*message.XxxResponse)` 單值斷言（沒有
   `, ok`），只要「先查型別再斷言」這個不變量被未來的修改破壞，就會 panic。
   外部版重寫時建議直接用 `switch x := msg.(type)` 或雙值斷言，寫法更安全，
   不需要保留內部版這個歷史寫法。

4. **靜默吞掉 marshal error**：`NewSDFFilterAlwaysWithBIDFlag` 遇到
   `Marshal()` 失敗會直接回傳 `nil`（吞掉 error，`pfcp_build.go:172-177`
   附近），呼叫端拿到 `nil` 只會少了這個 SDF filter，不會有任何錯誤訊息。
   外部版如果重寫這塊，建議把 error 往上傳，方便除錯。

5. **`UPFSelectionParams` 是內部版自己標記的 deprecated legacy path**
   （`upf.go:79-98` 附近註解 `// Deprecated: prefer RouteParams with
   RouteGenerator`）——跟 go-pfcp 遷移無關，但如果外部版程式碼裡也有類似
   命名的東西、又剛好在遷移範圍附近，不要誤以為這是遷移的目標寫法去抄，
   這是內部版自己都在淘汰的舊路徑。

6. **`reliable_pfcp_request_test.go` 是空測試**：兩邊 repo 都有這個檔名，
   內容整份被註解掉（見第 3 節「6.8 Tests」）。如果外部版的目標之一是
   補齊文件 9.1 節要求的 timeout/retry/duplicate request 測試，這個檔案
   目前完全不能參考，需要從零寫。

---

## 6. 小結：外部版最推薦的執行順序

1. 先照第 4 節第 1 點決定目錄切法（可維持外部版現有的 `message/`/`handler/`/
   `udp/` 子目錄骨架，不必大搬家）。
2. 依文件原本 Phase 1-5 順序遷移，但每個 Phase 都對照本文件第 1 節表格
   與第 3 節逐項回答，把內部版「已驗證正確」的細節（buffer 複製、
   transaction key、timeout 約定、QER/URR 去重、Cause 常數統一用
   `ie.CauseXxx`）直接搬過去，**跳過**第 4 節第 7 點列的 Saviah-only 機制。
3. 到 Phase 4/5 時，額外規劃「CreatedPDR/F-TEID 讀回」這個內部版沒有的功能
   （如果外部版原本的 free5gc/pfcp 實作有做這件事，遷移時不能漏掉，要
   自己重新設計實作，不是照抄內部版）。
4. Phase 7 完成後，除了文件 9-10 節的驗證清單，另外加測
   「單值型別斷言前是否都先驗證過 MessageType」這條，避免重蹈內部版的
   技術債。
