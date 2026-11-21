import { Button, Checkbox, Input, Select, Switch, cn } from "@braedonsaunders/appkit-ui";
import { EyeOff, Plus, Search, Trash2 } from "lucide-react";
import { useMemo, useState } from "react";
import { Field, NumberInput, Segmented } from "../../components/fields";
import { localized, t } from "../../i18n";
import { apiFetch } from "../../lib/api";
import { useAsync } from "../../lib/hooks";
import type { NotificationsData } from "../notifications/types";
import { ConditionBuilder } from "./ConditionBuilder";
import type { Catalogue } from "./shared";
import { TemplateField } from "./TemplateField";
import type {
  ConditionGroup,
  EventSchema,
  FlowNode,
  ParamSchema,
  PickerFieldGroup,
  SwitchCase,
} from "./types";
import { actionGroupLabel, describeCron, domainLabel } from "./types";

const DURATION_PRESETS = localized(() => [
  { value: "5m", label: t("5 minutes") },
  { value: "15m", label: t("15 minutes") },
  { value: "30m", label: t("30 minutes") },
  { value: "1h", label: t("1 hour") },
  { value: "6h", label: t("6 hours") },
  { value: "24h", label: t("24 hours") },
  { value: "7d", label: t("7 days") },
]);

function DurationField({ value, onChange, label, hint }: { value: string; onChange: (v: string) => void; label: string; hint?: string }) {
  return (
    <Field label={label} hint={hint}>
      <div className="space-y-1.5">
        <div className="flex flex-wrap gap-1">
          {DURATION_PRESETS.map((p) => (
            <button
              key={p.value}
              type="button"
              aria-pressed={value === p.value}
              onClick={() => onChange(p.value)}
              className={cn(
                "rounded-full border px-2 py-0.5 text-xs transition-colors",
                value === p.value ? "border-primary bg-primary-subtle text-primary" : "border-border text-fg-muted hover:text-fg",
              )}
            >
              {p.label}
            </button>
          ))}
        </div>
        <Input aria-label={label} value={value} placeholder={t("or type one: 45s, 20m, 3h, 2d")} onChange={(e) => onChange(e.target.value)} className="w-44 font-mono text-[13px]" />
      </div>
    </Field>
  );
}

export function EventPicker({
  events,
  selected,
  onChange,
  label,
}: {
  events: EventSchema[];
  selected: string[];
  onChange: (next: string[]) => void;
  label: string;
}) {
  const [query, setQuery] = useState("");
  const byDomain = useMemo(() => {
    const q = query.trim().toLowerCase();
    const map = new Map<string, EventSchema[]>();
    for (const ev of events) {
      if (q && !ev.label.toLowerCase().includes(q) && !ev.type.includes(q)) continue;
      const list = map.get(ev.domain) ?? [];
      list.push(ev);
      map.set(ev.domain, list);
    }
    return [...map.entries()];
  }, [events, query]);
  const toggle = (type: string, on: boolean) => onChange(on ? [...selected, type] : selected.filter((x) => x !== type));
  return (
    <Field label={label}>
      <div className="overflow-hidden rounded-lg border border-border">
        <label className="flex items-center gap-2 border-b border-border px-2.5 py-2">
          <Search size={14} className="shrink-0 text-fg-subtle" />
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={t("Search events…")}
            aria-label={t("Search events")}
            className="min-w-0 flex-1 bg-transparent text-sm text-fg outline-none placeholder:text-fg-subtle"
          />
          <span className="text-xs text-fg-subtle">{t("{count} chosen", { count: selected.length })}</span>
        </label>
        <div className="max-h-72 overflow-y-auto">
          {byDomain.map(([domain, evs]) => (
            <div key={domain} className="py-1">
              <p className="px-3 py-1 text-[11px] font-semibold uppercase tracking-wide text-fg-subtle">{domainLabel(domain)}</p>
              {evs.map((ev) => (
                <label key={ev.type} className="flex cursor-pointer items-start gap-2.5 px-3 py-1.5 hover:bg-surface-hover">
                  <Checkbox
                    className="mt-0.5"
                    checked={selected.includes(ev.type)}
                    onChange={(e) => toggle(ev.type, e.target.checked)}
                    aria-label={ev.label}
                  />
                  <span className="min-w-0">
                    <span className="block text-sm text-fg">{ev.label}{ev.critical ? <span className="ml-1.5 text-xs font-medium text-danger">{t("critical")}</span> : null}</span>
                    <span className="block truncate text-xs text-fg-subtle">{ev.description ?? ev.type}</span>
                  </span>
                </label>
              ))}
            </div>
          ))}
          {byDomain.length === 0 ? <p className="px-3 py-4 text-sm text-fg-muted">{t("No events match.")}</p> : null}
        </div>
      </div>
    </Field>
  );
}

