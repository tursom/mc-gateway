export type Role = "admin" | "member" | "guest";

export interface User {
  username: string;
  role: Role;
  disabled: boolean;
}

export interface RouteRecord {
  host: string;
  upstream: string;
  enabled: boolean;
  note?: string;
}

export interface ServiceRecord {
  name: string;
  enabled: boolean;
  port: number;
  options?: Record<string, unknown>;
  restart_required?: boolean;
}

export interface Metrics {
  total_connections?: number;
  active_connections?: number;
  tcp_connections?: number;
  websocket_connections?: number;
  route_misses?: number;
  upstream_dial_errors?: number;
  route_hits?: Record<string, number>;
}

export interface AuditLog {
  created_at: number;
  actor: string;
  action: string;
  target_type: string;
  target_id: string;
  success: boolean;
  message?: string;
}

export interface SetupStatus {
  required: boolean;
}

export interface LoginResponse {
  token: string;
  user: User;
}

export interface RuntimeConfig {
  apiPrefix: string;
}
