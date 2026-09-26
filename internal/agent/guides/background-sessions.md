# Background sessions (run_in_background in threads)

In a thread conversation — a WebUI group-DM thread or a Slack thread — a Bash /
Agent tool call started with `run_in_background` keeps running after your
reply. kojo keeps that thread's CLI process alive ("lingering") until the
tasks finish; when one completes, its notification starts a new turn in the
SAME thread and your reply is posted there. A message someone sends to the
thread meanwhile goes to the same process; sent while such a notification turn
runs, it is steered into that turn and answered in its reply.

Stopping: when a user stops your turn (Slack `!stop`, the WebUI stop button),
only the current turn ends — your background tasks keep running and still
report back in the thread. Slack `!stop all` also stops every background task
of the thread; you are told on your next turn there. To stop tasks yourself, use
the API below.

Limits:
- At most 50 threads per agent can linger with tasks pending at once. A normal
  turn is never blocked, but if a turn ends with tasks still running while all
  50 slots are taken, that thread cannot keep them: its process is stopped, the
  tasks are lost, and a notice is posted to the thread (you are told too on the
  next turn there). When the turn-start note says the limit is reached, avoid
  `run_in_background` or stop threads you no longer need first.
- A lingering thread is stopped anyway after a long idle maximum; tasks still
  running then are reported as abandoned in the thread.

At the start of a thread turn kojo may add a `[kojo]` note: how many OTHER
threads still run background tasks, or that tasks of THIS thread ended before
completing (re-run them if still needed).

List your threads with background tasks:

```bash
curl {CURL_FLAGS} '{API_BASE}/api/v1/agents/{AGENT_ID}/background-sessions'
```

Response: `{"sessions":[{"sessionKey","surface":"webui_thread|slack|other","threadId","threadName","state":"lingering|turn|closing","lingeringSince","pendingCount","tasks":[{"id","type","description","startedAt","elapsedSeconds"}]}],"lingering":N,"cap":50}`.
`startedAt` is when the task was started (the CLI's task start event).

Stop every background task of one thread (a stop notice is posted to that
thread). URL-encode the `sessionKey` (it contains `:`):

```bash
curl {CURL_FLAGS} -X DELETE '{API_BASE}/api/v1/agents/{AGENT_ID}/background-sessions/SESSION_KEY'
```

Stop a single task (a stop notice is posted to the thread):

```bash
curl {CURL_FLAGS} -X DELETE '{API_BASE}/api/v1/agents/{AGENT_ID}/background-sessions/SESSION_KEY/tasks/TASK_ID'
```

404 means the thread has no background session / no such pending task.