function ParamEditor({
  param,
  value,
  onChange,
  fieldGroups,
  sample,
  channels,
}: {
  param: ParamSchema;
  value: unknown;
  onChange: (v: unknown) => void;
  fieldGroups: PickerFieldGroup[];
  sample?: Record<string, unknown>;
  channels: { id: string; name: string }[];
}) {
  const label = param.label + (param.required ? " *" : "");
  if (param.picker === "notifyChannels") {
    const selected = new Set((Array.isArray(value) ? value : []).map(String));
    return (
      <Field label={label} hint={channels.length === 0 ? t("No channels yet — add one under Notifications, or leave empty to use all enabled channels.") : t("Leave empty to use all enabled channels.")}>
        <div className="flex flex-wrap gap-1">
          {channels.map((c) => (
            <button
              key={c.id}
              type="button"
              aria-pressed={selected.has(c.id)}
              onClick={() => {
                const next = new Set(selected);
                if (next.has(c.id)) next.delete(c.id); else next.add(c.id);
                onChange([...next]);
              }}
              className={cn(
                "rounded-full border px-2.5 py-1 text-xs transition-colors",
                selected.has(c.id) ? "border-primary bg-primary-subtle text-primary" : "border-border text-fg-muted hover:text-fg",
              )}
            >
              {c.name || c.id}
            </button>
          ))}
        </div>
      </Field>
    );
  }
  if (param.picker === "libraries") {
    return (
      <Field label={label}>
        <Select aria-label={param.label} value={String(value ?? "")} onChange={(e) => onChange(e.target.value)}>
          <option value="">{t("Choose…")}</option>
          <option value="1">{t("Movies")}</option>
          <option value="2">{t("TV Shows")}</option>
        </Select>
      </Field>
    );
  }
  if (param.type === "bool") {
    return (
      <label className="flex items-center justify-between gap-3 py-1">
        <span className="text-sm text-fg">{param.label}</span>
        <Switch aria-label={param.label} checked={value === true} onChange={(e) => onChange(e.target.checked)} />
      </label>
    );
  }
  if (param.type === "enum" && param.enum) {
    return (
      <Field label={label}>
        <Select aria-label={param.label} value={String(value ?? param.default ?? "")} onChange={(e) => onChange(e.target.value)}>
          <option value="">{t("Choose…")}</option>
          {param.enum.map((o) => (
            <option key={o.value} value={o.value}>{o.label ?? o.value}</option>
          ))}
        </Select>
      </Field>
    );
  }
  if (param.type === "int" || param.type === "float") {
    return (
      <Field label={label}>
        <span className="flex items-center gap-1.5">
          <NumberInput aria-label={param.label} value={Number(value ?? 0)} onChange={(n) => onChange(param.type === "int" ? Math.round(n) : n)} step={param.type === "int" ? 1 : "any"} className="w-32" />
          {param.unit ? <span className="text-xs text-fg-subtle">{param.unit}</span> : null}
        </span>
      </Field>
    );
  }
  const text = typeof value === "string" ? value : value === undefined || value === null ? "" : String(value);
  if (param.template) {
    return (
      <Field label={label} hint={param.description}>
        <TemplateField value={text} onChange={onChange} fieldGroups={fieldGroups} sample={sample} multiline={param.key === "message" || param.key === "body" || text.length > 60} ariaLabel={param.label} />
      </Field>
    );
  }
  return (
    <Field label={label} hint={param.description}>
      <Input aria-label={param.label} value={text} onChange={(e) => onChange(e.target.value)} />
    </Field>
  );
}

const SCHEDULE_PRESETS = localized(() => [
  { cron: "0 * * * *", label: t("Every hour") },
  { cron: "0 9 * * *", label: t("Daily at 09:00") },
  { cron: "0 17 * * 5", label: t("Fridays at 17:00") },
  { cron: "0 8 * * 1", label: t("Mondays at 08:00") },
  { cron: "0 6 1 * *", label: t("First of the month at 06:00") },
]);

