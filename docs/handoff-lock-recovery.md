# 短時間往復と強制復帰の所有権回復

## 原因

出発元の `complete` は、その端末のDBに移動先をholderとする5分leaseを残す。
この行はSlack(現在は`slack`拡張パッケージ)の添付の中継認可にも使うため、出発直後に失効させてはいけない。
一方、5分以内に同じ端末へ戻ると、通常の `AcquireAgentLock` ではこの行を取得できず、finalizeが失敗していた。
後からGuardだけが取得するとproxyがselfになり、未完了のfinalizeを経由せずにSlack代理認可が壊れた。

別の不具合として、強制復帰でDBをlocalへ戻しても、Slackルーターのメモリhintが古い移動先を指し続けた。
403は再送してよい証拠ではないため、全403を再送する修正は採らない。

## 受信側の永続receipt

`incoming_handoffs` は端末ローカルの移動受入記録で、peer間では同期しない。
同じsnapshot transactionで、agent ID、op ID、認証source、target、受信側の既存holderと永続fencing counterを束縛する。
別端末のcounter同士は比較しない。

```text
phase-1 snapshot → prepared
source complete/drain → finalize → accepted → activated → done
                                holder/proxy/tokenを原子的に確定
prepared → drop → aborted
```

- `prepared`：通常のGuard取得とターン受付を禁止する。
- `accepted`：holderとHub proxyは確定したが、token採用やruntime登録は未完了。起動時の自動復帰とターン受付を禁止する。
- `activated`：runtime hook完了。到着ターンを受付可能にして、既存の到着配信プロトコルへ進む。
- `done`：到着処理まで完了。同じopを再度activateしない。
- `aborted`：同世代のsnapshotを自動起動しない。後続の正当な受入または明示的な強制復帰による世代更新で、この起動制限は効かなくなる。

finalize再試行はreceiptのsourceと世代を検証してからruntimeを読み直す。
強制復帰や別の所有権取得でcounterが進んだら、古いfinalizeは拒否する。
正常な停止でlock行だけ消えた場合は、counterが変わっていないことを確認して再取得する。

`drop` はpending credentialsのKV保存に依存しない。
phase-1より先に届いたキャンセルも、agent行を必要としないsource付きtombstoneとして保存する。
未知のop同士には順序を付けられないため、同世代の未完了opを新しいsnapshotで自動上書きしない。
キャンセル応答が失われても同じdropを再試行できるが、drop自体が届かなければ次の移動前に明示的なキャンセルが必要になる。

## 複数端末を経由する帰還

A→B→C→Aでは、Aに残ったholderがBのままでも正常である。
AはBのDBに基づくreadiness routeをデバイス認証付き通信で照会し、Cへの委譲を確認する。
最大8 hopまで追跡し、循環、照会失敗、sourceとの不一致は拒否する。
確認結果はAの既存世代に束縛し、snapshot transactionとfinalizeで再検証する。
state probeはlockやruntimeをpurgeせず、全量snapshotを要求する。
A自身がholderなら、古いsourceの照会でその所有権を消すことはない。

## 遅延処理の制限

Slack hintには、その端末のholderと永続counterを付ける。
探索やPOSTより前に取得した世代だけを応答処理に使い、古い応答が新世代の経路を学習させないようにする。
世代不一致のhintは次のroutingで使わない。
すでに送信したPOSTは、到達した可能性があるため別holderへ無条件に再送しない。

出発元の遅延release、abort、未送信のfinalizeにも、取得時またはcomplete時の世代を使う。
GuardのAdd、Remove、refreshもagent単位で直列化し、Remove前のsnapshotによる遅延取得を止める。

## 強制復帰と旧holder

強制復帰（`POST /api/v1/agents/{id}/handoff/force-reclaim`）は、復帰した端末のkojo.dbだけを書き換える。
旧holderには自分がholderだという記録（lock行または`arrived/<id>`）が残り、cronや定期チェックインを持つ2つ目のruntimeが動き続ける。
そのまま旧holderへ再移動すると、state probeが`wrong_holder`（409）で失敗する。
`authorizeIncomingSource`は、sourceからの照会でlocalの所有権を消さない設計だからである。

このため、強制復帰した端末は旧holderへ解放を通知する。

1. 強制復帰は、commitの前に、自分以外の登録peer全員を宛先とする通知を`kv`（namespace `handoff_reclaim_notice`、machine scope）に保存する。commit直後にクラッシュしても通知が残るようにするためで、保存できなければ強制復帰自体を500で中止する。
   commit後にその場で配信し、結果は応答の`reclaimNotice`（`releasedOn` / `notHolderOn` / `pendingPeers` / `supersededOn`）で確認できる。
