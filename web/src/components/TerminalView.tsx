import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";

export type TerminalState = { kind: "connecting" } | { kind: "open" } | { kind: "closed"; reason: string };

/**
 * A shell in the browser. Keystrokes go to the panel as binary WebSocket messages and output
 * comes back the same way; size changes go as {"type":"resize"} text messages. Loaded on
 * demand, like the code editor.
 */
export default function TerminalView({ path, attempt, onState }: { path: string; attempt: number; onState: (s: TerminalState) => void }) {
  const box = useRef<HTMLDivElement>(null);
  const report = useRef(onState);
  report.current = onState;

  useEffect(() => {
    const term = new Terminal({
      cursorBlink: true,
      fontFamily: '"JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, monospace',
      fontSize: 14,
      scrollback: 5000,
      // The brand's terminal colours: tile black, screen white, cursor green.
      theme: { background: "#0f1513", foreground: "#eaf2ee", cursor: "#36d27e", cursorAccent: "#0f1513", selectionBackground: "#28513b" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(box.current!);
    fit.fit();
    term.focus();

    const url = new URL(path, window.location.href);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    url.searchParams.set("cols", String(term.cols));
    url.searchParams.set("rows", String(term.rows));
    const ws = new WebSocket(url);
    ws.binaryType = "arraybuffer";
    const encoder = new TextEncoder();
    let ended = false;
    report.current({ kind: "connecting" });

    ws.onopen = () => report.current({ kind: "open" });
    ws.onmessage = (e) => {
      if (e.data instanceof ArrayBuffer) {
        term.write(new Uint8Array(e.data));
        return;
      }
      try {
        const msg = JSON.parse(e.data as string) as { type: string; reason?: string };
        if (msg.type === "exit") {
          ended = true;
          report.current({ kind: "closed", reason: msg.reason ?? "" });
        }
      } catch {
        // not a control message
      }
    };
    ws.onclose = () => {
      if (!ended) report.current({ kind: "closed", reason: "" });
    };
    const input = term.onData((d) => ws.readyState === WebSocket.OPEN && ws.send(encoder.encode(d)));
    const resize = term.onResize(({ cols, rows }) => ws.readyState === WebSocket.OPEN && ws.send(JSON.stringify({ type: "resize", cols, rows })));
    const observer = new ResizeObserver(() => fit.fit());
    observer.observe(box.current!);

    return () => {
      observer.disconnect();
      input.dispose();
      resize.dispose();
      ws.close();
      term.dispose();
    };
  }, [path, attempt]);

  return <div ref={box} className="h-full w-full" dir="ltr" />;
}
