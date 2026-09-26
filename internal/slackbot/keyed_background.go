package slackbot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loppo-llc/kojo/internal/agent"
)

// keyedBackgroundRegistrar is the optional ChatManager capability that routes
// unsolicited turns of a lingering keyed claude session (run_in_background
// completion notifications) back to this bot.
type keyedBackgroundRegistrar interface {
	RegisterKeyedBackgroundHandler(agentID string, h agent.KeyedBackgroundHandler) func()
}

// backgroundPendingNote is appended to a final reply whose session keeps
// lingering for still-running background tasks.
func backgroundPendingNote(n int) string {
	return fmt.Sprintf("_バックグラウンド処理 %d件 実行中。完了したらこのスレッドで続きを投稿します_", n)
}

// stoppedBackgroundNote tells the thread that a stopped turn's background
// tasks keep running: a stop ends only the turn (like the WebUI), and their
// results still arrive in the thread. `!stop all` stops them too.
func stoppedBackgroundNote(n int) string {
	return fmt.Sprintf("_バックグラウンド処理 %d件 は継続中です。完了したらこのスレッドで続きを投稿します（止めるには `!stop all`）_", n)
}

const (
	stopAllNothing     = "_このスレッドに停止するターンやバックグラウンド処理はありません_"
	stopAllUnsupported = "_このエージェントではバックグラウンド処理を停止できません_"
)

// threadBackgroundStopper is the optional ChatManager capability behind
// `!stop all` (the router follows the thread's holder, local or peer).
type threadBackgroundStopper interface {
	StopThreadBackgroundTasks(ctx context.Context, agentID, sessionKey string) error
}

// stopThreadBackgroundTasks stops every background task of the thread's keyed
// session. Success is reported by the abandoned notice (reason
// agent.KeyedUserStopReason) through the thread FIFO; only "nothing to stop"
// (when no turn was stopped either) and failures are answered here.
func (b *Bot) stopThreadBackgroundTasks(ctx context.Context, channel, threadTS string, turnStopped bool) {
	stopper, ok := b.mgr.(threadBackgroundStopper)
	if !ok {
		if !turnStopped {
			b.postCommandNotice(ctx, channel, threadTS, stopAllUnsupported)
		}
		return
	}
	key := slackSessionKey(b.agentID, channel, threadTS)
	b.beginStopAll(key)
	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := stopper.StopThreadBackgroundTasks(stopCtx, b.agentID, key)
	cancel()
	// Only "nothing to stop" is a definite no-op: any other failure (e.g. a
	// relayed stop whose response was lost) may still have stopped the
	// tasks, so a repeat within the window must not claim there was nothing.
	coalesced := b.endStopAll(key, !errors.Is(err, agent.ErrBackgroundSessionNotFound))
	switch {
	case err == nil:
	case errors.Is(err, agent.ErrBackgroundSessionNotFound):
		if !turnStopped && !coalesced {
			b.postCommandNotice(ctx, channel, threadTS, stopAllNothing)
		}
	default:
		b.logger.Warn("slack !stop all: stopping background tasks failed", "channel", channel, "threadTS", threadTS, "err", err)
		b.postCommandNotice(ctx, channel, threadTS, "_バックグラウンド処理を停止できませんでした: "+err.Error()+"_")
	}
}

// stopAllCoalesceWindow is how long after a (possibly) successful `!stop all`
// a repeat that finds nothing stays silent (the first stop's notice answers
// it). Expired entries are pruned on every stop, so stopAllDone only holds the
// stops of the last window before the latest one.
const stopAllCoalesceWindow = 30 * time.Second

func (b *Bot) beginStopAll(key string) {
	b.stopAllMu.Lock()
	defer b.stopAllMu.Unlock()
	if b.stopAllInflight == nil {
		b.stopAllInflight = make(map[string]int)
	}
	b.stopAllInflight[key]++
}

// endStopAll records the outcome (ok: the stop may have stopped something)
// and reports whether another `!stop all` for the same thread is in flight or
// recently (possibly) succeeded.
func (b *Bot) endStopAll(key string, ok bool) bool {
	b.stopAllMu.Lock()
	defer b.stopAllMu.Unlock()
	now := time.Now()
	if n := b.stopAllInflight[key] - 1; n > 0 {
		b.stopAllInflight[key] = n
	} else {
		delete(b.stopAllInflight, key)
	}
	for k, at := range b.stopAllDone {
		if now.Sub(at) >= stopAllCoalesceWindow {
			delete(b.stopAllDone, k)
		}
	}
	coalesced := b.stopAllInflight[key] > 0
	if _, recent := b.stopAllDone[key]; recent {
		coalesced = true
	}
	if ok {
		if b.stopAllDone == nil {
			b.stopAllDone = make(map[string]time.Time)
		}
		b.stopAllDone[key] = now
	}
	return coalesced
}

