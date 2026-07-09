// Lightweight, dependency-free UI internationalization (Japanese / English).
//
// Design:
//   - Locale is a global module-level value with a subscriber set, exposed to
//     React via useSyncExternalStore (useLocale). setLocale() persists the
//     override to localStorage and notifies every subscriber, so a language
//     switch re-renders the whole app without a provider tree.
//   - Detection order: localStorage "kojo.locale" override → navigator.language
//     ("ja*" → ja) → "en".
//   - Dictionaries are keyed by stable English-ish keys; each entry has ja+en.
//   - t(key, params?) interpolates {name}-style placeholders. A missing key
//     returns the key itself (fail-soft) and console.warns in dev.
//
// Only user-visible UI strings live here. Log/console text, API error codes,
// model/tool names, file paths, and anything sent to the server stay literal.

import { useSyncExternalStore } from "react";

export type Locale = "ja" | "en";

const STORAGE_KEY = "kojo.locale";

function detect(): Locale {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === "ja" || saved === "en") return saved;
  } catch {
    /* localStorage unavailable (private mode / SSR) — fall through */
  }
  if (typeof navigator !== "undefined" && navigator.language?.startsWith("ja")) {
    return "ja";
  }
  return "en";
}

let current: Locale = detect();
const listeners = new Set<() => void>();

export function getLocale(): Locale {
  return current;
}

export function setLocale(loc: Locale): void {
  if (loc === current) return;
  current = loc;
  try {
    localStorage.setItem(STORAGE_KEY, loc);
  } catch {
    /* ignore persistence failure */
  }
  for (const l of listeners) l();
}

function subscribe(cb: () => void): () => void {
  listeners.add(cb);
  return () => listeners.delete(cb);
}

/** Subscribe a component to locale changes; returns the current locale. */
export function useLocale(): Locale {
  return useSyncExternalStore(subscribe, getLocale, getLocale);
}

/**
 * Hook form of {@link t}. Subscribes the calling component to locale changes
 * and returns the same `t` function, so components re-render on setLocale().
 */
export function useT(): typeof t {
  useLocale();
  return t;
}

interface Entry {
  ja: string;
  en: string;
}

