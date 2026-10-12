# 协议规划

全部协议规划集中在本文：先看消息、字段、路由和适用状态，具体 .proto 在对应业务开始实现时再按需要拆分，不预建 auth/session/mole 等空目录。拆分后仍由本文提供完整索引。当前登录/位置/打地鼠协议集中实现于 [demo.proto](../common/proto/demo/business/v1/demo.proto)，Probe 单独保留；斗地主和持久化积分仍为规划。

## 1. 总览

客户端始终只连接 access。请求表达业务意图，access 从已确认会话填入身份、选择内部目标；客户端不能指定 Node/ServiceName，也不能用 UserID/LoginID 充当凭据。

| 消息组 | 内容 | access 内部调用 | 进度 |
| --- | --- | --- | --- |
| Probe | 诊断请求、四节点响应、mole 主动 Push | login / online / mole / settlement | 已实现 |
| 登录 | 密码认证并确认当前会话；第三方凭据随后加入 | Login → Online → 原 Room（重连时） | 已实现；本地测试账号 |
| 会话与位置 | 查询当前位置/待确认操作、进房、离开、位置更新 | Online → RoomDirectory / Room | 已实现 |
| 打地鼠 | 共享快照、命中、计分和局末结果 | 当前 MoleRoom | 已实现到局末冻结 |
| 三人斗地主 | 准备、本人可见快照、出牌、不出 | 当前 LandlordRoom | 规划 |
| 积分与结算 | 本人余额、本人到账查询、到账通知 | Settlement；内部由 Recovery 提交全组结果 | 规划 |

## 2. 消息外层与编号

复用 GSR Envelope 的 msg_id、request_id、payload。Request 的 request_id 非零，Response 回同一编号，Push 为零；request_id 只关联一次响应，不作为跨重连业务幂等键。复杂内部命令及返回值统一编码为 Protobuf bytes，不直接跨节点传业务 Go struct。

当前固定编号如下，斜线分别是 Request/Response；Push 单独列出。9001 是框架 ErrorResponse，业务不得占用，后续编号在实际实现时分配。

| 消息 | 编号 |
| --- | --- |
| Probe / ProbePush | 1001/1002，1003 |
| PasswordLogin | 1101/1102 |
| QuerySession | 1111/1112 |
| QueryOperation | 1121/1122 |
| SessionReplaced | 1191 |
| EnterMole | 1201/1202 |
| LeaveRoom | 1211/1212 |
| MoleSnapshot | 1301/1302 |
| MoleHit | 1311/1312 |
| MoleState | 1391 |

客户端 msg_id 全目录唯一；内部 CommandID 在 common/command/business.go 独立定义，不能用作客户端编号。

Registry 定义类型、方向和 Request/Response/Push，状态策略定义当前连接是否准入。Dispatcher 接线固定，不随状态切换重建。状态拒绝写对应业务 Response，明确返回 not_authenticated、state_mismatch、operation_pending、binding_replaced 等结果；格式/方向/未实现消息由框架错误协议处理。

## 3. 共用数据与状态准入

下列统一说明数据用途，已实现字段和编号以 demo.proto 为准；斗地主/结算字段用到时补入。

| 数据 | 内容与用途 |
| --- | --- |
| SessionView | UserID、用户位置、LocationVersion、可选房间信息（GameKind、RoomID、RoundID、RoomPhase、RoomStateVersion）、待确认 OperationID、allowed_msg_ids；不暴露内部路由或 BindingToken |
| PendingOperation | 内部保存 OperationID、操作内容、目标位置/版本和比较绑定信息；客户端只能查询本连接的操作，OperationID 本身不授权 |
| RoomKey | 客户端可带 RoomID/RoundID 作为预期位置，服务端只做匹配检查；路由始终取 Agent 的已确认位置 |
| MemberView | 成员 UserID、座位、连接标记；分数/准备/手牌数量按游戏加入，私人手牌不放公共成员数据 |
| ResultView | SettlementID、RoundID、冻结积分结果和由保存确认/回执推导的进度；本人结算查询只返回本人到账信息 |
| 内部绑定信息 | UserID、LoginID、AccessNode、AgentName、RoomID、BindingToken；由服务端填写并条件校验 |