// registerKeyedBackground installs the bot as the agent's keyed background
// handler when the manager supports it. The returned func unregisters.
func (b *Bot) registerKeyedBackground() func() {
	r, ok := b.mgr.(keyedBackgroundRegistrar)
	if !ok {
		return func() {}
	}
	return r.RegisterKeyedBackgroundHandler(b.agentID, b)
}

// parseSlackSessionKey reverses slackSessionKey for this bot's agent.
func (b *Bot) parseSlackSessionKey(agentID, sessionKey string) (channel, threadTS string, ok bool) {
	if agentID != b.agentID {
		return "", "", false
	}
	suffix, ok := strings.CutPrefix(sessionKey, b.agentID+":slack:")
	if !ok {
		return "", "", false
	}
	channel, threadTS, ok = strings.Cut(suffix, ":")
	if !ok || channel == "" || threadTS == "" {
		return "", "", false
	}
	return channel, threadTS, true
}

// HandleKeyedBackgroundTurn posts an unsolicited background turn into its
// Slack thread. It opens a synthetic admission on the same thread FIFO as
// user turns (precedent: resumeGoal) so posts never interleave, registers an
// active turn so !stop works, and reuses the regular delivery pipeline.
func (b *Bot) HandleKeyedBackgroundTurn(agentID, sessionKey string, events <-chan agent.ChatEvent, cancel func()) {
	defer func() {
		for range events {
		}
	}()
	channel, threadTS, ok := b.parseSlackSessionKey(agentID, sessionKey)
	if !ok || b.ctx.Err() != nil {
		if cancel != nil {
			cancel()
		}
		return
	}
	reservation := b.reserveThread(channel, threadTS)
	turnCtx, turnCancel := context.WithCancel(b.ctx)
	active := b.registerBackgroundActiveTurn(channel, threadTS, func() {
		if cancel != nil {
			cancel()
		}
		turnCancel()
	})
	defer func() {
		b.finishActiveTurn(channel, threadTS, active)
		turnCancel()
		b.finishStopTransaction(channel, threadTS, active)
		b.releaseThreadReservation(channel, threadTS, reservation)
	}()
	reservation.Wait()
	if turnCtx.Err() != nil {
		if cancel != nil {
			cancel()
		}
		return
	}
	b.deliverAgentTurn(turnCtx, slackTurnDelivery{
		channel:    channel,
		threadTS:   threadTS,
		sessionKey: sessionKey,
		turnCtx:    turnCtx,
		turnCancel: turnCancel,
		active:     active,
		userTurn:   false,
	}, events)
}

// KeyedBackgroundTasksAbandoned posts a best-effort notice when a lingering
// thread session was closed while background tasks were still running.
func (b *Bot) KeyedBackgroundTasksAbandoned(agentID, sessionKey string, pending int, reason string) {
	channel, threadTS, ok := b.parseSlackSessionKey(agentID, sessionKey)
	if !ok || pending <= 0 {
		return
	}
	msg := keyedAbandonedNotice(pending, reason)
	// Queue behind any in-flight reply on the thread so the notice never
	// interleaves with streamed or chunked output.
	reservation := b.reserveThread(channel, threadTS)
	defer b.releaseThreadReservation(channel, threadTS, reservation)
	reservation.Wait()
	ctx, c := context.WithTimeout(context.Background(), chunkPostTimeoutBase)
	defer c()
	b.postMessage(ctx, channel, threadTS, msg)
}

// keyedAbandonedNotice words the thread notice for background tasks that
// ended before completing: an explicit stop reads as such (same wording as
// WebUI threads), anything else as an unexpected end with its reason.
func keyedAbandonedNotice(pending int, reason string) string {
	switch reason {
	case agent.KeyedStopRequestedReason:
		return fmt.Sprintf("_バックグラウンド処理 %d件 を停止しました（エージェントの依頼）_", pending)
	case agent.KeyedUserStopReason:
		return fmt.Sprintf("_バックグラウンド処理 %d件 を停止しました（ユーザーの依頼）_", pending)
	}
	msg := fmt.Sprintf("_バックグラウンド処理 %d件 が完了前に終了しました", pending)
	if reason != "" {
		msg += "（" + reason + "）"
	}
	return msg + "。必要なら改めて依頼してください_"
}
