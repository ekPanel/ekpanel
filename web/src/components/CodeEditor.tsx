import { useEffect, useRef } from "react";
import { EditorView, basicSetup } from "codemirror";
import { EditorState, type Extension } from "@codemirror/state";
import { keymap } from "@codemirror/view";
import { php } from "@codemirror/lang-php";
import { html } from "@codemirror/lang-html";
import { css } from "@codemirror/lang-css";
import { javascript } from "@codemirror/lang-javascript";
import { json } from "@codemirror/lang-json";

function languageFor(name: string): Extension[] {
  const ext = name.toLowerCase().split(".").pop() ?? "";
  switch (ext) {
    case "php":
    case "phtml":
      return [php()];
    case "html":
    case "htm":
    case "svg":
    case "xml":
      return [html()];
    case "css":
    case "scss":
      return [css()];
    case "js":
    case "mjs":
    case "cjs":
    case "jsx":
      return [javascript({ jsx: true })];
    case "ts":
    case "tsx":
      return [javascript({ typescript: true, jsx: ext === "tsx" })];
    case "json":
      return [json()];
    default:
      return [];
  }
}

// Colors come from the panel's theme tokens, so the editor follows light and dark mode.
const theme = EditorView.theme({
  "&": { height: "100%", fontSize: "13px", backgroundColor: "var(--surface)", color: "var(--ink)" },
  ".cm-content": { fontFamily: "var(--font-mono)", caretColor: "var(--accent)" },
  ".cm-gutters": { backgroundColor: "var(--sunk)", color: "var(--muted)", borderInlineEnd: "1px solid var(--line)" },
  ".cm-activeLine, .cm-activeLineGutter": { backgroundColor: "color-mix(in srgb, var(--accent-soft) 45%, transparent)" },
  "&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection": { backgroundColor: "var(--accent-soft) !important" },
  "&.cm-focused": { outline: "none" },
});

/** CodeMirror editor. Loaded on demand; Ctrl/⌘+S calls onSave. */
export default function CodeEditor({ name, value, onChange, onSave }: { name: string; value: string; onChange: (v: string) => void; onSave: () => void }) {
  const host = useRef<HTMLDivElement>(null);
  const handlers = useRef({ onChange, onSave });
  handlers.current = { onChange, onSave };

  useEffect(() => {
    const view = new EditorView({
      parent: host.current!,
      state: EditorState.create({
        doc: value,
        extensions: [
          basicSetup,
          theme,
          ...languageFor(name),
          EditorView.lineWrapping,
          keymap.of([{ key: "Mod-s", preventDefault: true, run: () => (handlers.current.onSave(), true) }]),
          EditorView.updateListener.of((u) => u.docChanged && handlers.current.onChange(u.state.doc.toString())),
        ],
      }),
    });
    view.focus();
    return () => view.destroy();
    // The editor is created once per file; later value changes come from typing.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [name]);

  return <div ref={host} dir="ltr" className="h-full overflow-hidden rounded-lg border border-line" />;
}