/** Configuration form for the selected canvas node. */
export function NodeInspector({
  node,
  onChange,
  catalogue,
  fieldGroups,
  sample,
}: {
  node: FlowNode;
  onChange: (next: FlowNode) => void;
  catalogue: Catalogue;
  fieldGroups: PickerFieldGroup[];
  sample?: Record<string, unknown>;
}) {
  const cfg = (node.config ?? {}) as Record<string, unknown>;
  const setCfg = (patch: Record<string, unknown>) => onChange({ ...node, config: { ...cfg, ...patch } });
  const notices = useAsync(() => apiFetch<NotificationsData>("/notifications/").catch(() => null), []);
  const channels = (notices.data?.channels ?? []).map((c) => ({ id: c.id, name: c.name || c.kind }));

  return (
    <div className="space-y-4">
      <Field label={t("Step name")}>
        <Input aria-label={t("Step name")} value={node.label ?? ""} placeholder={t("Name this step")} onChange={(e) => onChange({ ...node, label: e.target.value })} />
      </Field>
      <label className="flex items-center justify-between gap-3">
        <span className="flex items-center gap-2 text-sm text-fg"><EyeOff size={14} className="text-fg-subtle" /> {t("Skip this step (disabled)")}</span>
        <Switch aria-label={t("Skip this step")} checked={Boolean(node.disabled)} onChange={(e) => onChange({ ...node, disabled: e.target.checked || undefined })} />
      </label>

      {node.kind === "trigger.event" ? (
        <>
          <EventPicker label={t("Run this flow when…")} events={catalogue.events} selected={(cfg.events as string[] | undefined) ?? []} onChange={(events) => setCfg({ events })} />
          <div>
            <p className="mb-1.5 text-sm font-medium text-fg">{t("…but only when (optional pre-filter)")}</p>
            <ConditionBuilder value={(cfg.filter as ConditionGroup | undefined) ?? { op: "and", rules: [] }} fieldGroups={fieldGroups} onChange={(filter) => setCfg({ filter })} />
          </div>
        </>
      ) : null}

      {node.kind === "trigger.schedule" ? (
        <>
          <Segmented
            ariaLabel={t("Schedule kind")}
            value={cfg.cron ? "times" : "interval"}
            options={[
              { value: "interval" as const, label: t("Repeat interval") },
              { value: "times" as const, label: t("Specific times") },
            ]}
            onChange={(mode) => setCfg(mode === "interval" ? { every: String(cfg.every ?? "1h"), cron: undefined } : { cron: String(cfg.cron ?? "0 9 * * *"), every: undefined })}
          />
          {cfg.cron ? (
            <Field label={t("When")} hint={describeCron(String(cfg.cron))}>
              <div className="space-y-1.5">
                <div className="flex flex-wrap gap-1">
                  {SCHEDULE_PRESETS.map((p) => (
                    <button key={p.cron} type="button" aria-pressed={cfg.cron === p.cron} onClick={() => setCfg({ cron: p.cron })}
                      className={cn("rounded-full border px-2 py-0.5 text-xs transition-colors", cfg.cron === p.cron ? "border-primary bg-primary-subtle text-primary" : "border-border text-fg-muted hover:text-fg")}>
                      {p.label}
                    </button>
                  ))}
                </div>
                <Input aria-label={t("Cron expression")} value={String(cfg.cron)} onChange={(e) => setCfg({ cron: e.target.value })} className="w-44 font-mono text-[13px]" />
                <p className="text-xs text-fg-subtle">{t("Five fields: minute hour day month weekday.")}</p>
              </div>
            </Field>
          ) : (
            <DurationField label={t("Run every")} value={String(cfg.every ?? "1h")} onChange={(every) => setCfg({ every })} />
          )}
          <Field label={t("Time zone")} hint={t("IANA name, e.g. America/New_York. Blank uses the server's.")}>
            <Input aria-label={t("Time zone")} value={String(cfg.timezone ?? "")} placeholder="UTC" onChange={(e) => setCfg({ timezone: e.target.value })} className="w-52" />
          </Field>
        </>
      ) : null}

      {node.kind === "trigger.webhook" ? (
        <>
          <p className="text-sm text-fg-muted">{t("Other tools can start this flow by calling its private URL. The URL is shown after you save; rotate it from the flow's settings if it ever leaks.")}</p>
          <label className="flex items-center justify-between gap-3">
            <span className="text-sm text-fg">{t("Require the secret token")}</span>
            <Switch aria-label={t("Require the secret token")} checked={cfg.requireAuth !== false} onChange={(e) => setCfg({ requireAuth: e.target.checked })} />
          </label>
          <Field label={t("Secret header name")}>
            <Input aria-label={t("Secret header name")} value={String(cfg.secretHeader ?? "")} placeholder="X-Ether-Flow-Token" onChange={(e) => setCfg({ secretHeader: e.target.value })} className="font-mono text-[13px]" />
          </Field>
        </>
      ) : null}

      {node.kind === "trigger.manual" ? (
        <>
          <Field label={t("Sample event (for the field pickers)")}>
            <Select aria-label={t("Sample event")} value={String(cfg.sampleEventType ?? "playback.session.started")} onChange={(e) => setCfg({ sampleEventType: e.target.value })}>
              {catalogue.events.map((ev) => (
                <option key={ev.type} value={ev.type}>{ev.label}</option>
              ))}
            </Select>
          </Field>
          <p className="text-sm text-fg-muted">{t("Manual flows run from the “Run now” button on the Flows page.")}</p>
        </>
      ) : null}

      {node.kind === "condition" ? (
        <ConditionBuilder value={(cfg.condition as ConditionGroup | undefined) ?? { op: "and", rules: [] }} fieldGroups={fieldGroups} onChange={(condition) => setCfg({ condition })} />
      ) : null}

      {node.kind === "logic.switch" ? (
        <SwitchCasesEditor cfg={cfg} setCfg={setCfg} fieldGroups={fieldGroups} />
      ) : null}

      {node.kind === "logic.delay" ? (
        <DurationField label={t("Wait for")} value={String(cfg.duration ?? "")} onChange={(duration) => setCfg({ duration })} hint={t("The run sleeps durably — restarts don't lose it.")} />
      ) : null}

      {node.kind === "logic.wait_event" ? (
        <>
          <EventPicker label={t("Wait for this to happen…")} events={catalogue.events} selected={(cfg.eventTypes as string[] | undefined) ?? []} onChange={(eventTypes) => setCfg({ eventTypes })} />
          <label className="flex items-center justify-between gap-3">
            <span className="text-sm text-fg">{t("Same session / title / viewer only")}</span>
            <Switch aria-label={t("Match the same session")} checked={cfg.matchCorrelation !== false} onChange={(e) => setCfg({ matchCorrelation: e.target.checked })} />
          </label>
          <DurationField label={t("Give up after")} value={String(cfg.timeout ?? "15m")} onChange={(timeout) => setCfg({ timeout })} hint={t("On timeout the run follows the “Timed out” connection.")} />
        </>
      ) : null}

      {node.kind === "logic.window" ? (
        <>
          <Field label={t("Count per")} hint={t("Usually the session, so each stream is counted on its own.")}>
            <TemplateField value={String(cfg.keyTemplate ?? "")} onChange={(keyTemplate) => setCfg({ keyTemplate })} fieldGroups={fieldGroups} sample={sample} ariaLabel={t("Count per")} />
          </Field>
          <Field label={t("This many times")}>
            <NumberInput aria-label={t("Threshold")} value={Number(cfg.threshold ?? 3)} min={1} step={1} onChange={(n) => setCfg({ threshold: Math.max(1, Math.round(n)) })} className="w-24" />
          </Field>
          <DurationField label={t("within")} value={String(cfg.window ?? "2m")} onChange={(window) => setCfg({ window })} />
          <label className="flex items-center justify-between gap-3">
            <span className="text-sm text-fg">{t("Start counting again once reached")}</span>
            <Switch aria-label={t("Reset after reaching")} checked={cfg.resetOnReach !== false} onChange={(e) => setCfg({ resetOnReach: e.target.checked })} />
          </label>
        </>
      ) : null}

      {node.kind === "logic.dedupe" ? (
        <>
          <Field label={t("Treat as duplicates when this matches")}>
            <TemplateField value={String(cfg.keyTemplate ?? "")} onChange={(keyTemplate) => setCfg({ keyTemplate })} fieldGroups={fieldGroups} sample={sample} ariaLabel={t("Duplicate key")} />
          </Field>
          <Field label={t("Allow at most")}>
            <span className="flex items-center gap-2 text-sm text-fg-muted">
              <NumberInput aria-label={t("Max per cooldown")} value={Number(cfg.maxPerWindow ?? 1)} min={1} step={1} onChange={(n) => setCfg({ maxPerWindow: Math.max(1, Math.round(n)) })} className="w-24" />
              {t("per cooldown")}
            </span>
          </Field>
          <DurationField label={t("Cooldown")} value={String(cfg.cooldown ?? "30m")} onChange={(cooldown) => setCfg({ cooldown })} />
        </>
      ) : null}

      {node.kind === "action" ? (
        <ActionEditor cfg={cfg} setCfg={setCfg} catalogue={catalogue} fieldGroups={fieldGroups} sample={sample} channels={channels} />
      ) : null}
    </div>
  );
}