用户仅 guest（未登录）、lobby（大厅）、room（房间）；房间仅 filling（等待）、running（进行）、ended（结束）。位置由已确认会话/路由推导，Room 推进房间阶段。认证、RPC 等待、保存和结算步骤不增加状态。

| 当前位置/阶段 | 允许的客户端请求 |
| --- | --- |
| guest | PasswordLogin；第三方登录在对应切片加入 |
| lobby | QuerySession/QueryOperation、EnterMole、本人 QueryBalance/QuerySettlement；EnterLandlord 随斗地主加入 |
| room / filling | QuerySession/QueryOperation、对应游戏 Snapshot、LeaveRoom、本人结算查询；斗地主另有 Ready |
| room / running | QuerySession/QueryOperation、对应游戏 Snapshot、本人结算查询；按 GameKind 开放 MoleHit 或 LandlordPlay/Pass |
| room / ended | QuerySession/QueryOperation、对应游戏 Snapshot、本人结算查询、LeaveRoom（Room 检查到账回执） |

有 PendingOperation 时只开放 QuerySession/QueryOperation，操作查询限定本连接；没有房间阶段快照时只开放同步和本人查询。连接级探活不增加业务权限。停止中的 Agent 不处理业务。准入默认拒绝，access/Online 共用策略，Room 再验证绑定、阶段和具体操作条件。allowed_msg_ids 只是交互提示。

## 4. 客户端消息清单

下表登录、查询、进/离房和打地鼠消息已实现；EnterLandlord/Landlord*/QueryBalance/QuerySettlement 尚未实现，每个 Request 配对应 Response。写操作带 operation_id，响应返回同一 ID；位置操作和命中同 ID 改内容返回冲突；登录仅在 guest 准入，已登录连接不再次认证。位置未确认时通过 QuerySession/QueryOperation 核对，已确认房间的快照滞后时通过对应 Snapshot 恢复。

| Request → Response | 请求主要字段 | 成功返回与边界 |
| --- | --- | --- |
| PasswordLogin → PasswordLoginResponse | operation_id、username、password | 已确认 SessionView；认证成功但绑定未知返回待确认，不能假报已登录 |
| QuerySession → QuerySessionResponse | 无身份/路由参数 | 本连接的位置、阶段、待确认操作和准入协议；refreshed 标明是否已刷新；unavailable 时返回本连接缓存 |
| QueryOperation → QueryOperationResponse | operation_id | 本连接操作的确定结果或待确认；历史结果不能再次切换路由 |
| EnterMole → EnterMoleResponse | operation_id | RoomDirectory 分组确认后的 SessionView 与 MoleSnapshot，客户端不指定组或座位 |
| EnterLandlord → EnterLandlordResponse | operation_id | 三人房间确认后的 SessionView 与本人可见 LandlordSnapshot |
| LeaveRoom → LeaveRoomResponse | operation_id、预期 room_id | 移除确认后的大厅 SessionView；running 拒绝，ended 未到账则返回待结算且不移除 |
| MoleSnapshot → MoleSnapshotResponse | 无，使用当前路由 | 房间/局/版本、成员座位与分数、共享地鼠实例/洞位/窗口、局期限、可选 ResultView |
| MoleHit → MoleHitResponse | operation_id、预期 room_id、round_id、mole_instance_id | 命中结果与最新可见状态；服务端判断时间、先后和奖励，客户端不传分数 |
| LandlordSnapshot → LandlordSnapshotResponse | 无，使用当前路由 | 房间/局/版本、座位/准备、公共牌局、当前回合/期限、本人手牌、可选 ResultView |
| LandlordReady → LandlordReadyResponse | operation_id、预期 room_id、准备版本 | 本人准备确认；不替其他玩家准备 |
| LandlordPlay → LandlordPlayResponse | operation_id、预期 room_id、round_id、turn_id、cards | 合法出牌后的本人可见状态；实际持牌、牌型、压制和回合均由 Room 判断 |
| LandlordPass → LandlordPassResponse | operation_id、预期 room_id、round_id、turn_id | 不出后的状态；新领出时不能不出 |
| QueryBalance → QueryBalanceResponse | 无 UserID 参数 | 已认证用户本人的整数积分余额 |
| QuerySettlement → QuerySettlementResponse | settlement_id | 核验该用户是参与者后返回本人增量/到账结果；未知或待确认不代表到账 |

