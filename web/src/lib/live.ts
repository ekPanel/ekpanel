import { useEffect, useState } from "react";
import type { Metrics } from "./api";

const MAX_POINTS = 600; // matches the panel's in-memory history (about 20 minutes)

export interface LiveState {
  /** The browser's own connection to the panel is open. */
  connected: boolean;
  /** The server's agent is connected to the panel. */
  online: boolean;
  history: Metrics[];
}

type Message =
  | { type: "snapshot"; online: boolean; history: Metrics[] | null }
  | { type: "status"; online: boolean }
  | { type: "metrics"; online: boolean; metrics: Metrics };

/** Streams a server's metrics over a WebSocket, reconnecting with backoff. */
export function useServerLive(serverId: string): LiveState {
  const [state, setState] = useState<LiveState>({ connected: false, online: false, history: [] });

  useEffect(() => {
    let ws: WebSocket | null = null;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let delay = 1000;
    let stopped = false;

    const connect = () => {
      const proto = location.protocol === "https:" ? "wss" : "ws";
      ws = new WebSocket(`${proto}://${location.host}/api/v1/servers/${encodeURIComponent(serverId)}/live`);
      ws.onopen = () => {
        delay = 1000;
        setState((s) => ({ ...s, connected: true }));
      };
      ws.onmessage = (ev) => {
        const msg = JSON.parse(ev.data as string) as Message;
        setState((s) => {
          switch (msg.type) {
            case "snapshot":
              return { connected: true, online: msg.online, history: msg.history ?? [] };
            case "status":
              return { ...s, online: msg.online };
            case "metrics": {
              const history = s.history.length >= MAX_POINTS ? s.history.slice(1) : s.history.slice();
              history.push(msg.metrics);
              return { ...s, online: msg.online, history };
            }
          }
        });
      };
      ws.onclose = () => {
        setState((s) => ({ ...s, connected: false }));
        if (stopped) return;
        retry = setTimeout(connect, delay);
        delay = Math.min(delay * 2, 15000);
      };
    };
    connect();

    return () => {
      stopped = true;
      clearTimeout(retry);
      ws?.close();
    };
  }, [serverId]);

  return state;
}
