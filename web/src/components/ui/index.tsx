import { useEffect, useId, useRef, useState, type ButtonHTMLAttributes, type InputHTMLAttributes, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { AlertCircle, Check, Copy, Loader2, X } from "lucide-react";

const cx = (...c: (string | false | null | undefined)[]) => c.filter(Boolean).join(" ");

// ---- Button ----

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "primary" | "secondary" | "ghost" | "danger";
  size?: "sm" | "md";
  loading?: boolean;
};

export function Button({ variant = "secondary", size = "md", loading, className, children, disabled, ...rest }: ButtonProps) {
  return (
    <button
      {...rest}
      disabled={disabled || loading}
      className={cx(
        "inline-flex items-center justify-center gap-2 rounded-lg font-medium whitespace-nowrap transition-colors disabled:opacity-60 disabled:cursor-not-allowed",
        size === "sm" ? "h-8 px-3 text-sm" : "h-10 px-4",
        variant === "primary" && "bg-accent text-accent-ink hover:brightness-110",
        variant === "secondary" && "border border-line bg-surface text-ink hover:bg-sunk",
        variant === "ghost" && "text-ink hover:bg-sunk",
        variant === "danger" && "border border-line bg-surface text-crit hover:bg-crit-soft",
        className,
      )}
    >
      {loading && <Loader2 className="size-4 animate-spin" aria-hidden />}
      {children}
    </button>
  );
}

// ---- Form fields ----

type FieldProps = InputHTMLAttributes<HTMLInputElement> & { label: string; hint?: ReactNode; error?: string };

export function Field({ label, hint, error, id, className, ...rest }: FieldProps) {
  const auto = useId();
  const fieldId = id ?? auto;
  return (
    // One column of at most the space given: an input's default width must not widen it.
    <div className={cx("grid min-w-0 grid-cols-1 gap-1.5", className)}>
      <label htmlFor={fieldId} className="text-sm font-medium">
        {label}
      </label>
      <input
        id={fieldId}
        aria-invalid={error ? true : undefined}
        aria-describedby={hint || error ? fieldId + "-desc" : undefined}
        className="h-10 w-full min-w-0 rounded-lg border border-line bg-surface px-3 text-ink placeholder:text-muted focus:border-accent focus:outline-none aria-[invalid]:border-crit"
        {...rest}
      />
      {(error || hint) && (
        <p id={fieldId + "-desc"} className={cx("text-sm", error ? "text-crit" : "text-muted")}>
          {error || hint}
        </p>
      )}
    </div>
  );
}

// ---- Surfaces and status ----

export function Card({ className, children }: { className?: string; children: ReactNode }) {
  return <div className={cx("rounded-xl border border-line bg-surface", className)}>{children}</div>;
}

export type Tone = "ok" | "warn" | "crit" | "neutral" | "accent";

const toneClasses: Record<Tone, string> = {
  ok: "bg-ok-soft text-ok",
  warn: "bg-warn-soft text-warn",
  crit: "bg-crit-soft text-crit",
  neutral: "bg-sunk text-muted",
  accent: "bg-accent-soft text-accent",
};

export function Badge({ tone = "neutral", children }: { tone?: Tone; children: ReactNode }) {
  return (
    <span className={cx("inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-semibold whitespace-nowrap", toneClasses[tone])}>
      {children}
    </span>
  );
}

const dotClasses: Record<Tone, string> = { ok: "bg-ok", warn: "bg-warn", crit: "bg-crit", neutral: "bg-muted", accent: "bg-accent" };

export function StatusDot({ tone, pulse }: { tone: Tone; pulse?: boolean }) {
  return (
    <span className="relative inline-flex size-2.5 shrink-0" aria-hidden>
      {pulse && <span className={cx("absolute inset-0 animate-ping rounded-full opacity-60", dotClasses[tone])} />}
      <span className={cx("relative inline-flex size-2.5 rounded-full", dotClasses[tone])} />
    </span>
  );
}

export function ErrorAlert({ children }: { children: ReactNode }) {
  if (!children) return null;
  return (
    <div role="alert" className="flex items-start gap-2 rounded-lg bg-crit-soft px-3 py-2.5 text-sm text-crit">
      <AlertCircle className="mt-0.5 size-4 shrink-0" aria-hidden />
      <span>{children}</span>
    </div>
  );
}

export function Spinner({ label }: { label?: string }) {
  return (
    <div className="flex items-center justify-center gap-2 p-8 text-muted" role="status">
      <Loader2 className="size-5 animate-spin" aria-hidden />
      {label && <span>{label}</span>}
    </div>
  );
}

/** A horizontal usage bar. Turns amber at 75% and red at 90%. */
export function Meter({ percent, label }: { percent: number; label: string }) {
  const p = Math.max(0, Math.min(100, percent));
  const color = p >= 90 ? "bg-crit" : p >= 75 ? "bg-warn" : "bg-accent";
  return (
    <div className="h-1.5 overflow-hidden rounded-full bg-sunk" role="meter" aria-label={label} aria-valuenow={Math.round(p)} aria-valuemin={0} aria-valuemax={100}>
      <div className={cx("h-full rounded-full transition-[width] duration-500", color)} style={{ width: `${p}%` }} />
    </div>
  );
}

// ---- Copy to clipboard ----

export function CopyButton({ text, className }: { text: string; className?: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const id = setTimeout(() => setCopied(false), 1500);
    return () => clearTimeout(id);
  }, [copied]);
  return (
    <Button
      size="sm"
      variant="ghost"
      className={className}
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          setCopied(true);
        } catch {
          // clipboard blocked (e.g. plain HTTP); the text is still selectable
        }
      }}
      aria-label={t("common.copy")}
    >
      {copied ? <Check className="size-4 text-ok" aria-hidden /> : <Copy className="size-4" aria-hidden />}
      {copied ? t("common.copied") : t("common.copy")}
    </Button>
  );
}

// ---- Dialog (native <dialog>: focus trap, Esc and backdrop come for free) ----

export function Dialog({ open, onClose, title, children, wide }: { open: boolean; onClose: () => void; title: string; children: ReactNode; wide?: boolean }) {
  const ref = useRef<HTMLDialogElement>(null);
  const { t } = useTranslation();
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) {
      d.showModal();
      // showModal focuses the first focusable element, which is the close button. Start in
      // the first field instead, so typing a space cannot close the dialog by accident.
      d.querySelector<HTMLElement>("input, select, textarea")?.focus();
    }
    if (!open && d.open) d.close();
  }, [open]);
  return (
    <dialog
      ref={ref}
      onClose={onClose}
      onClick={(e) => e.target === ref.current && onClose()}
      className={cx(
        "m-auto w-[calc(100%-2rem)] rounded-2xl border border-line bg-surface p-0 text-ink shadow-2xl backdrop:bg-black/40",
        wide ? "max-w-2xl" : "max-w-md",
      )}
    >
      {open && (
        <div className="grid gap-5 p-6">
          <div className="flex items-start justify-between gap-4">
            <h2 className="text-lg font-semibold">{title}</h2>
            <button onClick={onClose} className="-m-1 rounded-md p-1 text-muted hover:bg-sunk" aria-label={t("common.close")}>
              <X className="size-5" aria-hidden />
            </button>
          </div>
          {children}
        </div>
      )}
    </dialog>
  );
}

export { cx };