主动推送优先复用完整可见状态，减少细碎事件协议：

| Push | 字段与用途 |
| --- | --- |
| SessionChanged | SessionView；仅由已确认位置结果产生，游戏推送不能切换路由 |
| MoleState | 与 MoleSnapshot 相同的完整可见状态，覆盖开局、目标变化、命中和结束 |
| LandlordState | 与 LandlordSnapshot 相同的本人可见状态，公共状态/本人手牌分别生成 |
| SettlementCompleted | SettlementID、本人到账回执；丢失时可通过 QuerySettlement 核对 |
| SessionReplaced | 当前连接被新的绑定替换的通知；正确性仍依赖 Room 拒绝旧 LoginID |

## 5. 内部命令清单

下列统一列出内部载荷。Authenticate→login/auth、Position（bind/enter/leave/query/operation）→online/sessions、AllocateRoom→mole/rooms、ReserveSeat/BindRoom/RemoveMember/Snapshot/Hit→mole/<RoomID> 已接线；其余随结算和斗地主加入。断线、RoomState/RoomPhase/SessionReplaced 都使用条件 Send。

| 命令/通知 | 主要请求字段 | 结果或接收规则 |
| --- | --- | --- |
| AuthVerify（access → Login） | 用户名密码或第三方提供商凭据（由 Login 验证） | UserID 或认证拒绝；不返回密码哈希，不决定位置 |
| Bind / Enter / Leave / QueryOperation（access → Online） | OperationID、服务端身份/接入地址、预期位置版本、游戏/房间操作参数 | 条件协调后的完整位置结果；同用户仅一条待确认操作 |
| AllocateRoom（Online → RoomDirectory） | OperationID、GameKind、服务端玩家绑定信息 | 可加入房间/座位及首次比较绑定所需信息；满组分配新组 |
| CompareAndBind（Online → Room） | RoomID、UserID、OperationID、ExpectedBindingToken、目标 LoginID/Agent 地址 | 新绑定令牌与阶段快照；旧令牌不能覆盖新连接 |
| Remove / QueryBinding（Online → Room） | RoomID、UserID、LoginID、绑定令牌、OperationID | 条件移除或当前绑定/操作结果；移除仍保留防迟到的代次/墓碑 |
| Snapshot / Hit / Ready / Play / Pass（Agent → Room） | 服务端绑定信息加客户端操作参数 | 校验当前绑定、局/回合、业务条件和幂等键后的编码结果 |
| RoomState（Room → Agent，Send） | UserID、LoginID、RoomID、BindingToken、RoomStateVersion、本人可见快照 | 先匹配已确认绑定，再比较版本，向客户端 Push |
| RoomPhase（Room → Online，Send） | 当前玩家绑定、RoomID、RoundID、阶段、RoomStateVersion | 仅更新当前房间的低频阶段投影，不同步每次命中/出牌 |
| SubmitSettlement（Recovery → Settlement） | SettlementID、协议/规则版本、GameKind、RoomID/RoundID、按 UserID 排序的全组积分增量及必要结果 | 全组同事务回执；同 ID 相同内容返回原结果，改内容/同局换 ID 冲突 |
| QuerySettlement（Recovery → Settlement） | SettlementID | 已提交的全组回执或尚未确认；只能按原 ID 条件重试 |
| SettlementReceipt（Recovery → Room） | SettlementID、匹配冻结结果的回执 | ended 内记录到账确认；通知丢失可查本节点持久化完成确认 |