2. 受信側（`POST /api/v1/peers/agent-sync/reclaimed`）は、自分がまだagentを保持している場合だけ解放する。
   - 署名peerは`reclaimer_device_id`と一致する必要がある。
   - lock行または到着markerが自分を指しているかを確認する。
   - 解放は、switch完了後のsourceと同じ処理で行う。
     - 自分を指すlock行を消し、fencing counterを進める。到着markerだけで保持と判定した場合、他peerを指すlock行（shadow）とcounterには触れない。
     - 未完了の`incoming_handoffs`をabortedにする。
     - 到着markerとpendingを消す。
     - `released/<id>`と`full_sync/<id>`を書き込む。
     - ここまでを1 transactionで行い、その後cron、notify、one-shotを止め、AgentLockGuardから外す。200を返す時点で解放は永続化されているので、再起動してもruntimeは復活しない。
   - 保持していなければ何もせず`released=false`を返す。このため全peerへの一斉送信でも安全で、多段移動の中継peerが記録されていても届く。
3. 解放した端末に付いた`handoff/full_sync/<id>` markerにより、次にこの端末へ移動するときはstate probeが`Known=false`を返し、全量snapshotを要求する。
   probeの後でmarkerが付いた場合も、差分の`agent-sync`は`409 full_sync_required`で拒否する。
   markerは全量syncのcommitと同じtransactionで消える。
4. 届かなかったpeerへの通知はpendingのまま残り、次の契機で再送する。
   - 1分ごとの再送
   - そのpeerがofflineからonlineに変わったとき
   - そのpeerへのswitch-deviceの直前

   再送時に復帰した端末自身がもうholderでなければ、通知は古いものとして捨てる。
   通知はcommit前に書くため、commitされた強制復帰の記録（後述の`handoff_reclaimed_at`）が無ければ捨てる。直近の強制復帰が失敗して古い記録だけが残っている場合は、その記録の時刻で通知する。この記録は再起動を挟んでも有効なので、再起動後も再送は続く。
   相手が`409 concurrent_reclaim`を返した場合は、再送しても結果が変わらないので配信済みとして扱う（`supersededOn`）。
   7日経っても届かない通知も捨てる。

旧holderが長期間offlineのときは、旧holder側でowner権限を使って同じendpointを手動で呼べる（`reclaimer_device_id`には復帰した端末のIDを指定する）。

限界:

- 2台がほぼ同時に強制復帰すると、双方がholderで互いに通知を送る状態になる。両方が解放すると誰も保持しなくなるため、各端末は最後に強制復帰した時刻をagentごとに保存し（`handoff_reclaimed_at`）、通知にも復帰時刻（`reclaimed_at`）を載せる。受信側は`(復帰時刻, device ID)`の順序で自分の方が後なら`409 concurrent_reclaim`で拒否し、先なら解放する。双方が同じ2組を比較するため、通知の到着順や再送に関係なく片方だけが残る。時計のずれは「どちらが残るか」を変えるだけで、両方が解放されることはない。復帰時刻は強制復帰と同じtransactionで新しいfencing tokenと組（`<時刻>:<token>`）にして書き、受信側の順序判定では、自端末が今もそのtokenでlockを保持している間だけ有効とする。agentが端末を離れた後（switchでの送出、通知による解放）、再起動でlockを取り直した後、強制復帰が失敗した場合（transactionごとrollback）は無効になるので、過去の復帰時刻が後の正当な強制復帰を拒否し続けることはない。無効な場合は譲る側に倒す。
- 通知の認可は「署名peer＝`reclaimer_device_id`」と「受信側がまだ保持しているか」だけで、送信元が本当に強制復帰したかは検証できない（端末ローカルfencingのv1では、他端末の所有を証明する手段がない）。登録済みpeerは同じownerの端末として信頼する前提で、他のpeer間endpointと同じ信頼モデルである。誤って解放されても、データは消えない。その端末で強制復帰すれば戻せる。
- この修正より前のkojoを動かすpeerは、新しいendpointを持たない（403または404を返す）。通知は、その端末を更新するか7日のTTLが切れるまでpendingのまま残る。

## 適用条件と限界

- 修正対象のHubと参加peerをすべて更新してから往復を確認する。旧targetの通常取得処理はこの修正では変わらない。
- 旧版で保存された世代情報のないpending finalizeは409で拒否する。既に壊れた移動を無条件再実行せず、正しい所有者を確認して強制復帰し、新しい移動を開始する。
- これはv1の端末ローカルfencingと委譲確認であり、分散コンセンサスではない。すでに遠隔端末へ送信されたPOSTを、別端末の強制復帰が遡って取り消す保証はない。
- 受入と到着配信は別段階である。到着応答が不確定な場合は、既存の重複防止に従い自動再送を抑制する。

## 回帰テスト

`incoming_handoffs_test.go`、`agentlockguard_test.go`、`handoff_recovery_test.go`で次を検証する。

- 2 Store間の5分未満の往復と原子的なproxy設定
- accepted/activatedからの停止後再試行
- 強制復帰後の古いfinalize、release、abort、Slack応答の拒否
- phase-1より先のdrop、credentials保存前のdrop、未知の旧opによる上書き拒否
- 3 peer帰還の委譲確認と、照会中に起きたlocal強制復帰の保護
- owner権限があるデバイスでもsource IDを偽装できないこと
- 強制復帰の通知による旧holderの解放、非holderの無視、署名不一致の拒否（`reclaim_notice_test.go`）
- 解放後のprobeが全量syncを要求することと、差分syncの拒否
- offline peerへの通知をpendingのまま保持し、holderでなくなったときに破棄すること
