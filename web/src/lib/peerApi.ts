// Peer registry API client (Phase G slice 2/3).
//
// Mirrors the wire shapes in internal/server/peer_handlers.go. The
// server's wire contract is camelCase; we re-export the same names
// so call sites can read like the API spec without an adapter layer.
//
// Auth: every endpoint is Owner-only on the server. The shared
// httpClient already attaches the Owner Bearer when present, so the
// caller doesn't have to think about auth headers.
//
// Capabilities is a JSON string (the server stores it verbatim);
// callers that want a typed object should JSON.parse() at the call
// site. Empty string = "no caps reported" (distinct from "{}" =
// "empty caps object").

import { get, post, del, patch } from "./httpClient";

export interface PeerInfo {
  deviceId: string;
  name: string;
  url?: string;
  publicKey: string;
  capabilities?: string;
  lastSeen?: number; // unix millis
  status: "online" | "offline" | "degraded";
  isSelf: boolean;
  // trusted gates the privileged cross-peer surface on this host
  // (sessions, ws, info, dirs, files, git, upload). Untrusted
  // peers can still pair but only the inter-peer endpoints
  // (events, blobs, agent-sync, register-push) admit them.
  trusted: boolean;
}

export interface PeerListResponse {
  items: PeerInfo[];
  selfDeviceId?: string;
}

export interface PeerRegisterRequest {
  deviceId: string;
  name: string;
  url: string;
  publicKey: string;
  capabilities?: string;
  trusted?: boolean;
}

export interface PeerRotateKeyResponse {
  peer: PeerInfo;
  previousPublicKey: string;
}

// PeerPendingInfo mirrors one row of `peer_pending` — a peer that
// auto-discovered the Hub and POSTed a join-request, awaiting Owner
// Approve from the Settings UI. See docs/peer-onboarding-plan.md.
export interface PeerPendingInfo {
  deviceId: string;
  name: string;
  url: string;
  publicKey: string;
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
  // updateMetadata patches name + url only. publicKey rotates
  // through rotateKey; trusted flips through setTrust;
  // capabilities is owned by the peer's own self-report. Narrow
  // patch shape so the inline Edit form can't accidentally
  // clobber fields another surface owns.
  updateMetadata: (deviceId: string, req: { name: string; url: string }) =>
    patch<PeerInfo>(`/api/v1/peers/${encodeURIComponent(deviceId)}`, req),
  remove: (deviceId: string) => del<void>(`/api/v1/peers/${encodeURIComponent(deviceId)}`),
  rotateKey: (deviceId: string, publicKey: string) =>
    post<PeerRotateKeyResponse>(
      `/api/v1/peers/${encodeURIComponent(deviceId)}/rotate-key`,
      { publicKey },
    ),
  setTrust: (deviceId: string, trusted: boolean) =>
    patch<PeerInfo>(`/api/v1/peers/${encodeURIComponent(deviceId)}/trust`, { trusted }),
};
