self.addEventListener("push", (event) => {
  let data = { type: "notification", tool: "kojo", workDir: "" };
  try {
    data = event.data.json();
  } catch {
    // fallback
  }

  let title = "kojo";
  let body = "Session event";
  let tag = "kojo-session";

  if (data.type === "session_exit") {
    title = `${data.tool} exited`;
    const code = data.exitCode !== undefined && data.exitCode !== null ? data.exitCode : "?";
    body = `Exit code: ${code}\n${data.workDir}`;
    tag = `kojo-exit-${data.sessionId}`;
  }

  event.waitUntil(
    self.registration.showNotification(title, {
      body,
      tag,
      data: { sessionId: data.sessionId },
    })
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const sessionId = event.notification.data?.sessionId;
  const url = sessionId ? `/session/${sessionId}` : "/";
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
