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

import { get, post, del } from "./httpClient";

export interface PeerInfo {
  deviceId: string;
  name: string;
  publicKey: string;
  capabilities?: string;
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
  publicKey: string;
  capabilities?: string;
}

export interface PeerRotateKeyResponse {
  peer: PeerInfo;
  previousPublicKey: string;
}

export const peersApi = {
  list: () => get<PeerListResponse>("/api/v1/peers"),
  self: () => get<PeerInfo>("/api/v1/peers/self"),
  register: (req: PeerRegisterRequest) => post<PeerInfo>("/api/v1/peers", req),
  remove: (deviceId: string) => del<void>(`/api/v1/peers/${encodeURIComponent(deviceId)}`),
  rotateKey: (deviceId: string, publicKey: string) =>
    post<PeerRotateKeyResponse>(
      `/api/v1/peers/${encodeURIComponent(deviceId)}/rotate-key`,
      { publicKey },
    ),
};