function SwitchCasesEditor({ cfg, setCfg, fieldGroups }: { cfg: Record<string, unknown>; setCfg: (patch: Record<string, unknown>) => void; fieldGroups: PickerFieldGroup[] }) {
  const cases = (cfg.cases as SwitchCase[] | undefined) ?? [];
  const setCases = (next: SwitchCase[]) => setCfg({ cases: next });
  return (
    <div className="space-y-3">
      <label className="flex items-center justify-between gap-3">
        <span className="text-sm text-fg">{t("Follow every matching branch")}</span>
        <Switch aria-label={t("Match all branches")} checked={Boolean(cfg.matchAll)} onChange={(e) => setCfg({ matchAll: e.target.checked })} />
      </label>
      {cases.map((c, i) => (
        <div key={c.key} className="space-y-2 rounded-lg border border-border p-3">
          <div className="flex items-center gap-2">
            <Input aria-label={t("Branch name")} value={c.label ?? c.key} onChange={(e) => setCases(cases.map((x, j) => (j === i ? { ...x, label: e.target.value } : x)))} className="flex-1" />
            <Button type="button" variant="ghost" size="sm" aria-label={t("Remove branch")} onClick={() => setCases(cases.filter((_, j) => j !== i))}>
              <Trash2 size={14} />
            </Button>
          </div>
          <ConditionBuilder value={c.condition} fieldGroups={fieldGroups} onChange={(condition) => setCases(cases.map((x, j) => (j === i ? { ...x, condition } : x)))} />
        </div>
      ))}
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => {
          let n = cases.length + 1;
          while (cases.some((c) => c.key === `case_${n}`)) n += 1;
          setCases([...cases, { key: `case_${n}`, label: t("Case {n}", { n }), condition: { op: "and", rules: [] } }]);
        }}
      >
        <Plus size={13} /> {t("Add branch")}
      </Button>
      <p className="text-xs text-fg-subtle">{t("Anything else follows the Default connection.")}</p>
    </div>
  );
}

