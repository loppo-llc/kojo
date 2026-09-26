package agent

import (
	"encoding/json"
	"log/slog"
)

// Native app-server owns continuation: goal/set active starts idle work itself.
func runCodexGoal(scanner *jsonlLineScanner, q *GoalRequest, r *codexGoalRuntime, steer *codexSteerer, respond codexServerRequestResponder, logger *slog.Logger, send func(ChatEvent) bool, questions ...*codexQuestionState) *codexStreamResult {
	return runCodexGoalWithReply(scanner, q, r, steer, respond, logger, send, nil, questions...)
}

// A reply is accepted as a user turn while the stored goal is paused, before
// reactivation. Do not resume autonomous work if the input is rejected.
func runCodexGoalWithReply(scanner *jsonlLineScanner, q *GoalRequest, r *codexGoalRuntime, steer *codexSteerer, respond codexServerRequestResponder, logger *slog.Logger, send func(ChatEvent) bool, replyStart func() (int64, error), questions ...*codexQuestionState) *codexStreamResult {
	combined := &codexStreamResult{}
	activated := false
	defer func() {
		if replyStart != nil && !activated {
			// Never recover autonomous work when reply acceptance/reactivation
			// failed or its outcome is unknown. A fresh user reply can retry.
			if err := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) {
				b.DesiredPaused = true
				b.ActivationPending = false
				b.RecoveryPending = false
			}); err != nil {
				combined.processError += "; persist failed reply pause: " + err.Error()
			}
		}
	}()

	var qs *codexQuestionState
	if len(questions) > 0 {
		qs = questions[0]
	}
	current := &codexStreamResult{questions: qs}
	phases := map[string]string{}
	// Slack control responses belong to each native turn, not the accumulated
	// goal transcript. Filter before events leave this peer and before absorb.
	slackTurn := isSlackConversationKey(r.agentID, r.key)
	text := newGoalTurnText(slackTurn, false, send)
	absorbCurrent := func() {
		text.finish(current)
		combined.absorb(current)
	}
	method, params := goalRPC(q, r.threadID)
	var startID, replyID int64
	var err error
	if replyStart != nil {
		replyID, err = replyStart()
	} else {
		r.mu.Lock()
		if r.stopRequested {
			r.mu.Unlock()
			combined.processError = "goal stopped before activation"
			return combined
		}
		startID, err = r.write(method, params)
		r.mu.Unlock()
	}
	if err != nil {
		combined.processError = err.Error()
		return combined
	}
	r.mu.Lock()
	r.ready = replyStart == nil
	r.mu.Unlock()
	var goal *CodexGoal
	defer func() { combined.fullText.WriteString("\n\n" + goalSummary(goal)) }()
	inTurn := false
	currentTurnID := ""
	var checkID int64
	var handoffPauseID int64
	handoffPaused := false
	canFinish := func() bool {
		if !activated || inTurn || checkID != 0 || goal != nil && goal.Status == "active" {
			return false
		}
		if r.wantsHandoff() && goal != nil && goal.Status == "paused" {
			if !handoffPaused || handoffPauseID != 0 {
				return false
			}
			r.checkpointHandoff()
		}
		return true
	}
	publish := func(g *CodexGoal) bool {
		if r.wantsHandoff() && g != nil && g.Status != "active" && g.Status != "paused" {
			_ = updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) { cancelGoalHandoff(b, "native goal ended or became blocked before handoff") })
		}
		goal = g
		if err := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) { b.State = g }); err != nil {
			combined.processError = "persist goal checkpoint: " + err.Error()
			return false
		}
		return send(ChatEvent{Type: "goal", Goal: g})
	}
	for scanner.Scan() {
		var msg rpcMessage
		if json.Unmarshal([]byte(scanner.Text()), &msg) != nil {
			continue
		}
		if handled, err := handleCodexServerRequest(&msg, respond, logger); err != nil {
			combined.processError = err.Error()
			break
		} else if handled {
			continue
		}
		if id, ok := msg.numericID(); ok {
			if handoffPauseID != 0 && id == handoffPauseID {
				handoffPauseID = 0
				if msg.Error != nil {
					combined.processError = "handoff pause: " + msg.Error.Message
					return combined
				}
				handoffPaused = true
				if !publish(decodeGoal(msg.Result)) {
					return combined
				}
				if inTurn && currentTurnID != "" {
					_, _ = r.write("turn/interrupt", map[string]any{"threadId": r.threadID, "turnId": currentTurnID})
				} else {
					checkID, err = r.write("thread/goal/get", map[string]any{"threadId": r.threadID})
					if err != nil {
						combined.processError = err.Error()
						return combined
					}
				}
				continue
			}
			handled := r.resolve(&msg)
			if !handled && steer != nil {
				steer.resolve(id, msg.Error)
			}
			if replyID != 0 && id == replyID {
				if msg.Error != nil {
					combined.processError = "goal reply rejected: " + msg.Error.Message
					return combined
				}
				replyID = 0
				r.mu.Lock()
				if r.stopRequested {
					r.mu.Unlock()
					combined.processError = "goal stopped before reply reactivation"
					return combined
				}
				startID, err = r.write(method, params)
				r.ready = err == nil
				r.mu.Unlock()
				if err != nil {
					combined.processError = err.Error()
					return combined
				}
				continue
			}
			if (startID != 0 && id == startID) || (checkID != 0 && id == checkID) {
				if id == checkID {
					checkID = 0
				}
				if msg.Error != nil {
					combined.processError = msg.Error.Message
					break
				}
				if id == startID {
					if err := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) {
						b.ActivationPending = false
						if q.ExpectedHandoffID != "" && b.Handoff != nil && b.Handoff.ID == q.ExpectedHandoffID && b.Handoff.Phase == "resuming" {
							b.Handoff.Phase = "resumed"
						}
						b.RecoveryAttempts = 0
						rememberGoalOperation(b, q.OperationID)
					}); err != nil {
						combined.processError = err.Error()
						break
					}
				}
				if !publish(decodeGoal(msg.Result)) {
					break
				}
				if id == startID {
					activated = true
					if q.ExpectedHandoffID != "" {
						notice := "Goal resumed after device transfer. Operation: " + q.ExpectedHandoffID + "\n\n"
						combined.fullText.WriteString(notice)
						send(ChatEvent{Type: "text", Delta: notice})
					}
				}
				if r.handoffReadyToPark() && !handoffPaused && handoffPauseID == 0 && goal != nil && goal.Status == "active" {
					handoffPauseID, err = r.write("thread/goal/set", map[string]any{"threadId": r.threadID, "status": "paused"})
					if err != nil {
						combined.processError = err.Error()
						return combined
					}
				}
				if canFinish() {
					combined.turnCompleted = true
					return combined
				}
			}
			continue
		}
		switch msg.Method {
		case "thread/goal/updated":
			if !activated {
				continue
			}
			if !publish(decodeGoal(msg.Params)) {
				absorbCurrent()
				return combined
			}
			if goal != nil && goal.Status == "paused" && inTurn && currentTurnID != "" {
				_, _ = r.write("turn/interrupt", map[string]any{"threadId": r.threadID, "turnId": currentTurnID})
			}
			// A goal may complete during a turn. Drain the final response before
			// releasing the runner, attachment ownership, and Slack stream.
			if canFinish() {
				combined.turnCompleted = true
				return combined
			}
		case "thread/goal/cleared":
			if !activated {
				continue
			}
			publish(nil)
			if inTurn && currentTurnID != "" {
				_, _ = r.write("turn/interrupt", map[string]any{"threadId": r.threadID, "turnId": currentTurnID})
			}
			if !inTurn {
				combined.turnCompleted = true
				return combined
			}
		case "turn/started":
			r.nativeTurnStarted()
			inTurn = true
			currentTurnID = decodeCodexTurnID(msg.Params)
			if handoffPaused && currentTurnID != "" {
				_, _ = r.write("turn/interrupt", map[string]any{"threadId": r.threadID, "turnId": currentTurnID})
			}
			if steer != nil {
				steer.setTurnID(decodeCodexTurnID(msg.Params))
			}
		case "turn/completed":
			current.handleNotification(&msg, phases, logger, text.send)
			if current.turnStatus == "interrupted" && (goal == nil || goal.Status == "paused") {
				current.processError = ""
			}
			if current.processError == "" && current.turnStatus == "completed" {
				if err := updateGoalBinding(r.agentID, r.key, func(b *GoalBinding) { b.RuntimeFailures = 0 }); err != nil {
					combined.processError = err.Error()
					return combined
				}
			}
			absorbCurrent()
			current = &codexStreamResult{questions: qs}
			text = newGoalTurnText(slackTurn, combined.fullText.Len() > 0, send)
			phases = map[string]string{}
			inTurn = false
			if steer != nil {
				steer.finishTurn()
			}
			if combined.processError != "" || combined.cancelled {
				return combined
			}

			// The curl requesting a move has now finished inside a completed
			// native turn. Only here may parking interrupt autonomous work.
			r.nativeTurnCompleted()
			checkID, err = r.write("thread/goal/get", map[string]any{"threadId": r.threadID})
			if err != nil {
				combined.processError = err.Error()
				return combined
			}
		default:
			if current.handleNotification(&msg, phases, logger, text.send) && current.cancelled {
				absorbCurrent()
				return combined
			}
		}
	}
	absorbCurrent()
	if combined.processError == "" {
		combined.processError = "Codex goal stream ended before goal stopped"
		if err := scanner.Err(); err != nil {
			combined.processError = codexReadErrorMessage(err)
		}
	}
	return combined
}
