# Antigravity CLI lifecycle補完

upstreamの`gemini-agy`を基準に、旧試験版`exp/antigravity-backend`にあった処理だけを移植する。
バックエンド自体の置換ではなく、MCP、extension skills、思考要約、モデル・effort設定はupstream方式を維持する。

## Prompt

agyのcustom agent本文は会話作成時に固定されるため、毎ターン変わるkojoの指示は`.agents/rules/kojo-system-*.md`（`always_on`）へ出す。
UTF-8と改行境界を考慮して各ファイルを23KB以下に分割する。古いkojo管理chunk（`kojo-system-NN.md`）だけを削除し、同じprefixのユーザー定義ruleを含め他は残す。
custom agentはupstreamのtool whitelistを維持し、本文はrulesを参照する固定文にする。
MCP設定とrulesは一つのworkspace holdで保持し、別promptのターンに上書きされないようにする。

すでに旧upstream方式で`gemini-agy`の会話を作成している場合は、切替時にCLIセッションを一度リセットする。
これにより古いcustom agent本文を保持したまま再開することを避ける。kojoの会話履歴や記憶の削除は不要。
rules全体の大きさがagy側の予算を超えた場合にはファイル参照へ縮約される可能性がある点は、旧試験版と同じ制約である。

## 会話の後始末

会話参照はupstreamの`.agy/conversation_id`と`.agy/threads/key-*`を維持する。
加えて、保持・削除再試行が必要な会話IDを`.agy/conversations`へ記録する。参照が置き換わった会話や失敗・キャンセルした会話もresetで掃除できる。削除済みOneShotはledgerへ追加しない。

- CLIセッションreset・記憶初期化・truncate/rewindで参照と、このagentが記録した会話実体を削除する。
- OneShotでは永続参照と共有されていない会話実体を、成功・失敗・キャンセル時に削除する。
- 削除対象はID形式を検証した会話DB、そのsidecar、annotations、presence、brain。このagentの参照・ledgerにない会話IDは走査・削除しない。
- 参照の読込みに失敗した場合は削除を開始しない。実体削除に失敗したIDはledgerに残し、次のresetで再試行する。
- Agyのcleanupが失敗したresetは成功扱いにしない。truncate/rewindはkojoのtranscriptを先に更新するため、cleanup失敗時はその部分実行を示すエラーを返す。
- agyが所有する共有`conversation_summaries.db`の行は変更しない。完全な痕跡消去を保証する機能ではない。

## 停止と安全性

cancelとresult後の終了待ち上限で、開始時のPID identityに結び付けたprocess-tree killを実行する。
別session/PTYへ移った子孫も対象にし、kill処理完了後にChatを終える。
状態・rules・MCP設定の親ディレクトリや参照ファイルのsymlinkを拒否する。
親からの`ANTIGRAVITY_CONVERSATION_ID`は継承しない。

## 表示と診断

別response stepの間に段落を入れ、最終本文のIDE専用`file://`リンクをラベルへ整形する。
toolの`ERROR`も完了表示し、stdoutのfatal行・eventなしのエラー・`denied_actions`を診断に残す（本文がある場合はログのみ、失敗statusではエラー文に併記）。

## 対象外

- `antigravity`という旧tool名・`.antigravity`参照の自動移行。
- upstreamが除外しているbrowser系／command_status等の組み込みtoolの再公開。custom agentで拒否されるtool名や意図的な制限を戻さない。
- 新しいdevice-switch skill対応やPTYのPATH fallback統一。旧試験版にもなかった機能のため分離する。
