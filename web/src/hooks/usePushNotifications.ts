import { useEffect, useState, useCallback } from "react";
import { api } from "../lib/api";

type PushState = "unsupported" | "default" | "granted" | "denied";

function urlBase64ToUint8Array(base64String: string): Uint8Array<ArrayBuffer> {
  const padding = "=".repeat((4 - (base64String.length % 4)) % 4);
  const base64 = (base64String + padding).replace(/-/g, "+").replace(/_/g, "/");
  const rawData = atob(base64);
  const buf = new ArrayBuffer(rawData.length);
  const view = new Uint8Array(buf);
  for (let i = 0; i < rawData.length; i++) {
    view[i] = rawData.charCodeAt(i);
  }
  return view;
}

export function usePushNotifications() {
  const [state, setState] = useState<PushState>("unsupported");
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!("serviceWorker" in navigator) || !("PushManager" in window)) {
      setState("unsupported");
      return;
    }
    const perm = Notification.permission as PushState;
    setState(perm);

    // auto-resubscribe if already granted (handles server restart)
    if (perm === "granted") {
      resubscribe();
    }
  }, []);

  const resubscribe = async () => {
    try {
      const registration = await navigator.serviceWorker.register("/sw.js");
      await navigator.serviceWorker.ready;
      let existing = await registration.pushManager.getSubscription();
      if (!existing) {
        // subscription lost (browser cleared it or expired) - recreate
        const vapidKey = await api.push.vapidKey();
        const applicationServerKey = urlBase64ToUint8Array(vapidKey);
        existing = await registration.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey,
        });
      }
      await api.push.subscribe(existing.toJSON());
    } catch {
      // silent - best effort
    }
  };

  const subscribe = useCallback(async () => {
    if (state === "unsupported" || state === "denied") return;
    setLoading(true);
    try {
      const registration = await navigator.serviceWorker.register("/sw.js");
      await navigator.serviceWorker.ready;

      const vapidKey = await api.push.vapidKey();
      const applicationServerKey = urlBase64ToUint8Array(vapidKey);

      const subscription = await registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey,
      });

      await api.push.subscribe(subscription.toJSON());
      setState("granted");
    } catch (err) {
      console.error("Push subscription failed:", err);
      if (Notification.permission === "denied") {
        setState("denied");
      }
    } finally {
      setLoading(false);
    }
  }, [state]);

  return { state, loading, subscribe };
}