内部 Cluster 只用于受信任网络，Source 不能证明远端用户身份。Room 不 Call Online/Agent，Settlement 不 Call Room；热点操作从 Agent 直达已确认 Room，不经 Online 逐条转发。

## 6. 关联、版本与重复操作

- LoginID 是服务端生成且不复用的登录关联标识，BindingToken 是房间比较绑定凭据；客户端不能自行选择。待确认 RoomRequest 的载荷在首次发送前冻结，断线不改变同 ID 重试内容，而是对确认后的绑定条件清理。旧连接通知、操作及离开均按条件检查。
- LocationVersion 由 Online 为每用户单调分配，确认身份/路由变化才安装新版本。目标版本拒绝后也不复用；Connected 和命中不推进位置版本。同位置版本的身份/路由必须一致。
- RoomStateVersion 在同 RoomID 内单调增加，不随新局重置；RoundID 单独识别局。状态消息先匹配当前绑定，再比较房间版本；新快照后忽略旧版本，缺口可查询快照。
- 位置操作用 OperationID/内容/比较令牌；命中用 UserID/RoomID/RoundID/实例/OperationID；出牌与准备还检查回合或准备版本。缓存有界，但淘汰不能让旧局/旧回合重新执行。
- Call 超时只表示结果未确认。位置操作保留原记录；游戏操作以原 ID 查询/重试，不能改成新局/新回合。StartRound 由满员服务端触发，客户端不重新触发一局。
- 响应和 Push 的到达顺序不承担正确性。先于进房确认的目标消息不激活路由，确认快照和后续同步修复遗漏；重连清空旧客户端待处理操作，以新快照恢复。

## 7. 已实现 Probe 与生成方式

当前 schema 为 [diagnostic.proto](../common/proto/demo/diagnostic/v1/diagnostic.proto)。ProbeRequest 为 operation_id/text；ProbeResponse 为 operation_id/text/code/failed_node/nodes；ProbePush 为 operation_id/text/node。响应 code 为 ok/invalid/unavailable/disabled，ok 只表示四个 Handler 返回，CLI 另等实际 Push。

内部 NodeProbeRequest 为 probe/access_node/agent_name/agent_token，NodeProbeResponse 为 operation_id/text/node，AgentProbePush 为 push/agent_token。探测令牌由 access 生成，仅隔离当前连接，不充当 LoginID。Probe 是开发诊断接口，access 的 enable_probe 默认 false，业务准入目录不包含它。

从根目录运行 scripts/generate.sh（或 npm --prefix typeclient run generate）。源 schema 在 common/proto，Go 与 registry 输出 common/gen，TS 输出 typeclient/src/gen；所有结果和工具输出只在 gsrdemo。Go 插件选中整个 demo/ 目录，新增实际业务消息同时生成 Go/registry；TS 另包含只读 GSR 的 Envelope/选项。

工具版本：protoc-gen-go 1.36.1、Buf 1.73.0、protoc-gen-es 2.15.0。GSR Envelope/ErrorResponse/目录选项来自只读本地基线；registry 工具从该依赖构建，npm 工具由 package-lock.json 固定。生成文件及锁文件纳入 Git，生成结果不手改。

当前 PositionResult 统一携带 code/operation_id/session/snapshot/refreshed，各客户端 Response 用独立类型包装。RoundResult.progress 当前只有 unpersisted；结算未接入，ended 的 LeaveRoom 固定返回 settlement_pending 并保留位置和成员。SessionChanged、LandlordState、SettlementCompleted 尚未实现；位置确认目前通过对应 Response/查询恢复，房间 Push 只更新快照，不安装路由。