function ActionEditor({
  cfg,
  setCfg,
  catalogue,
  fieldGroups,
  sample,
  channels,
}: {
  cfg: Record<string, unknown>;
  setCfg: (patch: Record<string, unknown>) => void;
  catalogue: Catalogue;
  fieldGroups: PickerFieldGroup[];
  sample?: Record<string, unknown>;
  channels: { id: string; name: string }[];
}) {
  const actionType = String(cfg.actionType ?? "");
  const spec = catalogue.actionsByType.get(actionType);
  const params = (cfg.params ?? {}) as Record<string, unknown>;
  const groups = useMemo(() => {
    const map = new Map<string, typeof catalogue.actions>();
    for (const a of catalogue.actions) {
      const list = map.get(a.group) ?? [];
      list.push(a);
      map.set(a.group, list);
    }
    return [...map.entries()];
  }, [catalogue.actions]);
  return (
    <div className="space-y-4">
      <Field label={t("Do this")}>
        <Select
          aria-label={t("Action")}
          value={actionType}
          onChange={(e) => {
            const next = catalogue.actionsByType.get(e.target.value);
            const defaults: Record<string, unknown> = {};
            for (const p of next?.params ?? []) if (p.default !== undefined) defaults[p.key] = p.default;
            setCfg({ actionType: e.target.value, params: { ...defaults, ...params } });
          }}
        >
          <option value="">{t("Choose an action…")}</option>
          {groups.map(([group, actions]) => (
            <optgroup key={group} label={actionGroupLabel(group)}>
              {actions.map((a) => (
                <option key={a.type} value={a.type}>{a.label}</option>
              ))}
            </optgroup>
          ))}
        </Select>
      </Field>
      {spec?.description ? <p className="-mt-2 text-sm text-fg-muted">{spec.description}</p> : null}
      {(spec?.params ?? []).map((param) => (
        <ParamEditor
          key={param.key}
          param={param}
          value={params[param.key]}
          channels={channels}
          fieldGroups={fieldGroups}
          sample={sample}
          onChange={(v) => setCfg({ params: { ...params, [param.key]: v } })}
        />
      ))}
      <label className="flex items-center justify-between gap-3 border-t border-border pt-3">
        <span className="text-sm text-fg">{t("Keep going if this step fails")}</span>
        <Switch aria-label={t("Continue on error")} checked={Boolean(cfg.continueOnError)} onChange={(e) => setCfg({ continueOnError: e.target.checked || undefined })} />
      </label>
    </div>
  );
}
