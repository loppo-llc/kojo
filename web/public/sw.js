self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data.json();
  } catch {
    // fallback
  }

  let title = "kojo";
  let body = "";
  let tag = "kojo";
  let navData = {};
  let renotify = false;

  if (data.type === "agent_chat_done" && data.groupId) {
    // Thread reply: separate tag per thread so it neither replaces nor is
    // replaced by the agent's main-chat notification; clicking opens the
    // thread itself.
    title = data.threadName
      ? `${data.name || "Agent"} · ${data.threadName}`
      : data.name || "Agent";
    body = data.preview || "Response ready";
    tag = `kojo-thread-${data.groupId}`;
    navData = { agentId: data.agentId, groupId: data.groupId };
  } else if (data.type === "agent_chat_done") {
    title = data.name || "Agent";
    body = data.preview || "Response ready";
    tag = `kojo-agent-${data.agentId}`;
    navData = { agentId: data.agentId };
  } else if (data.type === "agent_awaiting_input") {
    title = `回答待ち: ${data.name || "Agent"}`;
    body = "エージェントが質問への回答を待っています";
    tag = `kojo-agent-${data.agentId}`;
    navData = { agentId: data.agentId };
    // A second question raised on the same agent while an earlier
    // notification is still sitting unread would otherwise be silently
    // folded into the same tag with no re-alert.
    renotify = true;
  } else if (data.type === "agent_attention") {
    title = `呼び出し: ${data.name || "Agent"}`;
    body = data.reason || "エージェントがあなたを呼んでいます";
    tag = `kojo-agent-${data.agentId}`;
    // Every raise is a deliberate, distinct page — folding a second one
    // into the first notification silently would defeat the point.
    navData = { agentId: data.agentId };
    renotify = true;
  }

  event.waitUntil(
    self.registration.showNotification(title, {
      body,
      tag,
      data: navData,
      renotify,
    })
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const { agentId, groupId } = event.notification.data || {};
  const url = groupId
    ? `/groupdms/${encodeURIComponent(groupId)}`
    : agentId
      ? `/agents/${agentId}`
      : "/";
  event.waitUntil(
    clients.matchAll({ type: "window" }).then((windowClients) => {
      for (const client of windowClients) {
        if ("focus" in client) {
          client.navigate(url);
          return client.focus();
        }
      }
      return clients.openWindow(url);
    })
  );
});
