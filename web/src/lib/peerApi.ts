// Peer registry API client.
//
// Mirrors the wire shapes in internal/server/peer_handlers.go. The
// server's wire contract is camelCase; we re-export the same names
// so call sites can read like the API spec without an adapter layer.
//
// Auth: GET /api/v1/peers (list) is open to Owner and Agent
// principals on the server; every other endpoint here is owner-
// only. The shared httpClient already attaches the Owner Bearer
// when present, so the caller doesn't have to think about auth
// headers.

import { get, post, del, patch } from "./httpClient";

export interface PeerInfo {
  deviceId: string;
  name: string;
  url?: string;
  lastSeen?: number; // unix millis
  status: "online" | "offline" | "degraded";
  isSelf: boolean;
}

export interface PeerListResponse {
  items: PeerInfo[];
  selfDeviceId?: string;
}

export interface PeerRegisterRequest {
  deviceId: string;
  name: string;
  url: string;
}

// PeerPendingInfo mirrors one row of `peer_pending` — a peer that
// auto-discovered the Hub and POSTed a join-request, awaiting Owner
// Approve from the Settings UI. See docs/peer-onboarding-plan.md.
export interface PeerPendingInfo {
  deviceId: string;
  name: string;
  url: string;
  firstSeen: number;
  lastSeen: number;
}

export interface PeerPendingListResponse {
  items: PeerPendingInfo[];
}

export const peersApi = {
  list: () => get<PeerListResponse>("/api/v1/peers"),
  self: () => get<PeerInfo>("/api/v1/peers/self"),
  register: (req: PeerRegisterRequest) => post<PeerInfo>("/api/v1/peers", req),
  // Pending join requests (docs/peer-onboarding-plan.md).
  pending: () => get<PeerPendingListResponse>("/api/v1/peers/pending"),
  approvePending: (deviceId: string) =>
    post<PeerInfo>(
      `/api/v1/peers/pending/${encodeURIComponent(deviceId)}/approve`,
      {},
    ),
  rejectPending: (deviceId: string) =>
    post<void>(
      `/api/v1/peers/pending/${encodeURIComponent(deviceId)}/reject`,
      {},
    ),
  // updateMetadata patches name + url only — the only operator-
  // editable fields on the wire. status / last_seen are server-
  // owned. Narrow patch shape so the inline Edit form can't
  // accidentally clobber fields another surface owns.
  updateMetadata: (deviceId: string, req: { name: string; url: string }) =>
    patch<PeerInfo>(`/api/v1/peers/${encodeURIComponent(deviceId)}`, req),
  remove: (deviceId: string) => del<void>(`/api/v1/peers/${encodeURIComponent(deviceId)}`),
};