const messages = {
  // ── Shared ──
  "common.cancel": { ja: "キャンセル", en: "Cancel" },
  "common.close": { ja: "閉じる", en: "Close" },
  "common.back": { ja: "戻る", en: "Back" },
  "common.dismiss": { ja: "閉じる", en: "Dismiss" },
  "common.settings": { ja: "設定", en: "Settings" },
  "common.saved": { ja: "保存した", en: "Saved" },
  "common.removeName": { ja: "{name} を削除", en: "Remove {name}" },

  // ── Stale-frontend reload prompt ──
  "reload.available": {
    ja: "新しいバージョンがあります。リロードしてください",
    en: "A new version is available. Reload.",
  },
  "reload.action": { ja: "リロード", en: "Reload" },

  // ── Daemon self-update prompt ──
  "update.available": {
    ja: "kojo {latest} が利用可能です",
    en: "kojo {latest} is available",
  },
  "update.action": { ja: "更新", en: "Update" },
  "update.updating": {
    ja: "更新中… アプリが再接続します",
    en: "Updating… the app will reconnect",
  },
  "update.unsupported": {
    ja: "端末で kojo update を実行して更新してください",
    en: "Run kojo update in a terminal to update",
  },
  "update.notes": { ja: "リリースノート", en: "Release notes" },

  // ── Composer (shared by AgentChat + GroupDMChat) ──
  "composer.olderMessages": { ja: "過去のメッセージ", en: "older messages" },
  "composer.attachFiles": { ja: "ファイルを添付", en: "Attach files" },
  "composer.voiceInput": { ja: "音声入力", en: "Voice input" },
  "composer.stopVoice": { ja: "音声入力を停止", en: "Stop voice input" },
  "composer.send": { ja: "送信", en: "Send" },
  "composer.stop": { ja: "停止", en: "Stop" },

  // ── Dashboard ──
  "dash.fleetSummary": {
    ja: "{running} 稼働中 · {agents} エージェント · {sessions} セッション",
    en: "{running} running · {agents} agents · {sessions} sessions",
  },
  "dash.fleetDms": { ja: " · {count} DM", en: " · {count} DMs" },
  "dash.enableNotifPrompt": {
    ja: "セッション完了時に通知を受け取る?",
    en: "Enable notifications when sessions finish?",
  },
  "dash.enable": { ja: "有効化", en: "Enable" },
  "dash.agents": { ja: "エージェント", en: "Agents" },
  "dash.noAgents": { ja: "エージェントがまだない", en: "No agents yet" },
  "dash.threads": { ja: "スレッド", en: "Threads" },
  "dash.groupDms": { ja: "グループ DM", en: "Group DMs" },
  "dash.noGroupDms": { ja: "グループ DM がない", en: "No group DMs" },
  "dash.sessions": { ja: "セッション", en: "Sessions" },
  "dash.noSessions": { ja: "セッションがない", en: "No sessions" },
  "dash.cronPaused": { ja: "cron 停止中", en: "cron paused" },
  "dash.cronRunning": { ja: "cron 稼働中", en: "cron running" },
  "dash.mentionsYou": { ja: "あなた宛てのメンション", en: "Mentions you" },
  "dash.unread": { ja: "{count} 未読", en: "{count} unread" },
  "dash.collapse": { ja: "折りたたむ", en: "Collapse" },
  "dash.expand": { ja: "展開", en: "Expand" },
  "dash.processing": { ja: "処理中", en: "Processing" },
  "dash.awaitingAnswer": { ja: "回答待ち", en: "Awaiting answer" },
  "dash.transferring": { ja: "転移中 @ {peer}", en: "Transferring @ {peer}" },
  "dash.transferringPreview": {
    ja: "転移中 @ {peer} — 最新発言はこの端末では未反映",
    en: "Transferring @ {peer} — latest reply not reflected on this device",
  },
  "dash.noMessagesYet": { ja: "メッセージがまだない", en: "No messages yet" },
  "dash.youPrefix": { ja: "あなた: ", en: "You: " },
  "dash.forceReclaim": { ja: "強制復帰", en: "Force-reclaim" },
  "dash.forceReclaimTitle": {
    ja: "強制復帰: agent_locks をこの端末に書き戻してランタイムを再起動する。端末切替でエージェントが到達不能なピアに取り残された時に使う。",
    en: "Force-reclaim: rewrite agent_locks back to this host and restart the runtime. Use when device-switch left the agent stuck on an unreachable peer.",
  },
  "dash.forceReclaimConfirm": {
    ja: '"{name}" をこの端末に強制復帰する?\n現在の holder ({holder}) との通信を放棄し、この端末でランタイムを再起動する。',
    en: 'Force-reclaim "{name}" to this host?\nAbandon communication with the current holder ({holder}) and restart the runtime on this device.',
  },
  "dash.forceReclaimFailed": { ja: "強制復帰に失敗: {err}", en: "Force-reclaim failed: {err}" },
  "dash.newThread": { ja: "新規スレッド", en: "New thread" },
  "dash.newThreadWith": { ja: "{name} との新規スレッド", en: "New thread with {name}" },
  "dash.plusGroup": { ja: "+ グループ", en: "+ Group" },
  "dash.newSession": { ja: "新規セッション", en: "New session" },
  "dash.remove": { ja: "削除", en: "Remove" },
  "dash.exit": { ja: "exit {code}", en: "exit {code}" },
  "dash.exitParen": { ja: "(exit {code})", en: "(exit {code})" },
  "dash.more": { ja: "他 {count} 件", en: "+{count} more" },
  "dash.hide": { ja: "隠す", en: "Hide" },
  "dash.newGroupDm": { ja: "新規グループ DM", en: "New group DM" },
  "dash.name": { ja: "名前", en: "Name" },
  "dash.notifyMembers": { ja: "メンバーに通知", en: "Notify members" },
  "dash.members": { ja: "メンバー", en: "Members" },
  "dash.selected": { ja: "{count} 人選択", en: "{count} selected" },
  "dash.selectMin2": { ja: "メンバーを2人以上選んで", en: "Select at least 2 members" },
  "dash.createGroupFailed": { ja: "グループ作成に失敗", en: "Failed to create group" },
  "dash.creating": { ja: "作成中...", en: "Creating..." },
  "dash.create": { ja: "作成", en: "Create" },

  // ── AgentChat ──
  "chat.errorPrefix": { ja: "⚠️ エラー: {msg}", en: "⚠️ Error: {msg}" },
  "chat.errorGeneric": { ja: "エラーが発生した", en: "An error occurred" },
  "chat.hostOffline": { ja: "ホストがオフライン @ {peer}", en: "host offline @ {peer}" },
  "chat.typing": { ja: "出力中…", en: "typing…" },
  "chat.online": { ja: "オンライン", en: "online" },
  "chat.connecting": { ja: "接続中…", en: "connecting…" },
  "chat.autoTtsOn": { ja: "自動読み上げ: ON", en: "Auto TTS: ON" },
  "chat.autoTtsOff": { ja: "自動読み上げ: OFF", en: "Auto TTS: OFF" },
  "chat.credentials": { ja: "認証情報", en: "Credentials" },
  "chat.todos": { ja: "ToDo", en: "Todos" },
  "chat.dataFolder": { ja: "データフォルダ", en: "Data folder" },
  "chat.settings": { ja: "設定", en: "Settings" },

  // ── AgentTodos ──
  "todos.title": { ja: "ToDoリスト", en: "Todo list" },
  "todos.openCount": { ja: "件", en: "open" },
  "todos.addPlaceholder": { ja: "ToDoを追加…", en: "Add a todo…" },
  "todos.add": { ja: "追加", en: "Add" },
  "todos.empty": { ja: "ToDoはまだない", en: "No todos yet" },
  "todos.emptyHint": {
    ja: "上の入力欄から追加できる。エージェント自身もToDo APIで追加する",
    en: "Add one above — the agent can also add todos via its API",
  },
  "todos.doneSection": { ja: "完了 {count}", en: "Done {count}" },
  "todos.markDone": { ja: "完了にする", en: "Mark done" },
  "todos.reopen": { ja: "未完了に戻す", en: "Reopen" },
  "todos.delete": { ja: "削除", en: "Delete" },
  "todos.editTitle": { ja: "タイトルを編集", en: "Edit title" },
  "todos.conflict": {
    ja: "別の場所で先に更新されていた。最新の状態を読み込み直した",
    en: "Changed elsewhere first — reloaded the latest state",
  },
  "chat.emptyPrompt": { ja: "メッセージを送って会話を始めて", en: "Send a message to start chatting" },
  "chat.holderOfflineBannerPre": { ja: "ホスト端末 ", en: "Host device " },
  "chat.holderOfflineBannerPost": {
    ja: " がオフライン。送信したメッセージは復帰時に配送する。",
    en: " is offline. Messages you send will be delivered when it reconnects.",
  },
  "chat.queuedNotice": {
    ja: "キュー登録済み — 端末 {peer} の復帰時に配送する。",
    en: "Queued — will deliver when device {peer} reconnects.",
  },
  "chat.attachmentsCantQueue": {
    ja: "添付はキューに登録できない — 外すか、端末の復帰を待って。",
    en: "Attachments can't be queued — remove them or wait for the device to reconnect.",
  },
  "chat.queueFull": {
    ja: "このエージェントのキューが満杯 (最大100件) — キューを1件取り消すか、端末の復帰を待って。",
    en: "Queue is full for this agent (100 messages max) — cancel a queued message or wait for the device to reconnect.",
  },
  "chat.xaiKeyMissing": {
    ja: "xAI API キーが未設定。設定画面で登録して。",
    en: "xAI API key is not set. Register it in Settings.",
  },
  "chat.holderPeerOffline": { ja: "ホストピアがオフライン", en: "Holder peer offline" },
  "chat.steerPlaceholder": {
    ja: "実行中のターンに割り込む… ({key} で送信)",
    en: "Steer the running turn… ({key} to send)",
  },
  "chat.messagePlaceholder": { ja: "メッセージ… ({key} で送信)", en: "Message… ({key} to send)" },
  "chat.listening": { ja: "聞き取り中…", en: "Listening…" },
  "chat.steerTitle": { ja: "実行中のターンに割り込む", en: "Steer the running turn" },
  "chat.sendQueuedTitle": {
    ja: "ホストピアがオフライン — メッセージはキューに登録され @ {peer} の復帰時に配送する",
    en: "Holder peer is offline — message will be queued and delivered when @ {peer} reconnects",
  },

  // ── GlobalSettings ──
  "gs.language": { ja: "言語", en: "Language" },
  "gs.languageHelp": {
    ja: "この端末の表示言語。ブラウザに保存される。",
    en: "Display language for this device. Saved in your browser.",
  },

  // ── AgentSettings: sections ──
  "settings.sec.identity": { ja: "アイデンティティ", en: "Identity" },
  "settings.sec.injections": { ja: "コンテキスト注入", en: "Context Injections" },
  "settings.sec.model": { ja: "モデルとツール", en: "Model & Tools" },
  "settings.sec.schedule": { ja: "スケジュール", en: "Schedule" },
  "settings.sec.voice": { ja: "音声", en: "Voice" },
  "settings.sec.integrations": { ja: "連携", en: "Integrations" },
  "settings.sec.memory": { ja: "メモリ", en: "Memory" },
  "settings.sec.danger": { ja: "危険", en: "Danger" },

  // ── AgentSettings: injection checklist ──
  "settings.inj.user_context.label": { ja: "ユーザーコンテキスト", en: "User Context" },
  "settings.inj.user_context.desc": { ja: "ユーザープロフィール (user.md)", en: "User profile (user.md)" },
  "settings.inj.memory_md.label": { ja: "MEMORY.md", en: "MEMORY.md" },
  "settings.inj.memory_md.desc": {
    ja: "システムプロンプトに MEMORY.md の内容",
    en: "MEMORY.md contents in system prompt",
  },
  "settings.inj.credentials.label": { ja: "認証情報", en: "Credentials" },
  "settings.inj.credentials.desc": { ja: "認証情報の使い方ガイド", en: "Credentials usage guide" },
  "settings.inj.groupdm.label": { ja: "グループ DM", en: "Group DM" },
  "settings.inj.groupdm.desc": { ja: "グループ DM 機能", en: "Group DM capability" },
  "settings.inj.todo_api.label": { ja: "Todo", en: "Todos" },
  "settings.inj.todo_api.desc": {
    ja: "永続 Todo (ガイド + 毎ターンのリスト)",
    en: "Persistent todos (guide + per-turn list)",
  },
  "settings.inj.attachments.label": { ja: "添付", en: "Attachments" },
  "settings.inj.attachments.desc": { ja: "ファイル添付のステージング", en: "File attachment staging" },
  "settings.inj.status.label": { ja: "ステータス", en: "Status" },
  "settings.inj.status.desc": { ja: "エージェントのステータスブロック", en: "Agent status block" },
  "settings.inj.diary_notes.label": { ja: "日誌ノート", en: "Diary Notes" },
  "settings.inj.diary_notes.desc": {
    ja: "最近の活動日誌 (毎ターン)",
    en: "Recent activity diary (per turn)",
  },
  "settings.inj.memory_search.label": { ja: "メモリ検索", en: "Memory Search" },
  "settings.inj.memory_search.desc": {
    ja: "メモリ検索結果 (毎ターン)",
    en: "Memory search results (per turn)",
  },
  "settings.inj.recent_conversation.label": { ja: "直近の会話", en: "Recent Conversation" },
  "settings.inj.recent_conversation.desc": {
    ja: "セッション再開時の直近会話フォールバック",
    en: "Recent conversation fallback on session resume",
  },
  "settings.inj.persona_anchor.label": { ja: "口調アンカー", en: "Persona Anchor" },
  "settings.inj.persona_anchor.desc": {
    ja: "毎ターンの文脈末尾に注入される人格アンカー (anchor.md)",
    en: "Persona anchor appended to the per-turn context tail (anchor.md)",
  },

  // ── AgentSettings: card titles / descriptions ──
  "settings.card.identity.desc": {
    ja: "名前・人格・他者からの見え方。",
    en: "Name, persona, and how this agent appears to others.",
  },
  "settings.card.injections.desc": {
    ja: "このエージェントのシステムプロンプト / 毎ターンの文脈に注入する項目を選ぶ。外すとコンテキスト予算を少し節約できるが、その機能は失われる。",
    en: "Pick which pieces of context get injected into this agent's system prompt / per-turn context. Unchecking one saves a little context budget at the cost of that capability.",
  },
  "settings.card.model.desc": {
    ja: "バックエンド・モデル・権限。",
    en: "Backend, model, and capability permissions.",
  },
  "settings.card.schedule.desc": {
    ja: "このエージェントが自走するタイミングと、静かにするタイミング。",
    en: "When this agent runs on its own, and when it stays quiet.",
  },
  "settings.card.voice.desc": {
    ja: "Gemini か xAI Grok の TTS で返信を読み上げる。メッセージごとの手動再生。自動再生はチャットヘッダーで切り替え。",
    en: "Read assistant replies out loud via Gemini or xAI Grok TTS. Manual playback per message; auto playback toggled in the chat header.",
  },
  "settings.card.memory.desc": {
    ja: "CLIセッションを仕切り直す。メモリファイルやチャット履歴は保持される。履歴の削除は危険ゾーンで。",
    en: "Start a fresh CLI session. Memory files and chat history are kept. Destructive cleanup lives in the Danger Zone.",
  },
  "settings.card.danger": { ja: "危険ゾーン", en: "Danger Zone" },

  // ── AgentSettings: Identity fields ──
  "settings.changeAvatar": { ja: "アバターを変更", en: "Change Avatar" },
  "settings.generate": { ja: "生成", en: "Generate" },
  "settings.generating": { ja: "生成中...", en: "Generating..." },
  "settings.name": { ja: "名前", en: "Name" },
  "settings.personaPromptPlaceholder": { ja: "例: もっと毒舌にして", en: "e.g. make it snarkier" },
  "settings.templateNotSaved": { ja: "テンプレート — 未保存。", en: "Template — not yet saved." },
  "settings.userContextLabel": { ja: "ユーザーコンテキスト", en: "User Context" },
  "settings.userContextHelp": {
    ja: "このエージェントが関わる人についてのメモ — 名前・タイムゾーン・コミュニケーションの好みなど。データとしてシステムプロンプトに注入される (1500文字超は前後を残して省略)。",
    en: "Notes about the people this agent works with — name, timezone, communication preferences, etc. Injected into the system prompt as data (head/tail truncated above 1500 chars).",
  },
  "settings.statusLabel": { ja: "ステータス", en: "Status" },
  "settings.statusHelp": {
    ja: "エージェントが自分で管理する状態 (気分・エネルギー・眠気…)。システムプロンプトに注入される。エージェントが状態の変化に合わせて自分で更新する。ここでの編集は上書きになる。",
    en: "The agent's self-maintained state (mood, energy, sleepiness, ...) injected into its system prompt. The agent updates this on its own as its state drifts; edits here override it.",
  },
  "settings.anchorLabel": { ja: "口調アンカー", en: "Persona Anchor" },
  "settings.anchorHelp": {
    ja: "毎ターンの文脈末尾に注入される2〜3行の人格要約 (一人称・口調・態度)。空なら何も注入されない。長文はトークン税になるので短く。",
    en: "A 2-3 line persona summary (first person, tone, attitude) appended to the per-turn context tail. Nothing is injected when empty. Keep it short — long text costs tokens.",
  },
  "settings.publicProfile": { ja: "公開プロフィール", en: "Public Profile" },
  "settings.override": { ja: "上書き", en: "Override" },
  "settings.publicProfileHelpOverride": {
    ja: "手動の上書き — 人格を変えても置き換わらない。",
    en: "Manual override — won't be replaced when persona changes.",
  },
  "settings.publicProfileHelpAuto": {
    ja: "人格から自動生成。ディレクトリ経由で他のエージェントから見える。",
    en: "Auto-generated from persona. Visible to other agents via directory.",
  },
  "settings.publicProfilePlaceholderOverride": {
    ja: "カスタム公開プロフィールを入力",
    en: "Enter custom public profile",
  },
  "settings.publicProfilePlaceholderAuto": {
    ja: "人格から自動生成",
    en: "Auto-generated from persona",
  },

  // ── AgentSettings: Model & Tools ──
  "settings.autoEffort": { ja: "自動 Effort", en: "Auto Effort" },
  "settings.autoEffortDesc": {
    ja: "タスクの難易度に応じて毎ターンの effort を自動で選ぶ。Effort 設定は上限 / フォールバックになる。",
    en: "Pick per-turn effort automatically based on task difficulty; the Effort setting becomes the ceiling/fallback.",
  },
  "settings.customBaseUrl": { ja: "カスタム Base URL", en: "Custom Base URL" },
  "settings.customBaseUrlHelp": {
    ja: "Anthropic Messages API 互換のエンドポイント",
    en: "Anthropic Messages API compatible endpoint",
  },
  "settings.allowedTools": { ja: "許可ツール", en: "Allowed Tools" },
  "settings.allEmpty": { ja: "(空 = すべて)", en: "(empty = all)" },
  "settings.allowProtectedPaths": { ja: "保護パスの編集を許可", en: "Allow Edits in Protected Paths" },
  "settings.bypassGuard": { ja: "(claude-code ガードを回避)", en: "(bypass claude-code guard)" },
  "settings.allowProtectedPathsHelp": {
    ja: "最近の claude-code は bypassPermissions でも .claude / .git / .husky への Edit/Write で確認を求める。抑制するにはチェック。",
    en: "Recent claude-code versions prompt on Edit/Write to .claude, .git, .husky even with bypassPermissions. Check to suppress.",
  },
  "settings.thinking": { ja: "思考", en: "Thinking" },
  "settings.thinkingAuto": { ja: "auto (サーバー既定)", en: "auto (server default)" },
  "settings.privileged": { ja: "特権エージェント", en: "Privileged Agent" },
  "settings.privilegedDesc": {
    ja: "このエージェントに API 経由で他のエージェントの削除 / リセット / アーカイブを許可する。他エージェントのフォークや完全な記録の読み取りはできない。",
    en: "Allow this agent to delete / reset / archive other agents via the API. Cannot fork or read other agents' full record.",
  },

  // ── AgentSettings: Schedule ──
  "settings.notifyDuringSilent": { ja: "静音時間中も DM を受信", en: "Receive DM During Silent Hours" },
  "settings.notifyDuringSilentDesc": {
    ja: "有効時は静音時間中でもグループ DM 通知を配送する。無効時は通知を抑制する (メッセージ自体は残る)。",
    en: "When enabled, group DM notifications are delivered even during silent hours. When disabled, notifications are suppressed (messages remain in the transcript).",
  },

  // ── AgentSettings: Voice ──
  "settings.provider": { ja: "プロバイダ", en: "Provider" },
  "settings.providerHelp": {
    ja: "Gemini は自由記述のスタイルプロンプトを使う。Grok にスタイルプロンプトはなく、話し方は音声と返信中のインライン音声タグ ([pause]、[laugh]、<whisper>…</whisper>) で制御する。",
    en: "Gemini uses a free-form style prompt; Grok has no style prompt — control delivery with the voice and inline speech tags ([pause], [laugh], <whisper>…</whisper>) embedded in replies.",
  },
  "settings.model": { ja: "モデル", en: "Model" },
  "settings.default": { ja: "既定", en: "Default" },
  "settings.voice": { ja: "音声", en: "Voice" },
  "settings.playing": { ja: "▶ 再生中...", en: "▶ Playing..." },
  "settings.preview": { ja: "▶ プレビュー", en: "▶ Preview" },
  "settings.playbackError": { ja: "再生エラー", en: "Playback error" },
  "settings.voiceHelpPre": { ja: "", en: "Use " },
  "settings.voiceHelpLink": { ja: "プレビュー", en: "Preview" },
  "settings.voiceHelpPost": { ja: " で試聴。", en: " to listen." },
  "settings.browseVoices": { ja: "{count} 個の音声を一覧", en: "Browse all {count} voices" },
  "settings.grokNoStyle": {
    ja: "Grok にスタイルプロンプトはない。話し方は音声と返信テキスト中のインライン音声タグで決まる — 例: ",
    en: "Grok has no style prompt. Delivery is set by the voice and by inline speech tags in the reply text — e.g. ",
  },
  "settings.stylePrompt": { ja: "スタイルプロンプト", en: "Style Prompt" },
  "settings.stylePromptHelpText": {
    ja: "テキストの前に付ける自由記述プロンプト。[whispers]、[excited]、[laughs] などの音声タグをインラインで埋め込める。",
    en: "Free-form prompt prepended to the text. Audio tags such as [whispers], [excited], [laughs] can be embedded inline.",
  },
  "settings.stylePromptReference": { ja: "参考: ", en: "Reference: " },
  "settings.stylePromptGuide": { ja: "Gemini TTS プロンプトガイド", en: "Gemini TTS prompt guide" },
  "settings.stylePromptPlaceholder": {
    ja: "落ち着いた日本語で、淡々と短く読み上げて。",
    en: "Read in calm Japanese, plainly and briefly.",
  },
  "settings.ffmpegWarn": {
    ja: "ffmpeg が見つからない — WAV 出力のみ。ffmpeg を入れると Opus/MP3 (はるかに小さい) が使える。",
    en: "ffmpeg not detected — only WAV output is available. Install ffmpeg to enable Opus/MP3 (much smaller).",
  },
  "settings.saving": { ja: "保存中...", en: "Saving..." },
  "settings.enableTts": { ja: "TTS を有効化", en: "Enable TTS" },

  // ── AgentSettings: Memory ──
  "settings.truncateLabel": { ja: "この時刻以降のメモリを削除", en: "Truncate memory since" },
  "settings.truncateHelp": {
    ja: "この時刻以降に記録されたトランスクリプト・Claude --resume セッションエントリ・grok --resume セッション (丸ごと削除)・日次日誌の項目を削除する。人格・MEMORY.md・プロジェクト / 人物 / トピックのノート・アーカイブ・認証情報は保持される。",
    en: "Drop transcript records, Claude --resume session entries, the grok --resume session (dropped wholesale), and daily diary bullets recorded at or after this instant. Persona, MEMORY.md, project / people / topic notes, archive, and credentials are kept.",
  },
  "settings.truncating": { ja: "削除中...", en: "Truncating..." },
  "settings.truncateButton": { ja: "この時刻以降のメモリを削除", en: "Truncate Memory From This Time" },
  "settings.truncateThreshold": { ja: "しきい値: ", en: "Threshold: " },
  "settings.truncateResult": {
    ja: "トランスクリプト: {messages} · Claude セッション: {claudeEntries} エントリ / {claudeFiles} ファイル · Grok セッション: {grokSessions} セッション / {grokFiles} ファイル · 日誌: {diaryEntries} エントリ / {diaryFiles} ファイル",
    en: "Transcript: {messages} · Claude session: {claudeEntries} entries / {claudeFiles} files · Grok session: {grokSessions} sessions / {grokFiles} files · Diary: {diaryEntries} entries / {diaryFiles} files",
  },
  "settings.resetting": { ja: "リセット中...", en: "Resetting..." },
  "settings.resetData": { ja: "データをリセット", en: "Reset Data" },
  "settings.resetDataHelp": {
    ja: "会話ログとメモリを消す。設定・人格・アバター・認証情報は保持される。",
    en: "Clear conversation logs and memory. Settings, persona, avatar, and credentials are kept.",
  },

  // ── AgentSettings: banners / save ──
  "settings.saveConflict": {
    ja: "他の誰かがこのエージェントを更新した。再読み込み中…",
    en: "Someone else updated this agent. Reloading…",
  },
  "settings.saveChanges": { ja: "変更を保存", en: "Save Changes" },
  "settings.unsavedChanges": { ja: "未保存の変更がある", en: "Unsaved changes" },
  "settings.discard": { ja: "破棄", en: "Discard" },

  // ── AgentSettings: Danger Zone ──
  "settings.resetCliSession": { ja: "CLI セッションをリセット", en: "Reset CLI Session" },
  "settings.resetCliSessionHelp": {
    ja: "コンテキストウィンドウを作り直す。履歴とメモリは保持されるが、AI は全部を一から読み直す。",
    en: "Force a fresh context window. History and memory are kept, but the AI re-reads everything from scratch.",
  },
  "settings.forkAgent": { ja: "エージェントをフォーク", en: "Fork Agent" },
  "settings.forkAgentHelp": {
    ja: "人格とメモリを引き継いだコピーを作る。Slack・通知・認証情報は引き継がれない。",
    en: "Create a copy with persona and memory carried over. Slack, notifications, and credentials are not transferred.",
  },
  "settings.archiving": { ja: "アーカイブ中...", en: "Archiving..." },
  "settings.archiveAgent": { ja: "エージェントをアーカイブ", en: "Archive Agent" },
  "settings.archiveAgentHelp": {
    ja: "メインリストから隠してランタイム活動を止める。データは保持され、設定から復元できる。全グループ DM から外れる (アーカイブ解除しても復帰しない)。",
    en: "Hide from the main list and stop runtime activity. Data is kept; restore from Settings. Removes the agent from all group DMs (memberships are NOT restored on unarchive).",
  },
  "settings.deleting": { ja: "削除中...", en: "Deleting..." },
  "settings.deleteAgent": { ja: "エージェントを削除", en: "Delete Agent" },
  "settings.idLabel": { ja: "ID: {id}", en: "ID: {id}" },
  "settings.createdLabel": { ja: "作成: {date}", en: "Created: {date}" },

  // ── AgentSettings: Fork dialog ──
  "settings.forkDialogTitle": { ja: "エージェントをフォーク", en: "Fork agent" },
  "settings.forkIncludeHistory": { ja: "会話履歴を含める", en: "Include conversation history" },
  "settings.forkAlwaysCopied": {
    ja: "人格とメモリは常にコピーされる。",
    en: "Persona and memory are always copied.",
  },
  "settings.forkNotTransferred": {
    ja: "Slack ボット・通知ソース・認証情報は引き継がれない。",
    en: "Slack bot, notification sources, and credentials are not transferred.",
  },
  "settings.forking": { ja: "フォーク中…", en: "Forking…" },
  "settings.fork": { ja: "フォーク", en: "Fork" },

  // ── AgentSettings: confirm / alert / notice / error ──
  "settings.checkinSaveFirst": {
    ja: "先に変更を保存して — 手動チェックインは保存済みのチェックインメッセージとタイムアウトを使う。",
    en: "Save your changes first — manual check-in uses the saved Check-in Message and Timeout.",
  },
  "settings.checkinStarted": {
    ja: "チェックイン開始 — 終わったらチャットで返信する。",
    en: "Check-in started — the agent will reply in chat when it finishes.",
  },
  "settings.checkinSkipped": {
    ja: "チェックインをスキップ — エージェントは今別の作業中。",
    en: "Check-in skipped — the agent is already working on something.",
  },
  "settings.resetSessionConfirm": {
    ja: "CLI セッションをリセットする? 会話履歴とメモリは保持されるが、AI は新しいコンテキストウィンドウで始める。",
    en: "Reset CLI session? Conversation history and memory are kept, but the AI will start a fresh context window.",
  },
  "settings.pickDate": { ja: "削除する起点の日時を選んで。", en: "Pick a date/time to truncate from." },
  "settings.truncateConfirm": {
    ja: "{iso} 以降に記録された全メモリを削除する? kojo のトランスクリプト・Claude --resume セッションエントリ (末尾ターンの後処理あり)・grok --resume セッション全体 (events.jsonl にレコード単位のタイムスタンプがなく部分削除は安全でない — 次ターンは新セッションで開く)・該当する日次日誌の項目を削除する。人格・MEMORY.md・プロジェクト / 人物 / トピックのノート・認証情報は保持される。",
    en: "Delete every memory recorded at or after {iso}? This drops kojo transcript records, Claude --resume session entries (with trailing-turn cleanup), the entire grok --resume session (events.jsonl has no per-record timestamp so partial cuts are not safe — the next turn opens a fresh session), and matching daily diary bullets. Persona, MEMORY.md, project / people / topic notes, and credentials are kept.",
  },
  "settings.resetDataConfirm": {
    ja: "会話ログとメモリをリセットする? 設定・人格・アバター・認証情報は保持される。",
    en: "Reset conversation logs and memory? Settings, persona, avatar, and credentials will be kept.",
  },
  "settings.nameRequired": { ja: "名前は必須", en: "Name is required" },
  "settings.deleteConfirm": {
    ja: "このエージェントを削除する? 取り消せない。",
    en: "Delete this agent? This cannot be undone.",
  },
  "settings.archiveConfirm": {
    ja: "このエージェントをアーカイブする? ランタイム活動は止まるがデータは保持され、設定から復元できる。\n\n全グループ DM から外れ (2人グループは解散)、アーカイブ解除してもメンバーシップは復帰しない — 再招待が必要。",
    en: "Archive this agent? Runtime activity stops; data is kept and can be restored from Settings.\n\nThe agent will be removed from all group DMs (2-person groups dissolve), and memberships are NOT restored on unarchive — the agent must be re-invited.",
  },

  // ── AgentDataBrowser ──
  "adb.rootLabel": { ja: "データ", en: "Data" },

  // ── TransferSkipsNotice ──
  "skips.title": {
    ja: "直前のデバイス転移でスキップされたセッションファイルがある",
    en: "Some session files were skipped during the last device transfer",
  },
  "skips.summary": {
    ja: "転移時にスキップされたファイル: {count}件",
    en: "Files skipped during transfer: {count}",
  },

  // ── RateLimitBadge ──
  "rate.status": { ja: "レート制限: {status}", en: "Rate limit: {status}" },
  "rate.window": { ja: "ウィンドウ: {window}", en: "Window: {window}" },
  "rate.utilization": { ja: "使用率: {pct}%", en: "Utilization: {pct}%" },
  "rate.resets": { ja: "リセット: {time}", en: "Resets: {time}" },
  "rate.statusAllowed": { ja: "許可", en: "allowed" },
  "rate.statusWarning": { ja: "警告", en: "warning" },
  "rate.statusRejected": { ja: "拒否", en: "rejected" },
  "rate.window7d": { ja: "7日間", en: "7-day" },
  "rate.window5h": { ja: "5時間", en: "5-hour" },
  "rate.limitPct": { ja: "上限 {pct}%", en: "limit {pct}%" },

  // ── MediaOverlay ──
  "media.closePreview": { ja: "プレビューを閉じる", en: "Close preview" },
  "media.prevPreview": { ja: "前のプレビュー", en: "Previous preview" },
  "media.prev": { ja: "前へ", en: "Previous" },
  "media.nextPreview": { ja: "次のプレビュー", en: "Next preview" },
  "media.next": { ja: "次へ", en: "Next" },
  "media.videoUnplayable": {
    ja: "この動画形式はブラウザで再生できない。",
    en: "This video format cannot be played in the browser.",
  },
  "media.download": { ja: "ダウンロード", en: "Download" },

  // ── StreamingMessage / ChatMessage ──
  "stream.thinking": { ja: "思考中…", en: "Thinking…" },
  "stream.thought": { ja: "思考", en: "Thought" },
  "stream.showPlain": { ja: "プレーンテキストを表示", en: "Show plain text" },
  "stream.showRendered": { ja: "レンダリング表示", en: "Show rendered" },
  "stream.raw": { ja: "Raw", en: "Raw" },
  "stream.render": { ja: "Render", en: "Render" },
  "msg.copy": { ja: "コピー", en: "Copy" },
  "msg.copied": { ja: "コピーした", en: "Copied" },
  "msg.edit": { ja: "編集", en: "Edit" },
  "msg.delete": { ja: "削除", en: "Delete" },
  "msg.deleteConfirm": { ja: "このメッセージを削除する?", en: "Delete this message?" },
  "msg.deleteFailed": { ja: "メッセージの削除に失敗", en: "Failed to delete message" },
  "msg.saveFailed": { ja: "メッセージの保存に失敗", en: "Failed to save message" },
  "msg.regenerate": { ja: "再生成", en: "Regenerate" },
  "msg.ttsLoading": { ja: "読み込み中...", en: "Loading..." },
  "msg.ttsStop": { ja: "停止", en: "Stop" },
  "msg.ttsError": { ja: "TTS エラー — 再試行", en: "TTS error — retry" },
  "msg.ttsPlay": { ja: "再生", en: "Play" },
  "msg.toolOne": { ja: "1 ツール", en: "1 tool" },
  "msg.toolMany": { ja: "{count} ツール", en: "{count} tools" },

  // ── SystemMessage ──
  "sysmsg.hideBody": { ja: "通知の本文を隠す", en: "Hide notification body" },
  "sysmsg.showBody": { ja: "通知の本文を表示", en: "Show notification body" },
  "sysmsg.msgs": { ja: "{count} 件", en: "{count} msgs" },

  // ── ToolUseCard ──
  "tool.subCount": { ja: "{count} sub", en: "{count} sub" },
  "tool.backgroundDone": { ja: "バックグラウンド完了", en: "background done" },
  "tool.backgroundRunning": { ja: "バックグラウンド実行中", en: "background running" },
  "tool.input": { ja: "入力", en: "Input" },
  "tool.output": { ja: "出力", en: "Output" },
  "tool.subagent": { ja: "サブエージェント", en: "Subagent" },

  // ── UserQuestionCard ──
  "uq.answered": { ja: "回答済み: {answers}", en: "Answered: {answers}" },
  "uq.expired": {
    ja: "この質問は期限切れ (ターンが終了した)。",
    en: "This question has expired (the turn ended).",
  },
  "uq.question": { ja: "質問", en: "Question" },
  "uq.otherPlaceholder": { ja: "その他 (自由入力)", en: "Other (free text)" },
  "uq.submitting": { ja: "送信中…", en: "Submitting…" },
  "uq.submit": { ja: "回答する", en: "Answer" },

  // ── QueuedMessages ──
  "queued.one": { ja: "1 件のメッセージをキュー登録", en: "1 message queued" },
  "queued.many": { ja: "{count} 件のメッセージをキュー登録", en: "{count} messages queued" },
  "queued.deliverPre": { ja: " — 端末 ", en: " — will deliver when device " },
  "queued.deliverPost": { ja: " の復帰時に配送する", en: " reconnects" },
  "queued.cancelAria": {
    ja: "キュー登録済みメッセージ {id} を取り消す",
    en: "Cancel queued message {id}",
  },

  // ── SlackBotSettings ──
  "slack.connectedResult": {
    ja: "接続した: team={team}, bot={bot}",
    en: "Connected: team={team}, bot={bot}",
  },
  "slack.removeConfirm": { ja: "Slack ボット設定を削除する?", en: "Remove Slack bot configuration?" },
  "slack.connected": { ja: "接続済み", en: "Connected" },
  "slack.enableAria": { ja: "Slack ボットを有効化", en: "Enable Slack bot" },
  "slack.appToken": { ja: "App-Level Token (xapp-...)", en: "App-Level Token (xapp-...)" },
  "slack.botToken": { ja: "Bot Token (xoxb-...)", en: "Bot Token (xoxb-...)" },
  "slack.configured": { ja: "設定済み", en: "configured" },
  "slack.threadReplies": { ja: "常にスレッドで返信", en: "Always reply in thread" },
  "slack.respondTo": { ja: "応答する対象", en: "Respond to" },
  "slack.respondDM": { ja: "ダイレクトメッセージ", en: "Direct messages" },
  "slack.respondMention": { ja: "チャンネル内の @メンション", en: "@mentions in channels" },
  "slack.respondThread": {
    ja: "スレッドの続き (メンションなしで自動返信)",
    en: "Thread follow-ups (auto-reply without mention)",
  },
  "slack.testing": { ja: "テスト中...", en: "Testing..." },
  "slack.testConnection": { ja: "接続テスト", en: "Test Connection" },
  "slack.removeBot": { ja: "Slack ボットを削除", en: "Remove Slack Bot" },
  "slack.setupHelp": {
    ja: "Socket Mode を有効にした Slack App を作る。必要なスコープ: chat:write, app_mentions:read, im:history。購読するイベント: message.im, app_mention。",
    en: "Create a Slack App with Socket Mode enabled. Required scopes: chat:write, app_mentions:read, im:history. Subscribe to events: message.im, app_mention.",
  },

  // ── MessageAttachments ──
  "attach.imageUnavailable": { ja: "画像を取得できない", en: "image unavailable" },
  "attach.downloadTitle": { ja: "{name} をダウンロード", en: "Download {name}" },

  // ── ScheduleEditor ──
  "sched.tabPreset": { ja: "プリセット", en: "Preset" },
  "sched.tabHourly": { ja: "毎時", en: "Hourly" },
  "sched.tabDaily": { ja: "毎日", en: "Daily" },
  "sched.tabWeekly": { ja: "毎週", en: "Weekly" },
  "sched.tabAdvanced": { ja: "詳細", en: "Advanced" },
  "sched.dowSun": { ja: "日", en: "Sun" },
  "sched.dowMon": { ja: "月", en: "Mon" },
  "sched.dowTue": { ja: "火", en: "Tue" },
  "sched.dowWed": { ja: "水", en: "Wed" },
  "sched.dowThu": { ja: "木", en: "Thu" },
  "sched.dowFri": { ja: "金", en: "Fri" },
  "sched.dowSat": { ja: "土", en: "Sat" },
  "sched.relAgo": { ja: "{amount} 前", en: "{amount} ago" },
  "sched.relIn": { ja: "{amount} 後", en: "in {amount}" },
  "sched.timeout": { ja: "タイムアウト", en: "Timeout" },
  "sched.timeoutHelp": {
    ja: "スケジュール実行と手動チェックインそれぞれの最大時間。",
    en: "Max duration for each scheduled or manual check-in run.",
  },
  "sched.resumeWindow": { ja: "Resume ウィンドウ", en: "Resume Window" },
  "sched.resumeWindowSub": {
    ja: "(claude セッションのリセットしきい値)",
    en: "(claude session reset threshold)",
  },
  "sched.resumeWindowHelp": {
    ja: "コンテキスト超過のセッションを、最後の対話ターンからどれだけの間 resume し続けるか。短いほど早くリセットし、長いほど長い休止をまたいでコンテキストを保つ。既定値は Anthropic のプロンプトキャッシュ TTL に合わせている。",
    en: "How long an over-context session keeps being resumed after the last interactive turn. Smaller resets sooner; larger keeps context across longer pauses. Default matches Anthropic's prompt-cache TTL.",
  },
  "sched.silentHours": { ja: "静音時間", en: "Silent Hours" },
  "sched.from": { ja: "開始", en: "From" },
  "sched.to": { ja: "終了", en: "To" },
  "sched.silentRange": {
    ja: "{start}–{end} は静音。この時間帯は停止する。(サーバー時刻)",
    en: "Silent {start}–{end}. Paused during this window. (server time)",
  },
  "sched.silentRangeOvernight": {
    ja: "{start}–24:00 と 0:00–{end} は静音 (日またぎ、サーバー時刻)。",
    en: "Silent {start}–24:00 & 0:00–{end} (overnight, server time).",
  },
  "sched.runs247": {
    ja: "24時間稼働。静かにする時間帯を設定するには有効化して。",
    en: "Runs 24/7. Enable to set quiet hours.",
  },
  "sched.nextCheckin": { ja: "次のチェックイン", en: "Next check-in" },
  "sched.pausedSuffix": { ja: "(停止中)", en: "(paused)" },
  "sched.saveToUpdate": { ja: "保存すると更新される", en: "save to update" },
  "sched.checkingIn": { ja: "チェックイン中…", en: "Checking in…" },
  "sched.checkinNow": { ja: "今すぐチェックイン", en: "Check in now" },
  "sched.checkinMessage": { ja: "チェックインメッセージ", en: "Check-in Message" },
  "sched.checkinMessageHelpPre": {
    ja: "定期・手動チェックインのプロンプト末尾の指示を置き換える。今日の日付 (YYYY-MM-DD) のプレースホルダとして ",
    en: "Replaces the trailing instruction in periodic and manual check-in prompts. Use ",
  },
  "sched.checkinMessageHelpPost": {
    ja: " を使える。空なら既定のまま。",
    en: " as a placeholder for today (YYYY-MM-DD). Leave blank for the default.",
  },
  "sched.checkinMessageHint": {
    ja: "最近の出来事や気づきがあれば memory/{date}.md に記録し、必要なタスクを実行して。",
    en: "If there are recent events or observations, record them in memory/{date}.md, and execute any necessary tasks.",
  },
  "sched.everyHourAt": { ja: "毎時", en: "Every hour at" },
  "sched.minuteUnit": { ja: "分", en: "min" },
  "sched.everyDayAt": { ja: "毎日", en: "Every day at" },
  "sched.weeklyNoDow": {
    ja: "曜日を 1 つ以上選んで。空のまま保存するとスケジュール無効になる。",
    en: "Pick at least one day of the week. Saving with none selected disables the schedule.",
  },
  "sched.presetOff": { ja: "オフ", en: "Off" },
  "sched.presetDaily9": { ja: "毎日 09:00", en: "Daily 09:00" },
  "sched.resumeDefault": { ja: "既定 (5分)", en: "default (5m)" },
  "sched.advancedHelp": {
    ja: "5 フィールドの cron (分 時 日 月 曜日)。空 = 無効。Enter かフォーカスを外すと適用。",
    en: "5-field cron (minute hour day-of-month month day-of-week). Empty = off. Press Enter or tab away to apply.",
  },
  "sched.advancedInvalid": {
    ja: "構文が不正 — 空白区切りの 5 フィールドが必要。",
    en: "Invalid syntax — must be 5 whitespace-separated fields.",
  },

  // ── AgentCreate ──
  "create.newAgent": { ja: "新規エージェント", en: "New Agent" },
  "create.personaPromptRequired": {
    ja: "人格を生成するプロンプトを入力して",
    en: "Enter a prompt to generate persona",
  },
  "create.personaRequired": {
    ja: "先に人格の説明を書いて",
    en: "Write a persona description first",
  },
  "create.generatingPersona": { ja: "人格を生成中...", en: "Generating persona..." },
  "create.generatingName": { ja: "名前を生成中...", en: "Generating name..." },
  "create.generatingAvatar": { ja: "アバターを生成中...", en: "Generating avatar..." },
  "create.personaPlaceholder": {
    ja: "エージェントの性格・話し方・興味などを書いて...",
    en: "Describe the agent's personality, speaking style, interests...",
  },
  "create.personaPromptPlaceholder": {
    ja: "例: ツンデレな女の子にして",
    en: "e.g. make it a tsundere girl",
  },
  "create.uploadAvatarTitle": {
    ja: "クリックしてアバターをアップロード",
    en: "Click to upload avatar",
  },
  "create.avatarAlt": { ja: "アバター", en: "Avatar" },
  "create.regenAvatarTitle": { ja: "アバターを再生成", en: "Regenerate avatar" },
  "create.genNameTitle": { ja: "人格から名前を生成", en: "Generate name from persona" },
  "create.genHintAria": { ja: "生成ヒント", en: "Generation hint" },
  "create.genHintPlaceholder": {
    ja: "生成ヒント (任意)",
    en: "Generation hint (optional)",
  },
  "create.nameAndAvatar": { ja: "名前とアバター", en: "Name & Avatar" },
  "create.setNameFirst": { ja: "先に名前を設定して", en: "Set a name first" },
  "create.genAvatarOnly": { ja: "アバターだけ生成", en: "Generate avatar only" },
  "create.avatarProgress": { ja: "アバター...", en: "Avatar..." },
  "create.avatar": { ja: "アバター", en: "Avatar" },
  "create.apiBaseUrl": { ja: "API Base URL", en: "API Base URL" },
  "create.createAgent": { ja: "エージェントを作成", en: "Create agent" },

  // ── AgentCredentials ──
  "cred.importTotp": { ja: "TOTP をインポート", en: "Import TOTP" },
  "cred.qrImage": { ja: "QR 画像", en: "QR Image" },
  "cred.uriText": { ja: "URI テキスト", en: "URI Text" },
  "cred.decoding": { ja: "解析中...", en: "Decoding..." },
  "cred.tapSelectQr": { ja: "タップして QR 画像を選択", en: "Tap to select QR image" },
  "cred.parsing": { ja: "パース中...", en: "Parsing..." },
  "cred.parse": { ja: "パース", en: "Parse" },
  "cred.entriesFound": {
    ja: "{count} 件見つかった — 1 つ選んで",
    en: "{count} entries found — select one",
  },
  "cred.unknown": { ja: "不明", en: "Unknown" },
  "cred.label": { ja: "ラベル", en: "Label" },
  "cred.labelExample": { ja: "ラベル (例: GitHub)", en: "Label (e.g. GitHub)" },
  "cred.username": { ja: "ユーザー名 / ID", en: "Username / ID" },
  "cred.password": { ja: "パスワード", en: "Password" },
  "cred.newPasswordKeep": {
    ja: "新しいパスワード (空なら現状維持)",
    en: "New password (leave empty to keep)",
  },
  "cred.newTotpKeep": {
    ja: "新しい TOTP シークレット (空なら現状維持)",
    en: "New TOTP secret (leave empty to keep)",
  },
  "cred.totpOptional": { ja: "TOTP シークレット (任意)", en: "TOTP Secret (optional)" },
  "cred.switchingNoSave": {
    ja: "デバイス転移中。完了するまで保存できない。",
    en: "Device transfer in progress. Cannot save until it finishes.",
  },
  "cred.switchingNoAdd": {
    ja: "デバイス転移中。完了するまで追加できない。",
    en: "Device transfer in progress. Cannot add until it finishes.",
  },
  "cred.switchingNoEdit": {
    ja: "デバイス転移中。完了するまで編集できない。",
    en: "Device transfer in progress. Cannot edit until it finishes.",
  },
  "cred.switchingNoDelete": {
    ja: "デバイス転移中。完了するまで削除できない。",
    en: "Device transfer in progress. Cannot delete until it finishes.",
  },
  "cred.switchingBanner": {
    ja: "デバイス転移中。完了するまで認証情報は編集できない。",
    en: "Device transfer in progress. Credentials cannot be edited until it finishes.",
  },
  "cred.agentFetchFailed": {
    ja: "エージェントレコードの取得に失敗: {msg}",
    en: "agent record fetch failed: {msg}",
  },
  "cred.deleteConfirm": { ja: "この認証情報を削除する?", en: "Delete this credential?" },
  "cred.addButton": { ja: "+ 追加", en: "+ Add" },
  "cred.retry": { ja: "再試行", en: "Retry" },
  "cred.adding": { ja: "追加中...", en: "Adding..." },
  "cred.add": { ja: "追加", en: "Add" },
  "cred.copyPw": { ja: "PW をコピー", en: "Copy PW" },
  "cred.none": { ja: "登録済みの認証情報はない", en: "No credentials registered" },

  // ── Agent settings fields ──
  "field.persona": { ja: "人格", en: "Persona" },
  "field.personaGenPrompt": { ja: "人格生成プロンプト", en: "Persona generation prompt" },
  "field.effort": { ja: "Effort", en: "Effort" },
  "field.effortDefault": { ja: "既定 ({level})", en: "default ({level})" },
  "field.tool": { ja: "ツール", en: "Tool" },
  "field.modelName": { ja: "モデル名", en: "model name" },
  "field.fileStorage": { ja: "ファイル保存先", en: "File Storage" },
  "field.fileStorageHelp": {
    ja: "生成されたファイルはここに保存される。",
    en: "Generated files are saved here.",
  },
  "field.workDirPlaceholder": {
    ja: "(既定: エージェントのデータディレクトリ)",
    en: "(default: agent data dir)",
  },
  "field.addEntry": { ja: "+ 項目を追加", en: "+ Add entry" },
  "field.statusKey": { ja: "キー", en: "key" },
  "field.statusValue": { ja: "値", en: "value" },
  "field.statusKeyAria": { ja: "ステータスのキー {n}", en: "status key {n}" },
  "field.statusValueAria": { ja: "ステータスの値 {n}", en: "status value {n}" },
  "field.statusRemoveAria": { ja: "ステータス行 {n} を削除", en: "remove status row {n}" },

  // ── GlobalSettings sections ──
  "gs.apiKeys": { ja: "API キー", en: "API Keys" },
  "gs.apiKeysDesc": {
    ja: "API キーの暗号化ストレージ。埋め込み・画像生成・音声入力に使う。",
    en: "Encrypted storage for API keys. Used for embedding, image generation, and voice input.",
  },
  "gs.configured": { ja: "設定済み", en: "Configured" },
  "gs.usingFallback": { ja: "フォールバックを使用中", en: "Using fallback" },
  "gs.notConfigured": { ja: "未設定", en: "Not configured" },
  "gs.update": { ja: "更新", en: "Update" },
  "gs.configure": { ja: "設定する", en: "Configure" },
  "gs.removeGeminiKey": { ja: "Gemini API キーを削除", en: "Remove Gemini API key" },
  "gs.removeXaiKey": { ja: "xAI API キーを削除", en: "Remove xAI API key" },
  "gs.save": { ja: "保存", en: "Save" },
  "gs.embeddingModel": { ja: "埋め込みモデル", en: "Embedding Model" },
  "gs.loadingModels": { ja: "モデルを読み込み中...", en: "Loading models..." },
  "gs.modelUnavailable": { ja: "{model} (利用不可)", en: "{model} (unavailable)" },
  "gs.loadModelsFailed": { ja: "モデル一覧の取得に失敗", en: "Failed to load models" },
  "gs.configureKeyForModels": {
    ja: "API キーを設定すると利用可能なモデルが出る",
    en: "Configure API key to see available models",
  },
  "gs.voiceInputStt": { ja: "音声入力 (音声認識)", en: "Voice input (speech-to-text)" },
  "gs.archivedAgents": { ja: "アーカイブ済みエージェント", en: "Archived Agents" },
  "gs.archivedAgentsDesc": {
    ja: "アーカイブ済みエージェントはメインリストから隠れ、ランタイム活動もない。エージェント自身のデータ (1:1 チャット履歴・メモリ・人格・認証情報・通知トークン) は保持される。グループ DM のメンバーシップは保持されない — アーカイブ時に全グループから外れ (2人グループは解散してトランスクリプトも削除)、アーカイブ解除でも復帰しない。削除はすべてを完全に消す。",
    en: "Archived agents are hidden from the main list and have no runtime activity. The agent's own data (1:1 chat history, memory, persona, credentials, notify tokens) is preserved. Group DM memberships are not — the agent was removed from every group on archive (2-person groups were dissolved and their transcripts deleted), and memberships are NOT restored on unarchive. Delete wipes everything permanently.",
  },
  "gs.deleteArchivedConfirm": {
    ja: "「{name}」とそのデータをすべて完全に削除する? 取り消せない。",
    en: 'Permanently delete "{name}" and all of its data? This cannot be undone.',
  },
  "gs.loading": { ja: "読み込み中...", en: "Loading..." },
  "gs.noArchivedAgents": { ja: "アーカイブ済みエージェントはない", en: "No archived agents" },
  "gs.archivedOn": { ja: "{date} にアーカイブ", en: "archived {date}" },
  "gs.restore": { ja: "復元", en: "Restore" },
  "gs.chat": { ja: "チャット", en: "Chat" },
  "gs.sendWithEnter": { ja: "Enter で送信", en: "Send with Enter" },
  "gs.enterSendsHelp": {
    ja: "Enter で送信、Shift+Enter で改行",
    en: "Enter to send, Shift+Enter for newline",
  },
  "gs.ctrlEnterSendsHelp": {
    ja: "Ctrl+Enter で送信、Enter で改行",
    en: "Ctrl+Enter to send, Enter for newline",
  },
  "gs.system": { ja: "システム", en: "System" },
  "gs.systemDesc": {
    ja: "サーバーをソースから再ビルドするか、稼働中のデーモンを再起動する。再起動は実行中のエージェントターンの完了を待ってから re-exec するので、少し時間がかかることがある。",
    en: "Rebuild the server from source or restart the running daemon. Restart drains in-flight agent turns before re-execing, so it may take a moment.",
  },
  "gs.restarting": { ja: "再起動中...", en: "Restarting..." },
  "gs.restartRequested": {
    ja: "再起動を要求した。サーバーは re-exec 中 — 少し待ってから再読み込みして。",
    en: "Restart requested. The server is re-execing; reload in a moment.",
  },
  "gs.restartConfirm": {
    ja: "今すぐサーバーを再起動する? 実行中のエージェントターンは先に完了を待つ。",
    en: "Restart the server now? In-flight agent turns will drain first.",
  },
  "gs.rebuildConfirm": {
    ja: "サーバーを再ビルド (`make build`) して再起動する? 数分かかることがある。",
    en: "Rebuild the server (`make build`) and restart? This can take several minutes.",
  },
  "gs.building": { ja: "ビルド中... (数分かかることがある)", en: "Building... (this can take several minutes)" },
  "gs.rebuilding": { ja: "再ビルド中...", en: "Rebuilding..." },
  "gs.rebuildRestart": { ja: "再ビルドして再起動", en: "Rebuild & Restart" },
  "gs.restart": { ja: "再起動", en: "Restart" },

  // ── PeersSection ──
  "peers.title": { ja: "ピア", en: "Peers" },
  "peers.desc": {
    ja: "既知のクラスタメンバー。ピアの NodeKey バインディング (特権サーフェスへの入場資格) は join-request で取り込まれる。deviceId の行がすでにあればその場で追記され、未登録のリクエストはオペレーターが承認するまで下の保留パネルで待つ。手動 Register は行を先に作るだけで NodeKey は取り込まない。この端末自身はこの UI から追加・削除できない。",
    en: "Known cluster members. A peer's NodeKey binding (what admits it on the privileged surface) is captured by its join-request: a request whose deviceId already has a row here is back-filled in place; an unregistered request waits in the Pending panel below until the operator Approves it. Manual Register pre-seeds a row but never captures a NodeKey on its own. The local device cannot be added or removed from this UI.",
  },
  "peers.unavailable": {
    ja: "このサーバーではピアレジストリを使えない。ローカルのピアアイデンティティがまだ初期化されていないか、なしで起動された。",
    en: "Peer registry is not available on this server. The local peer identity has not been bootstrapped yet, or the server was started without one.",
  },
  "peers.never": { ja: "未確認", en: "never" },
  "peers.justNow": { ja: "たった今", en: "just now" },
  "peers.minAgo": { ja: "{n} 分前", en: "{n}m ago" },
  "peers.hourAgo": { ja: "{n} 時間前", en: "{n}h ago" },
  "peers.loadPendingFailed": {
    ja: "保留中ピアの取得に失敗: {msg}",
    en: "Failed to load pending peers: {msg}",
  },
  "peers.loadFailed": { ja: "ピアの取得に失敗: {msg}", en: "Failed to load peers: {msg}" },
  "peers.registerFailed": { ja: "登録に失敗: {msg}", en: "Register failed: {msg}" },
  "peers.editBothRequired": {
    ja: "編集: name と url を両方入力する必要がある",
    en: "Edit: both name and url are required",
  },
  "peers.editFailed": { ja: "編集に失敗: {msg}", en: "Edit failed: {msg}" },
  "peers.approveFailed": { ja: "承認に失敗: {msg}", en: "Approve failed: {msg}" },
  "peers.rejectConfirm": {
    ja: "「{name}」からの参加リクエストを却下する?",
    en: 'Reject join request from "{name}"?',
  },
  "peers.rejectFailed": { ja: "却下に失敗: {msg}", en: "Reject failed: {msg}" },
  "peers.removeConfirm": {
    ja: "ピア「{name}」を退役させる? 取り消せない。",
    en: 'Decommission peer "{name}"? This cannot be undone.',
  },
  "peers.deleteFailed": { ja: "削除に失敗: {msg}", en: "Delete failed: {msg}" },
  "peers.register": { ja: "登録", en: "Register" },
  "peers.pairingHelpPre": {
    ja: "相手ピアが起動時に表示するペアリングスペックを貼り付けて",
    en: "Paste the pairing spec the other peer prints on startup",
  },
  "peers.pairingHelpArg": { ja: " の引数", en: " argument" },
  "peers.pairingHelpFormat": { ja: "形式:", en: "Format:" },
  "peers.pairingHelpPost": {
    ja: "メタデータのみ — NodeKey バインディングは後でピアが join-request を送ったときに取り込まれる (接触時にこの行へ追記)。",
    en: "Metadata only — the NodeKey binding is captured later when the peer sends a join-request (back-filled into this row on contact).",
  },
  "peers.parsePrefix": { ja: "パース: ", en: "Parse: " },
  "peers.registering": { ja: "登録中...", en: "Registering..." },
  "peers.registerPeer": { ja: "ピアを登録", en: "Register peer" },
  "peers.pendingTitle": { ja: "保留中の参加リクエスト", en: "Pending join requests" },
  "peers.pendingHelpPre": {
    ja: "この Hub を ",
    en: "Peers that auto-discovered this Hub via ",
  },
  "peers.pendingHelpPost": {
    ja: " で自動発見して承認を待っているピア。承認すると特権サーフェスに入場できる。却下するとリクエストは破棄される — ピアは再試行できる。",
    en: " and are waiting for approval. Approve admits the peer to the privileged surface; Reject drops the request — the peer may retry.",
  },
  "peers.seen": { ja: "最終確認 {when}", en: "seen {when}" },
  "peers.approve": { ja: "承認", en: "Approve" },
  "peers.reject": { ja: "却下", en: "Reject" },
  "peers.none": { ja: "登録済みのピアはない。", en: "No peers registered." },
  "peers.thisDevice": { ja: "この端末", en: "this device" },
  "peers.editTitle": {
    ja: "このピアの表示名とダイヤル URL を編集",
    en: "Edit this peer's display name and dial URL",
  },
  "peers.edit": { ja: "編集", en: "Edit" },
  "peers.removeTitle": {
    ja: "このピアをレジストリから削除",
    en: "Remove this peer from the registry",
  },
  "peers.parseFieldCount": {
    ja: "パイプ区切りのフィールドは 3 個必要 — {count} 個だった",
    en: "expected 3 pipe-separated fields, got {count}",
  },
  "peers.parseFieldEmpty": {
    ja: "すべてのフィールド (deviceId | name | url) を埋める必要がある",
    en: "every field (deviceId | name | url) must be non-empty",
  },
  "peers.displayNameHelp": {
    ja: "表示名 (自由なラベル。エージェントはこの名前でピアを参照する):",
    en: "Display name (free-form label; agents reference this peer by name):",
  },
  "peers.dialUrlHelp": {
    ja: "ダイヤル URL (host:port または http(s)://host:port):",
    en: "Dial URL (host:port or http(s)://host:port):",
  },

  // ── App shell / file browser / misc ──
  "app.selectPane": {
    ja: "一覧からエージェントやセッションを選択",
    en: "Select an agent or session",
  },
  "fb.workdir": { ja: "作業ディレクトリ", en: "Workdir" },
  "fb.files": { ja: "ファイル", en: "Files" },
  "fb.localFs": { ja: "ローカルファイルシステム", en: "Local filesystem" },

  // ── AttachmentsTab ──
  "att.sortModified": { ja: "更新", en: "Modified" },
  "att.sortCreated": { ja: "作成", en: "Created" },
  "att.sortName": { ja: "名前", en: "Name" },
  "att.sortSize": { ja: "サイズ", en: "Size" },
  "att.pathCopied": { ja: "パスをコピーした", en: "Path copied" },
  "att.failed": { ja: "失敗", en: "Failed" },
  "att.none": { ja: "添付は見つからない", en: "No attachments detected" },
  "att.path": { ja: "パス", en: "Path" },
  "att.confirmDel": { ja: "OK?", en: "OK?" },
  "att.del": { ja: "削除", en: "Del" },

  // ── GitPanel ──
  "git.workingTree": { ja: "作業ツリー", en: "working tree" },
  "git.dangerConfirm": {
    ja: "破壊的な操作: git {cmd}。続ける?",
    en: "Destructive operation: git {cmd}. Continue?",
  },
  "git.refresh": { ja: "更新", en: "Refresh" },
  "git.tabStatus": { ja: "ステータス", en: "Status" },
  "git.tabLog": { ja: "ログ", en: "Log" },
  "git.tabDiff": { ja: "差分", en: "Diff" },
  "git.staged": { ja: "ステージ済み", en: "Staged" },
  "git.modified": { ja: "変更あり", en: "Modified" },
  "git.untracked": { ja: "未追跡", en: "Untracked" },
  "git.cleanTree": { ja: "作業ツリーはクリーン", en: "Clean working tree" },
  "git.noCommits": { ja: "コミットがない", en: "No commits" },
  "git.loading": { ja: "読み込み中…", en: "Loading…" },
  "git.loadMore": { ja: "さらに読み込む", en: "Load more" },
  "git.noDiff": { ja: "差分がない", en: "No diff" },
  "git.commandPlaceholder": { ja: "コマンド…", en: "command…" },
  "git.run": { ja: "実行", en: "Run" },

  // ── NewSession ──
  "ns.title": { ja: "新規セッション", en: "New Session" },
  "ns.peerInfoUnavailable": {
    ja: "ピア情報を取得できない。ピアはオンラインで、このホストとペアリング済み?",
    en: "Peer info unavailable. Is the peer online and paired with this host?",
  },
  "ns.host": { ja: "ホスト", en: "Host" },
  "ns.offline": { ja: "オフライン", en: "offline" },
  "ns.notAvailable": { ja: "(利用不可)", en: "(not available)" },
  "ns.defaultOption": { ja: "(既定)", en: "(default)" },
  "ns.workingDirectory": { ja: "作業ディレクトリ", en: "Working directory" },
  "ns.additionalArgs": { ja: "追加引数", en: "Additional arguments" },
  "ns.yoloMode": { ja: "Yolo モード", en: "Yolo Mode" },
  "ns.yoloClaude": {
    ja: "--dangerously-skip-permissions 付きで起動する",
    en: "Launches with --dangerously-skip-permissions",
  },
  "ns.yoloCodex": {
    ja: "--dangerously-bypass-approvals-and-sandbox 付きで起動する",
    en: "Launches with --dangerously-bypass-approvals-and-sandbox",
  },
  "ns.yoloOther": { ja: "権限確認をスキップする", en: "Skip permission prompts" },
  "ns.minimalPrompt": { ja: "最小システムプロンプト", en: "Minimal system prompt" },
  "ns.minimalPromptHelp": {
    ja: "既定のプロンプトを作業ディレクトリの注記だけに置き換える",
    en: "Replace the default prompt with just a working-directory note",
  },
  "ns.createSession": { ja: "セッションを作成", en: "Create session" },

  // ── FileDataBrowser ──
  "fdb.copyFolderTitle": {
    ja: "現在のフォルダの絶対パスをコピー",
    en: "Copy absolute path of current folder",
  },
  "fdb.copiedLower": { ja: "コピーした", en: "copied" },
  "fdb.copyPath": { ja: "パスをコピー", en: "copy path" },
  "fdb.filterPlaceholder": { ja: "絞り込み…", en: "Filter…" },
  "fdb.sortName": { ja: "名前", en: "Name" },
  "fdb.sortSize": { ja: "サイズ", en: "Size" },
  "fdb.sortMod": { ja: "更新", en: "Mod" },
  "fdb.toggleHidden": { ja: "隠しファイルの表示を切り替え", en: "Toggle hidden files" },
  "fdb.loading": { ja: "読み込み中…", en: "Loading…" },
  "fdb.noMatches": { ja: "一致なし。", en: "No matches." },
  "fdb.emptyFolder": { ja: "空のフォルダ。", en: "Empty folder." },
  "fdb.renderMd": { ja: "Markdown をレンダリング", en: "Render markdown" },
  "fdb.showRawSource": { ja: "ソースを表示", en: "Show raw source" },
  "fdb.downloadLower": { ja: "ダウンロード", en: "download" },
  "fdb.downloadInstead": { ja: "代わりにダウンロード", en: "Download instead" },

  // ── SessionPage ──
  "sess.tabCli": { ja: "CLI", en: "CLI" },
  "sess.tabTerminal": { ja: "ターミナル", en: "Terminal" },
  "sess.tabFiles": { ja: "ファイル", en: "Files" },
  "sess.tabGit": { ja: "Git", en: "Git" },
  "sess.tabAttach": { ja: "添付", en: "Attach" },
  "sess.stopSession": { ja: "セッションを停止", en: "Stop session" },
  "sess.reconnecting": { ja: "再接続中…", en: "Reconnecting…" },
  "sess.yoloTail": { ja: "yolo tail", en: "yolo tail" },
  "sess.tapToCopy": { ja: "タップしてコピー", en: "tap to copy" },
  "sess.attachFile": { ja: "ファイルを添付", en: "Attach file" },
  "sess.resume": { ja: "再開", en: "Resume" },

  // ── TerminalTab ──
  "term.tmuxMissing": {
    ja: "tmux が入っていない。\nインストール: brew install tmux",
    en: "tmux is not installed.\nInstall: brew install tmux",
  },
  "term.toolUnavailable": { ja: "{tool} は利用できない。", en: "{tool} is not available." },
  "term.tmuxNewWin": { ja: "+ウィンドウ", en: "+Win" },
  "term.tmuxPrevWin": { ja: "←ウィンドウ", en: "←Win" },
  "term.tmuxNextWin": { ja: "ウィンドウ→", en: "Win→" },
  "term.tmuxSplitH": { ja: "─", en: "─" },
  "term.tmuxSplitV": { ja: "│", en: "│" },
  "term.tmuxPane": { ja: "ペイン", en: "Pane" },
  "term.tmuxZoom": { ja: "ズーム", en: "Zoom" },
  "term.tmuxList": { ja: "一覧", en: "List" },
  "term.tmuxCopy": { ja: "コピー", en: "Copy" },
  "term.tmuxKill": { ja: "終了", en: "Kill" },

  // ── Relative time (timeAgo) ──
  "time.justNow": { ja: "たった今", en: "just now" },
  "time.minAgo": { ja: "{n}分前", en: "{n}m ago" },
  "time.hourAgo": { ja: "{n}時間前", en: "{n}h ago" },
  "time.dayAgo": { ja: "{n}日前", en: "{n}d ago" },

  // ── GroupDMChat ──
  "gdm.notFound": { ja: "グループが見つからない", en: "Group not found" },
  "gdm.groupName": { ja: "グループ名", en: "Group name" },
  "gdm.clickToRename": { ja: "クリックして名前を変更", en: "Click to rename" },
  "gdm.styleTitle": { ja: "スタイル: {style}", en: "Style: {style}" },
  "gdm.styleEfficient": { ja: "効率重視", en: "Efficient" },
  "gdm.styleExpressive": { ja: "表現重視", en: "Expressive" },
  "gdm.venueTitle": { ja: "場: {venue}", en: "Venue: {venue}" },
  "gdm.venueChatroom": { ja: "クローズドなチャットルーム", en: "Closed chat room" },
  "gdm.venueChatroomHint": {
    ja: "テキストのみ、同じ場所にはいない",
    en: "Text-only, not co-present",
  },
  "gdm.venueColocated": { ja: "同じ物理空間", en: "Same physical space" },
  "gdm.venueColocatedHint": {
    ja: "メンバーが実空間で同席している",
    en: "Members are co-present in real space",
  },
  "gdm.cooldownTitle": { ja: "通知クールダウン (秒)", en: "Notification cooldown (seconds)" },
  "gdm.maxHops": { ja: "最大ホップ数", en: "Max hops" },
  "gdm.hopsUnit": { ja: "ホップ", en: "hops" },
  "gdm.maxHopsTitle": {
    ja: "リレーの最大ホップ数 (空 = 既定 4、最大 20)",
    en: "Max relay hops (empty = default 4, max 20)",
  },
  "gdm.clearHistoryTitle": { ja: "メッセージ履歴を消去", en: "Clear message history" },
  "gdm.archiveThread": { ja: "スレッドをアーカイブ", en: "Archive thread" },
  "gdm.deleteGroup": { ja: "グループを削除", en: "Delete group" },
  "gdm.replying": { ja: "返信中…", en: "replying…" },
  "gdm.compacting": { ja: "整理中…", en: "compacting…" },
  "gdm.interrupted": { ja: "中断", en: "interrupted" },
  "gdm.interruptedTitle": {
    ja: "この返信は途中で中断された (部分的な出力)",
    en: "This reply was interrupted (partial output)",
  },
  "gdm.steerPlaceholder": {
    ja: "実行中の返信に割り込む… ({key} で送信)",
    en: "Steer the running reply… ({key} to send)",
  },
  "gdm.threadPlaceholder": {
    ja: "このスレッドにメッセージ… ({key} で送信)",
    en: "Message this thread… ({key} to send)",
  },
  "gdm.groupPlaceholder": {
    ja: "グループにメッセージ… ({key} で送信)",
    en: "Message the group… ({key} to send)",
  },
  "gdm.steerFailed": { ja: "割り込みに失敗", en: "Failed to steer" },
  "gdm.sendFailed": { ja: "送信に失敗", en: "Failed to send" },
  "gdm.clearConfirmTitle": { ja: "履歴を消去する?", en: "Clear history?" },
  "gdm.clearConfirmBody": {
    ja: "「{name}」のメッセージを削除する。グループ自体は残る。",
    en: "Messages in \u201c{name}\u201d will be deleted. The group stays open.",
  },
  "gdm.clearFailed": { ja: "履歴の消去に失敗", en: "Failed to clear history" },
  "gdm.clearing": { ja: "消去中…", en: "Clearing…" },
  "gdm.clear": { ja: "消去", en: "Clear" },
  "gdm.deleteConfirmTitle": { ja: "「{name}」を削除する?", en: "Delete \u201c{name}\u201d?" },
  "gdm.deleteFailed": { ja: "削除に失敗", en: "Failed to delete" },
  "gdm.deleting": { ja: "削除中…", en: "Deleting…" },
  "gdm.delete": { ja: "削除", en: "Delete" },
  "gdm.archiveConfirmTitle": { ja: "「{name}」をアーカイブする?", en: "Archive \u201c{name}\u201d?" },
  "gdm.archiveConfirmBody": {
    ja: "スレッドを完全に閉じる。復元はできない。",
    en: "This permanently closes the thread. It cannot be restored.",
  },
  "gdm.archiveFailed": { ja: "アーカイブに失敗", en: "Failed to archive" },
  "gdm.archiving": { ja: "アーカイブ中…", en: "Archiving…" },
  "gdm.archive": { ja: "アーカイブ", en: "Archive" },
} satisfies Record<string, Entry>;

export type MessageKey = keyof typeof messages;

/**
 * Translate a key for the current locale, interpolating {name}-style params.
 * Missing key → returns the key itself (fail-soft, warns in dev).
 */
export function t(key: MessageKey, params?: Record<string, string | number>): string {
  const entry = messages[key];
  if (!entry) {
    // import.meta.env is Vite-injected; cast since vite/client types aren't
    // pulled into this tsconfig.
    if ((import.meta as { env?: { DEV?: boolean } }).env?.DEV) {
      console.warn(`[i18n] missing key: ${key}`);
    }
    return key;
  }
  const out = entry[current];
  if (!params) return out;
  // Single pass with a function replacer so param values containing `$`
  // sequences ($&, $1, …) are inserted verbatim rather than interpreted as
  // replacement patterns.
  return out.replace(/\{(\w+)\}/g, (match, name: string) =>
    name in params ? String(params[name]) : match,
  );
}
